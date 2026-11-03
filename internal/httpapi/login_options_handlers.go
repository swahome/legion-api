package httpapi

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/jackc/pgx/v5"
)

// loginLinkKeys are the platform settings that hold the links shown on the
// login page. An empty or missing setting hides its link.
var loginLinkKeys = []string{"android_app", "ios_app", "terms", "support", "demo"}

// loginOptions is public: what the login page needs before anyone signs
// in. Countries feed the calling-code picker in front of the phone box.
func (s *Server) loginOptions(w http.ResponseWriter, r *http.Request) {
	rows, err := s.q.ListCountries(r.Context(), false)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	type country struct {
		Code        string `json:"code"`
		Name        string `json:"name"`
		CallingCode string `json:"calling_code"`
		IsDefault   bool   `json:"is_default"`
	}
	countries := make([]country, 0, len(rows))
	for _, c := range rows {
		if c.CallingCode == "" {
			continue
		}
		countries = append(countries, country{c.Code, c.Name, c.CallingCode, c.IsDefault})
	}
	links := map[string]string{}
	for _, k := range loginLinkKeys {
		v, err := s.q.GetPlatformSetting(r.Context(), "links."+k)
		if errors.Is(err, pgx.ErrNoRows) {
			continue
		}
		if err != nil {
			writeInternal(w, r, err)
			return
		}
		var u string
		if json.Unmarshal(v, &u) == nil && (strings.HasPrefix(u, "https://") || strings.HasPrefix(u, "http://")) {
			links[k] = u
		}
	}
	w.Header().Set("Cache-Control", "public, max-age=300")
	writeJSON(w, http.StatusOK, map[string]any{"countries": countries, "links": links})
}
