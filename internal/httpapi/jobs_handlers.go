package httpapi

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	"github.com/swahome/legion-api/internal/billing"
	"github.com/swahome/legion-api/internal/store"
)

// Background jobs in the admin panel: the run history of the nightly
// billing job, and a button to run it now.

// WithBilling lets admins run the billing job from the API. loc is the
// groups' calendar, used to work out "today".
func (s *Server) WithBilling(e *billing.Engine, loc *time.Location) *Server {
	s.billing, s.location = e, loc
	return s
}

type jobRunResponse struct {
	ID         int64           `json:"id"`
	Job        string          `json:"job"`
	RunDate    Date            `json:"run_date"`
	Trigger    string          `json:"trigger"`
	Status     string          `json:"status"`
	StartedAt  time.Time       `json:"started_at"`
	FinishedAt *time.Time      `json:"finished_at"`
	Summary    json.RawMessage `json:"summary"`
	Error      string          `json:"error"`
	StartedBy  *namedRef       `json:"started_by"`
}

func (s *Server) adminListJobRuns(w http.ResponseWriter, r *http.Request) {
	pg := pageParams(r)
	job := queryPtr(r, "job")
	rows, err := s.q.ListJobRuns(r.Context(), store.ListJobRunsParams{Job: job, Lim: pg.size, Off: pg.offset})
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	total, err := s.q.CountJobRuns(r.Context(), job)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	items := make([]jobRunResponse, 0, len(rows))
	for _, j := range rows {
		out := jobRunResponse{
			ID: j.ID, Job: j.Job, RunDate: Date{j.RunDate}, Trigger: j.Trigger, Status: j.Status,
			StartedAt: j.StartedAt, FinishedAt: j.FinishedAt, Summary: j.Summary, Error: j.Error,
		}
		if j.StartedBy != nil {
			out.StartedBy = &namedRef{ID: *j.StartedBy, Name: j.StartedByName}
		}
		items = append(items, out)
	}
	writeJSON(w, http.StatusOK, newPaged(items, total, pg))
}

type runBillingRequest struct {
	// Date to bill; default today. Entries already raised for a day are
	// not raised again, so re-running a day is harmless.
	Date *Date `json:"date"`
}

// adminRunBilling starts a billing run in the background and returns at
// once; the run shows in the job history.
func (s *Server) adminRunBilling(w http.ResponseWriter, r *http.Request) {
	if s.billing == nil {
		writeError(w, http.StatusServiceUnavailable, "billing_off", "Billing is not set up on this server.")
		return
	}
	var req runBillingRequest
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	today := billing.LocalToday(time.Now(), s.location)
	day := today
	if req.Date != nil {
		if req.Date.After(today) {
			writeError(w, http.StatusBadRequest, "invalid_date", "Billing can't run for a day that hasn't come yet.")
			return
		}
		day = req.Date.Time
	}
	running, err := s.q.IsJobRunning(r.Context(), billing.JobName)
	if err != nil {
		writeInternal(w, r, err)
		return
	}
	if running {
		writeError(w, http.StatusConflict, "job_running", "Billing is running already.")
		return
	}
	uid := claimsFrom(r.Context()).UserID
	s.audit(r, "billing.run", "job", 0, map[string]string{"date": day.Format(time.DateOnly)})
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Hour)
		defer cancel()
		if _, err := s.billing.Run(ctx, billing.RunOptions{Today: day, Trigger: "manual", StartedBy: &uid}); err != nil {
			slog.Error("manual billing run", "err", err)
		}
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "started", "date": day.Format(time.DateOnly)})
}
