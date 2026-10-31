package httpapi

import (
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// Per-group income, expense and fine categories, and outside depositors.
// Lists show active, visible rows unless ?all=true.

type categoryResponse struct {
	ID               int64   `json:"id"`
	Name             string  `json:"name"`
	Description      string  `json:"description"`
	Amount           *string `json:"amount,omitempty"` // fine categories: the default fine
	IsAdministrative *bool   `json:"is_administrative,omitempty"`
	IsHidden         bool    `json:"is_hidden"`
	IsActive         bool    `json:"is_active"`
}

type categoryRequest struct {
	Name        *string `json:"name"`
	Description *string `json:"description"`
	Amount      *Amount `json:"amount"`
	IsHidden    *bool   `json:"is_hidden"`
	IsActive    *bool   `json:"is_active"`
}

// clean trims the request and reports a validation problem.
func (c *categoryRequest) clean(create, allowAmount bool) string {
	if c.Name != nil {
		n := strings.TrimSpace(*c.Name)
		c.Name = &n
	}
	c.Description = trimmedPtr(c.Description)
	switch {
	case create && (c.Name == nil || *c.Name == ""):
		return "Name is required."
	case c.Name != nil && *c.Name == "":
		return "Name cannot be blank."
	case c.Amount != nil && !allowAmount:
		return "Only fine categories have an amount."
	case create && (c.IsHidden != nil || c.IsActive != nil):
		return "New categories start active and visible."
	}
	return ""
}

// categoryKind binds the generic handlers to one table's queries.
type categoryKind struct {
	notFound string
	amount   bool
	list     func(r *http.Request, groupID int64, all bool) ([]categoryResponse, error)
	create   func(r *http.Request, groupID int64, c categoryRequest) (categoryResponse, error)
	update   func(r *http.Request, groupID, id int64, c categoryRequest) (categoryResponse, error)
}

func (s *Server) incomeCategories() categoryKind {
	to := func(c store.IncomeCategory) categoryResponse {
		return categoryResponse{ID: c.ID, Name: c.Name, Description: c.Description, IsHidden: c.IsHidden, IsActive: c.IsActive}
	}
	return categoryKind{
		notFound: "income_category_not_found",
		list: func(r *http.Request, g int64, all bool) ([]categoryResponse, error) {
			rows, err := s.q.ListIncomeCategories(r.Context(), store.ListIncomeCategoriesParams{GroupID: g, IncludeInactive: all})
			return mapSlice(rows, to), err
		},
		create: func(r *http.Request, g int64, c categoryRequest) (categoryResponse, error) {
			row, err := s.q.CreateIncomeCategory(r.Context(), store.CreateIncomeCategoryParams{GroupID: g, Name: *c.Name, Description: deref(c.Description)})
			return to(row), err
		},
		update: func(r *http.Request, g, id int64, c categoryRequest) (categoryResponse, error) {
			row, err := s.q.UpdateIncomeCategory(r.Context(), store.UpdateIncomeCategoryParams{
				GroupID: g, ID: id, Name: c.Name, Description: c.Description, IsHidden: c.IsHidden, IsActive: c.IsActive,
			})
			return to(row), err
		},
	}
}

func (s *Server) expenseCategories() categoryKind {
	to := func(c store.ExpenseCategory) categoryResponse {
		return categoryResponse{ID: c.ID, Name: c.Name, Description: c.Description, IsAdministrative: &c.IsAdministrative,
			IsHidden: c.IsHidden, IsActive: c.IsActive}
	}
	return categoryKind{
		notFound: "expense_category_not_found",
		list: func(r *http.Request, g int64, all bool) ([]categoryResponse, error) {
			rows, err := s.q.ListExpenseCategories(r.Context(), store.ListExpenseCategoriesParams{GroupID: g, IncludeInactive: all})
			return mapSlice(rows, to), err
		},
		create: func(r *http.Request, g int64, c categoryRequest) (categoryResponse, error) {
			row, err := s.q.CreateExpenseCategory(r.Context(), store.CreateExpenseCategoryParams{GroupID: g, Name: *c.Name, Description: deref(c.Description)})
			return to(row), err
		},
		update: func(r *http.Request, g, id int64, c categoryRequest) (categoryResponse, error) {
			row, err := s.q.UpdateExpenseCategory(r.Context(), store.UpdateExpenseCategoryParams{
				GroupID: g, ID: id, Name: c.Name, Description: c.Description, IsHidden: c.IsHidden, IsActive: c.IsActive,
			})
			return to(row), err
		},
	}
}

func (s *Server) fineCategories() categoryKind {
	to := func(c store.FineCategory) categoryResponse {
		return categoryResponse{ID: c.ID, Name: c.Name, Description: c.Description, Amount: numericMoney(c.Amount),
			IsHidden: c.IsHidden, IsActive: c.IsActive}
	}
	return categoryKind{
		notFound: "fine_category_not_found",
		amount:   true,
		list: func(r *http.Request, g int64, all bool) ([]categoryResponse, error) {
			rows, err := s.q.ListFineCategories(r.Context(), store.ListFineCategoriesParams{GroupID: g, IncludeInactive: all})
			return mapSlice(rows, to), err
		},
		create: func(r *http.Request, g int64, c categoryRequest) (categoryResponse, error) {
			amount := Amount("0")
			if c.Amount != nil {
				amount = *c.Amount
			}
			row, err := s.q.CreateFineCategory(r.Context(), store.CreateFineCategoryParams{
				GroupID: g, Name: *c.Name, Description: deref(c.Description), Amount: amount.Numeric(),
			})
			return to(row), err
		},
		update: func(r *http.Request, g, id int64, c categoryRequest) (categoryResponse, error) {
			p := store.UpdateFineCategoryParams{
				GroupID: g, ID: id, Name: c.Name, Description: c.Description, IsHidden: c.IsHidden, IsActive: c.IsActive,
			}
			if c.Amount != nil {
				p.Amount = c.Amount.Numeric()
			}
			row, err := s.q.UpdateFineCategory(r.Context(), p)
			return to(row), err
		},
	}
}

func mapSlice[T, U any](in []T, f func(T) U) []U {
	out := make([]U, 0, len(in))
	for _, v := range in {
		out = append(out, f(v))
	}
	return out
}

func (s *Server) listCategories(k func() categoryKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := accessFrom(r.Context())
		rows, err := k().list(r, a.Group.ID, a.IsOfficial && includeInactive(r))
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"categories": rows})
	}
}

func (s *Server) createCategory(k func() categoryKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := accessFrom(r.Context())
		kind := k()
		var req categoryRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if msg := req.clean(true, kind.amount); msg != "" {
			writeError(w, http.StatusBadRequest, "invalid_category", msg)
			return
		}
		row, err := kind.create(r, a.Group.ID, req)
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		writeJSON(w, http.StatusCreated, row)
	}
}

func (s *Server) updateCategory(k func() categoryKind) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		a := accessFrom(r.Context())
		kind := k()
		id, ok := pathID(r, "categoryID")
		if !ok {
			writeError(w, http.StatusNotFound, kind.notFound, "Category not found.")
			return
		}
		var req categoryRequest
		if !decodeJSON(w, r, &req) {
			return
		}
		if msg := req.clean(false, kind.amount); msg != "" {
			writeError(w, http.StatusBadRequest, "invalid_category", msg)
			return
		}
		row, err := kind.update(r, a.Group.ID, id, req)
		if errors.Is(err, pgx.ErrNoRows) {
			writeError(w, http.StatusNotFound, kind.notFound, "Category not found.")
			return
		}
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, row)
	}
}

type depositorResponse struct {
	ID          int64   `json:"id"`
	Name        string  `json:"name"`
	Email       *string `json:"email"`
	Phone       *string `json:"phone"`
	Description string  `json:"description"`
	IsActive    bool    `json:"is_active"`
}

func toDepositor(d store.Depositor) depositorResponse {
	return depositorResponse{ID: d.ID, Name: d.Name, Email: d.Email, Phone: d.Phone, Description: d.Description, IsActive: d.IsActive}
}

type depositorRequest struct {
	Name        *string `json:"name"`
	Email       *string `json:"email"`
	Phone       *string `json:"phone"`
	Description *string `json:"description"`
	IsActive    *bool   `json:"is_active"`
}

func (s *Server) listDepositors(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	rows, err := s.q.ListDepositors(r.Context(), store.ListDepositorsParams{GroupID: a.Group.ID, IncludeInactive: includeInactive(r)})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"depositors": mapSlice(rows, toDepositor)})
}

func (s *Server) createDepositor(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	var req depositorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	name := strings.TrimSpace(deref(req.Name))
	if name == "" {
		writeError(w, http.StatusBadRequest, "missing_fields", "Name is required.")
		return
	}
	row, err := s.q.CreateDepositor(r.Context(), store.CreateDepositorParams{
		GroupID: a.Group.ID, Name: name, Email: emptyNil(trimmedPtr(req.Email)), Phone: emptyNil(trimmedPtr(req.Phone)),
		Description: strings.TrimSpace(deref(req.Description)),
	})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusCreated, toDepositor(row))
}

func (s *Server) updateDepositor(w http.ResponseWriter, r *http.Request) {
	a := accessFrom(r.Context())
	id, ok := pathID(r, "depositorID")
	if !ok {
		writeError(w, http.StatusNotFound, "depositor_not_found", "Depositor not found.")
		return
	}
	var req depositorRequest
	if !decodeJSON(w, r, &req) {
		return
	}
	req.Name = trimmedPtr(req.Name)
	if req.Name != nil && *req.Name == "" {
		writeError(w, http.StatusBadRequest, "invalid_name", "Name cannot be blank.")
		return
	}
	row, err := s.q.UpdateDepositor(r.Context(), store.UpdateDepositorParams{
		GroupID: a.Group.ID, ID: id, Name: req.Name, Email: trimmedPtr(req.Email), Phone: trimmedPtr(req.Phone),
		Description: trimmedPtr(req.Description), IsActive: req.IsActive,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		writeError(w, http.StatusNotFound, "depositor_not_found", "Depositor not found.")
		return
	}
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDepositor(row))
}

// Reference lists for opening sacco and mobile money accounts.

func (s *Server) listSaccos(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListSaccos(r.Context(), includeInactive(r))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSlice(rows, func(v store.Sacco) map[string]any {
		return map[string]any{"id": v.ID, "name": v.Name, "slug": v.Slug, "is_active": v.IsActive}
	}))
}

func (s *Server) listSaccoBranches(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r, "saccoID")
	if !ok {
		writeError(w, http.StatusNotFound, "sacco_not_found", "Sacco not found.")
		return
	}
	rows, err := s.q.ListSaccoBranches(r.Context(), store.ListSaccoBranchesParams{SaccoID: id, IncludeInactive: includeInactive(r)})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSlice(rows, func(v store.SaccoBranch) map[string]any {
		return map[string]any{"id": v.ID, "name": v.Name, "is_active": v.IsActive}
	}))
}

func (s *Server) listMobileMoneyProviders(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListMobileMoneyProviders(r.Context(), includeInactive(r))
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, mapSlice(rows, func(v store.MobileMoneyProvider) map[string]any {
		return map[string]any{"id": v.ID, "name": v.Name, "slug": v.Slug, "is_active": v.IsActive}
	}))
}
