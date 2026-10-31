package legacy

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Reference data and platform settings: legacy countries, counties,
// banks, bank_branches and the single settings row.

var CountryCols = []string{"id", "legacy_id", "name", "code", "currency_code", "currency_name",
	"calling_code", "is_default", "is_active", "created_at", "updated_at"}

var (
	reCountry  = regexp.MustCompile(`^[A-Z]{2}$`)
	reCurrency = regexp.MustCompile(`^[A-Z]{3}$`)
	reCalling  = regexp.MustCompile(`^[1-9][0-9]{0,3}$`)
)

// countries returns the imported ids and the default country's id.
func (p *Plan) countries(rows []Row, now time.Time) (ids map[int64]bool) {
	ids = map[int64]bool{}
	codes := map[string]bool{}
	haveDefault := false
	for _, r := range byID(rows) {
		id := r.Int("id")
		code := strings.ToUpper(clean(r.Str("code")))
		cur := strings.ToUpper(clean(r.Str("currency_code")))
		calling := strings.TrimPrefix(clean(r.Str("calling_code")), "+")
		switch {
		case !reCountry.MatchString(code) || codes[code]:
			p.Report.issue("countries: missing, invalid or duplicate code, skipped")
			continue
		case !reCurrency.MatchString(cur) || !reCalling.MatchString(calling):
			p.Report.issue("countries: invalid currency or calling code, skipped")
			continue
		}
		codes[code] = true
		def := r.Flag("default_country") && !haveDefault
		haveDefault = haveDefault || def
		created := timeOr(epoch(r.Str("created_on")), now)
		p.Countries = append(p.Countries, []any{id, id, clean(r.Str("name")), code, cur, clean(r.Str("currency")),
			calling, def, r.Flag("active") || def, created, timeOr(epoch(r.Str("modified_on")), created)})
		ids[id] = true
	}
	return ids
}

var CountyCols = []string{"id", "legacy_id", "country_id", "name", "code", "capital", "is_active", "created_at", "updated_at"}

// counties attaches counties with no country to the default country:
// every legacy county row is Kenyan and has country_id NULL.
func (p *Plan) counties(rows []Row, countries map[int64]bool, now time.Time) {
	var def int64
	for _, c := range p.Countries {
		if c[7].(bool) {
			def = c[0].(int64)
		}
	}
	names := map[string]bool{}
	for _, r := range byID(rows) {
		id := r.Int("id")
		country := r.Int("country_id")
		if country == 0 {
			country = def
		}
		name := clean(r.Str("name"))
		key := fmt.Sprint(country, "/", strings.ToLower(name))
		switch {
		case !countries[country]:
			p.Report.issue("counties: country missing, skipped")
			continue
		case name == "" || names[key]:
			p.Report.issue("counties: blank or duplicate name, skipped")
			continue
		}
		names[key] = true
		var code *int32
		if n, err := strconv.ParseInt(clean(r.Str("code")), 10, 32); err == nil {
			v := int32(n)
			code = &v
		}
		active := r.Ptr("active") == nil || r.Flag("active")
		created := timeOr(epoch(r.Str("created_on")), now)
		p.Counties = append(p.Counties, []any{id, id, country, name, code, clean(r.Str("capital")), active,
			created, timeOr(epoch(r.Str("modified_on")), created)})
	}
}

var BankCols = []string{"id", "legacy_id", "name", "slug", "code", "short_code", "logo", "branding", "settings",
	"is_partner", "is_default", "is_active", "created_at", "updated_at"}

var bankColumnsUsed = set("id", "name", "slug", "partner", "logo", "primary_color", "secondary_color",
	"tertiary_color", "text_color", "active", "created_by", "created_on", "modified_on", "modified_by",
	"default_bank", "bank_code", "old_id", "bank_short_code")

var reSlug = regexp.MustCompile(`[^a-z0-9]+`)

func (p *Plan) banks(rows []Row, now time.Time) map[int64]bool {
	ids := map[int64]bool{}
	slugs := map[string]bool{}
	for _, r := range byID(rows) {
		id := r.Int("id")
		name := clean(r.Str("name"))
		if name == "" {
			p.Report.issue("banks: blank name, skipped")
			continue
		}
		slug := strings.Trim(reSlug.ReplaceAllString(strings.ToLower(clean(r.Str("slug"))), "-"), "-")
		if slug == "" || slugs[slug] {
			slug = fmt.Sprintf("bank-%d", id)
			p.Report.issue("banks: missing or duplicate slug, regenerated")
		}
		slugs[slug] = true
		var code *int32
		if n, err := strconv.ParseInt(clean(r.Str("bank_code")), 10, 32); err == nil && n > 0 {
			v := int32(n)
			code = &v
		}
		branding := map[string]string{}
		for _, k := range []string{"primary_color", "secondary_color", "tertiary_color", "text_color"} {
			if v := clean(r.Str(k)); v != "" {
				branding[strings.TrimSuffix(k, "_color")] = v
			}
		}
		b, _ := json.Marshal(branding)
		created := timeOr(epoch(r.Str("created_on")), now)
		p.Banks = append(p.Banks, []any{id, id, name, slug, code, clean(r.Str("bank_short_code")), clean(r.Str("logo")),
			b, extras(r, bankColumnsUsed), r.Flag("partner"), r.Flag("default_bank"), r.Flag("active"),
			created, timeOr(epoch(r.Str("modified_on")), created)})
		ids[id] = true
	}
	return ids
}

var BankBranchCols = []string{"id", "legacy_id", "bank_id", "name", "code", "is_active", "created_at", "updated_at"}

func (p *Plan) bankBranches(rows []Row, banks map[int64]bool, now time.Time) map[int64]bool {
	ids := map[int64]bool{}
	for _, r := range byID(rows) {
		id := r.Int("id")
		name := clean(r.Str("name"))
		switch {
		case !banks[r.Int("bank_id")]:
			p.Report.issue("bank_branches: bank missing, skipped")
			continue
		case name == "":
			p.Report.issue("bank_branches: blank name, skipped")
			continue
		}
		var code *int32
		if n, err := strconv.ParseInt(clean(r.Str("code")), 10, 32); err == nil {
			v := int32(n)
			code = &v
		}
		created := timeOr(epoch(r.Str("created_on")), now)
		p.BankBranches = append(p.BankBranches, []any{id, id, r.Int("bank_id"), name, code,
			r.Ptr("active") == nil || r.Flag("active"), created, timeOr(epoch(r.Str("modified_on")), created)})
		ids[id] = true
	}
	return ids
}

var SettingCols = []string{"key", "value"}

var settingsSkipped = set("id", "active", "created_by", "created_on", "modified_on", "modified_by", "old_id")

// settings turns the legacy single-row settings table into key/value
// pairs. Integers become JSON numbers; everything else stays a string.
func (p *Plan) settings(rows []Row) {
	if len(rows) == 0 {
		return
	}
	if len(rows) > 1 {
		p.Report.issue("settings: %d rows, used the active one with the highest id", len(rows))
	}
	r := byID(rows)[len(rows)-1]
	for _, row := range byID(rows) {
		if row.Flag("active") {
			r = row
		}
	}
	keys := make([]string, 0, len(r))
	for k := range r {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		v := r[k]
		if settingsSkipped[k] || v == nil {
			continue
		}
		s := clean(*v)
		if s == "" {
			continue
		}
		var b []byte
		if n, err := strconv.ParseInt(s, 10, 64); err == nil {
			b, _ = json.Marshal(n)
		} else {
			b, _ = json.Marshal(s)
		}
		p.Settings = append(p.Settings, []any{strings.ToLower(k), b})
	}
}

var MobileTokenCols = []string{"token_hash", "legacy_id", "user_id", "created_at", "expires_at"}

// mobileTokens carries over the mobile app's live sessions, so the app
// stays signed in through the switch. Only the token's SHA-256 is kept;
// PINs are not imported (a fresh one is sent on the next PIN sign-in).
func (p *Plan) mobileTokens(rows []Row, users map[int64]bool, now time.Time) {
	seen := map[string]bool{}
	for _, r := range byID(rows) {
		token := strings.Trim(clean(r.Str("access_token")), `"`)
		expires := epoch(r.Str("access_token_expire_on"))
		switch {
		case token == "" || expires == nil || !expires.After(now):
			continue // signed out or expired: nothing to carry over
		case !users[r.Int("user_id")]:
			p.Report.issue("user_pin_access_token: user missing, skipped")
			continue
		case seen[token]:
			p.Report.issue("user_pin_access_token: duplicate token, skipped")
			continue
		}
		seen[token] = true
		h := sha256.Sum256([]byte(token))
		p.MobileTokens = append(p.MobileTokens, []any{h[:], r.Int("id"), r.Int("user_id"),
			timeOr(epoch(r.Str("access_token_created_on")), now), *expires})
	}
}
