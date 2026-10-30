package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/swahome/legion-api/internal/store"
)

// Reference data (countries, counties, banks, branches) is readable by
// any signed-in user for forms, and editable by platform admins under
// /v1/admin. ?all=true on the admin routes includes inactive rows.

func includeInactive(r *http.Request) bool { return r.URL.Query().Get("all") == "true" }

// writeRefError maps reference data write failures to responses.
func writeRefError(w http.ResponseWriter, r *http.Request, err error, notFound string) bool {
	var pgErr *pgconn.PgError
	switch {
	case err == nil:
		return false
	case errors.Is(err, pgx.ErrNoRows):
		writeError(w, http.StatusNotFound, "not_found", notFound)
	case errors.As(err, &pgErr) && pgErr.Code == "23505":
		writeError(w, http.StatusConflict, "duplicate", "Something with that name or code already exists.")
	case errors.As(err, &pgErr) && pgErr.Code == "23503":
		writeError(w, http.StatusBadRequest, "invalid_reference", "The linked record does not exist.")
	case errors.As(err, &pgErr) && pgErr.Code == "23514":
		writeError(w, http.StatusBadRequest, "invalid_value", "One of the values is not valid.")
	default:
		writeInternal(w, r, err)
	}
	return true
}

// ---- countries ----

type countryResponse struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	Code         string `json:"code"`
	CurrencyCode string `json:"currency_code"`
	CurrencyName string `json:"currency_name"`
	CallingCode  string `json:"calling_code"`
	IsDefault    bool   `json:"is_default"`
	IsActive     bool   `json:"is_active"`
}

func toCountry(c store.Country) countryResponse {
	return countryResponse{c.ID, c.Name, c.Code, c.CurrencyCode, c.CurrencyName, c.CallingCode, c.IsDefault, c.IsActive}
}

func (s *Server) listCountries(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListCountries(r.Context(), includeInactive(r))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]countryResponse, 0, len(rows))
	for _, c := range rows {
		out = append(out, toCountry(c))
	}
	writeJSON(w, http.StatusOK, out)
}

type countryRequest struct {
	Name         *string `json:"name"`
	Code         *string `json:"code"`
	CurrencyCode *string `json:"currency_code"`
	CurrencyName *string `json:"currency_name"`
	CallingCode  *string `json:"calling_code"`
	IsActive     *bool   `json:"is_active"`
	IsDefault    *bool   `json:"is_default"`
}

var (
	reCountryCode  = regexp.MustCompile(`^[A-Z]{2}$`)
	reCurrencyCode = regexp.MustCompile(`^[A-Z]{3}$`)
	reCallingCode  = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)
)

// clean normalises the request and reports the first invalid field.
func (c *countryRequest) clean() string {
	up := func(p *string) {
		if p != nil {
			*p = strings.ToUpper(strings.TrimSpace(*p))
		}
	}
	up(c.Code)
	up(c.CurrencyCode)
	if c.CallingCode != nil {
		*c.CallingCode = strings.TrimPrefix(strings.TrimSpace(*c.CallingCode), "+")
	}
	switch {
	case c.Name != nil && strings.TrimSpace(*c.Name) == "":
		return "Name cannot be empty."
	case c.Code != nil && !reCountryCode.MatchString(*c.Code):
		return "Code must be two letters, like KE."
	case c.CurrencyCode != nil && !reCurrencyCode.MatchString(*c.CurrencyCode):
		return "Currency code must be three letters, like KES."
	case c.CallingCode != nil && !reCallingCode.MatchString(*c.CallingCode):
		return "Calling code must be 1 to 4 digits, like 254."
	}
	return ""
}

func (s *Server) createCountry(w http.ResponseWriter, r *http.Request) {
	var req countryRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if msg := req.clean(); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_value", msg)
		return
	}
	if req.Name == nil || req.Code == nil || req.CurrencyCode == nil || req.CallingCode == nil {
		writeError(w, http.StatusBadRequest, "missing_fields", "Name, code, currency code and calling code are required.")
		return
	}
	var c store.Country
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var err error
		c, err = q.CreateCountry(r.Context(), store.CreateCountryParams{
			Name: strings.TrimSpace(*req.Name), Code: *req.Code, CurrencyCode: *req.CurrencyCode,
			CurrencyName: deref(req.CurrencyName), CallingCode: *req.CallingCode, IsActive: req.IsActive == nil || *req.IsActive,
		})
		if err == nil && req.IsDefault != nil && *req.IsDefault {
			err = setDefaultCountry(r, q, c.ID)
			c.IsDefault, c.IsActive = true, true
		}
		return err
	})
	if writeRefError(w, r, err, "Country not found.") {
		return
	}
	s.audit(r, "country.create", "country", c.ID, req)
	writeJSON(w, http.StatusCreated, toCountry(c))
}

func setDefaultCountry(r *http.Request, q *store.Queries, id int64) error {
	if err := q.ClearDefaultCountry(r.Context()); err != nil {
		return err
	}
	return q.SetDefaultCountry(r.Context(), id)
}

func (s *Server) updateCountry(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "countryID")
	var req countryRequest
	if !ok || !decodeJSON(w, r, &req) {
		if !ok {
			writeError(w, http.StatusNotFound, "not_found", "Country not found.")
		}
		return
	}
	if req.Code != nil {
		writeError(w, http.StatusBadRequest, "invalid_value", "A country's code can't be changed.")
		return
	}
	if msg := req.clean(); msg != "" {
		writeError(w, http.StatusBadRequest, "invalid_value", msg)
		return
	}
	var c store.Country
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		var err error
		if req.Name != nil {
			*req.Name = strings.TrimSpace(*req.Name)
		}
		c, err = q.UpdateCountry(r.Context(), store.UpdateCountryParams{
			ID: id, Name: req.Name, CurrencyCode: req.CurrencyCode, CurrencyName: req.CurrencyName,
			CallingCode: req.CallingCode, IsActive: req.IsActive,
		})
		if err != nil {
			return err
		}
		if req.IsDefault != nil && *req.IsDefault {
			if err := setDefaultCountry(r, q, id); err != nil {
				return err
			}
			c.IsDefault, c.IsActive = true, true
		}
		if c.IsDefault && !c.IsActive {
			return errDefaultInactive
		}
		return nil
	})
	if errors.Is(err, errDefaultInactive) {
		writeError(w, http.StatusConflict, "default_country", "The default country can't be deactivated. Make another country the default first.")
		return
	}
	if writeRefError(w, r, err, "Country not found.") {
		return
	}
	s.audit(r, "country.update", "country", id, req)
	writeJSON(w, http.StatusOK, toCountry(c))
}

var errDefaultInactive = errors.New("default country inactive")

// ---- counties ----

type countyResponse struct {
	ID        int64  `json:"id"`
	CountryID int64  `json:"country_id"`
	Name      string `json:"name"`
	Code      *int32 `json:"code"`
	Capital   string `json:"capital"`
	IsActive  bool   `json:"is_active"`
}

func toCounty(c store.County) countyResponse {
	return countyResponse{c.ID, c.CountryID, c.Name, c.Code, c.Capital, c.IsActive}
}

func (s *Server) listCounties(w http.ResponseWriter, r *http.Request) {
	params := store.ListCountiesParams{IncludeInactive: includeInactive(r)}
	if v := queryPtr(r, "country_id"); v != nil {
		id, err := strconv.ParseInt(*v, 10, 64)
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid_country", "country_id must be a number.")
			return
		}
		params.CountryID = &id
	}
	rows, err := s.q.ListCounties(r.Context(), params)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]countyResponse, 0, len(rows))
	for _, c := range rows {
		out = append(out, toCounty(c))
	}
	writeJSON(w, http.StatusOK, out)
}

type countyRequest struct {
	CountryID *int64  `json:"country_id"`
	Name      *string `json:"name"`
	Code      *int32  `json:"code"`
	Capital   *string `json:"capital"`
	IsActive  *bool   `json:"is_active"`
}

func (s *Server) createCounty(w http.ResponseWriter, r *http.Request) {
	var req countyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(deref(req.Name))
	if req.CountryID == nil || name == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "Country and name are required.")
		return
	}
	c, err := s.q.CreateCounty(r.Context(), store.CreateCountyParams{
		CountryID: *req.CountryID, Name: name, Code: req.Code,
		Capital: strings.TrimSpace(deref(req.Capital)), IsActive: req.IsActive == nil || *req.IsActive,
	})
	if writeRefError(w, r, err, "County not found.") {
		return
	}
	s.audit(r, "county.create", "county", c.ID, req)
	writeJSON(w, http.StatusCreated, toCounty(c))
}

func (s *Server) updateCounty(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "countyID")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "County not found.")
		return
	}
	var req countyRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.CountryID != nil {
		writeError(w, http.StatusBadRequest, "invalid_value", "A county can't move to another country.")
		return
	}
	if req.Name != nil {
		if *req.Name = strings.TrimSpace(*req.Name); *req.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid_value", "Name cannot be empty.")
			return
		}
	}
	c, err := s.q.UpdateCounty(r.Context(), store.UpdateCountyParams{
		ID: id, Name: req.Name, Code: req.Code, Capital: req.Capital, IsActive: req.IsActive,
	})
	if writeRefError(w, r, err, "County not found.") {
		return
	}
	s.audit(r, "county.update", "county", id, req)
	writeJSON(w, http.StatusOK, toCounty(c))
}

// ---- banks ----

type bankResponse struct {
	ID          int64           `json:"id"`
	Name        string          `json:"name"`
	Slug        string          `json:"slug"`
	Code        *int32          `json:"code"`
	ShortCode   string          `json:"short_code"`
	Logo        string          `json:"logo"`
	Branding    json.RawMessage `json:"branding"`
	IsPartner   bool            `json:"is_partner"`
	IsDefault   bool            `json:"is_default"`
	IsActive    bool            `json:"is_active"`
	BranchCount *int64          `json:"branch_count,omitempty"`
}

func toBank(b store.Bank) bankResponse {
	return bankResponse{ID: b.ID, Name: b.Name, Slug: b.Slug, Code: b.Code, ShortCode: b.ShortCode,
		Logo: b.Logo, Branding: b.Branding, IsPartner: b.IsPartner, IsDefault: b.IsDefault, IsActive: b.IsActive}
}

func (s *Server) listBanks(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListBanks(r.Context(), store.ListBanksParams{IncludeInactive: includeInactive(r), Search: queryPtr(r, "q")})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]bankResponse, 0, len(rows))
	for _, b := range rows {
		resp := toBank(store.Bank{ID: b.ID, Name: b.Name, Slug: b.Slug, Code: b.Code, ShortCode: b.ShortCode,
			Logo: b.Logo, Branding: b.Branding, IsPartner: b.IsPartner, IsDefault: b.IsDefault, IsActive: b.IsActive})
		n := b.BranchCount
		resp.BranchCount = &n
		out = append(out, resp)
	}
	writeJSON(w, http.StatusOK, out)
}

type bankRequest struct {
	Name      *string `json:"name"`
	Code      *int32  `json:"code"`
	ShortCode *string `json:"short_code"`
	IsPartner *bool   `json:"is_partner"`
	IsActive  *bool   `json:"is_active"`
}

var reNonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slugify(s string) string {
	return strings.Trim(reNonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

func (s *Server) createBank(w http.ResponseWriter, r *http.Request) {
	var req bankRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(deref(req.Name))
	if name == "" || slugify(name) == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "Bank name is required.")
		return
	}
	b, err := s.q.CreateBank(r.Context(), store.CreateBankParams{
		Name: name, Slug: slugify(name), Code: req.Code, ShortCode: strings.TrimSpace(deref(req.ShortCode)),
		IsPartner: req.IsPartner != nil && *req.IsPartner, IsActive: req.IsActive == nil || *req.IsActive,
	})
	if writeRefError(w, r, err, "Bank not found.") {
		return
	}
	s.audit(r, "bank.create", "bank", b.ID, req)
	writeJSON(w, http.StatusCreated, toBank(b))
}

func (s *Server) updateBank(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "bankID")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Bank not found.")
		return
	}
	var req bankRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		if *req.Name = strings.TrimSpace(*req.Name); *req.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid_value", "Name cannot be empty.")
			return
		}
	}
	b, err := s.q.UpdateBank(r.Context(), store.UpdateBankParams{
		ID: id, Name: req.Name, Code: req.Code, ShortCode: req.ShortCode, IsPartner: req.IsPartner, IsActive: req.IsActive,
	})
	if writeRefError(w, r, err, "Bank not found.") {
		return
	}
	s.audit(r, "bank.update", "bank", id, req)
	writeJSON(w, http.StatusOK, toBank(b))
}

// ---- branches ----

type branchResponse struct {
	ID       int64  `json:"id"`
	BankID   int64  `json:"bank_id"`
	Name     string `json:"name"`
	Code     *int32 `json:"code"`
	IsActive bool   `json:"is_active"`
}

func toBranch(b store.BankBranch) branchResponse {
	return branchResponse{b.ID, b.BankID, b.Name, b.Code, b.IsActive}
}

func (s *Server) listBranches(w http.ResponseWriter, r *http.Request) {
	bankID, ok := pathID(r, "bankID")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Bank not found.")
		return
	}
	rows, err := s.q.ListBankBranches(r.Context(), store.ListBankBranchesParams{
		BankID: bankID, IncludeInactive: includeInactive(r), Search: queryPtr(r, "q"),
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make([]branchResponse, 0, len(rows))
	for _, b := range rows {
		out = append(out, toBranch(b))
	}
	writeJSON(w, http.StatusOK, out)
}

type branchRequest struct {
	Name     *string `json:"name"`
	Code     *int32  `json:"code"`
	IsActive *bool   `json:"is_active"`
}

func (s *Server) createBranch(w http.ResponseWriter, r *http.Request) {
	bankID, ok := pathID(r, "bankID")
	if !ok {
		writeError(w, http.StatusNotFound, "not_found", "Bank not found.")
		return
	}
	var req branchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(deref(req.Name))
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "Branch name is required.")
		return
	}
	b, err := s.q.CreateBankBranch(r.Context(), store.CreateBankBranchParams{
		BankID: bankID, Name: name, Code: req.Code, IsActive: req.IsActive == nil || *req.IsActive,
	})
	if writeRefError(w, r, err, "Bank not found.") {
		return
	}
	s.audit(r, "branch.create", "bank_branch", b.ID, req)
	writeJSON(w, http.StatusCreated, toBranch(b))
}

func (s *Server) updateBranch(w http.ResponseWriter, r *http.Request) {
	bankID, ok1 := pathID(r, "bankID")
	id, ok2 := pathID(r, "branchID")
	if !ok1 || !ok2 {
		writeError(w, http.StatusNotFound, "not_found", "Branch not found.")
		return
	}
	var req branchRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Name != nil {
		if *req.Name = strings.TrimSpace(*req.Name); *req.Name == "" {
			writeError(w, http.StatusBadRequest, "invalid_value", "Name cannot be empty.")
			return
		}
	}
	b, err := s.q.UpdateBankBranch(r.Context(), store.UpdateBankBranchParams{
		ID: id, BankID: bankID, Name: req.Name, Code: req.Code, IsActive: req.IsActive,
	})
	if writeRefError(w, r, err, "Branch not found.") {
		return
	}
	s.audit(r, "branch.update", "bank_branch", id, req)
	writeJSON(w, http.StatusOK, toBranch(b))
}

// ---- platform settings ----

var reSettingKey = regexp.MustCompile(`^[a-z0-9_]+(\.[a-z0-9_]+)*$`)

func (s *Server) listSettings(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListPlatformSettings(r.Context())
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	out := make(map[string]json.RawMessage, len(rows))
	for _, row := range rows {
		out[row.Key] = row.Value
	}
	writeJSON(w, http.StatusOK, out)
}

// updateSettings merges the body into the settings: each key is set to
// its value, and a null value removes the key.
func (s *Server) updateSettings(w http.ResponseWriter, r *http.Request) {
	var req map[string]json.RawMessage
	if !decodeJSON(w, r, &req) {
		return
	}
	if len(req) == 0 || len(req) > 200 {
		writeError(w, http.StatusBadRequest, "invalid_body", "Send between 1 and 200 settings.")
		return
	}
	for k := range req {
		if len(k) > 100 || !reSettingKey.MatchString(k) {
			writeError(w, http.StatusBadRequest, "invalid_key", "Setting keys use lowercase letters, digits, _ and dots: "+k)
			return
		}
	}
	uid := claimsFrom(r.Context()).UserID
	err := pgx.BeginFunc(r.Context(), s.pool, func(tx pgx.Tx) error {
		q := s.q.WithTx(tx)
		for k, v := range req {
			var err error
			if string(v) == "null" {
				err = q.DeletePlatformSetting(r.Context(), k)
			} else {
				err = q.UpsertPlatformSetting(r.Context(), store.UpsertPlatformSettingParams{Key: k, Value: v, UpdatedBy: &uid})
			}
			if err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	s.audit(r, "settings.update", "settings", 0, req)
	s.listSettings(w, r)
}
