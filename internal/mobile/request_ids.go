package mobile

import (
	"context"
	"strconv"
	"strings"
	"time"
)

// requestIDActions need a request id (legacy Mobile_Controller
// _request_id_required). Some arrive with later milestones; the check
// applies once they are registered.
var requestIDActions = map[string]bool{
	"deposits/record_contribution_payments":         true,
	"deposits/record_fine_payments":                 true,
	"deposits/record_loan_repayments":               true,
	"deposits/record_miscellaneous_payments":        true,
	"deposits/record_income":                        true,
	"deposits/record_bank_loan":                     true,
	"deposits/record_contribution_transfer":         true,
	"withdrawals/record_expenses":                   true,
	"withdrawals/record_funds_transfer":             true,
	"withdrawals/record_contribution_refund":        true,
	"withdrawals/record_bank_loan_repayment":        true,
	"withdrawals/request_funds_transfer":            true,
	"loan_types/create_loan_type":                   true,
	"setup_tasks/create_group_contribution_setting": true,
	"invoices/create":                               true,
	"assets/record_asset_purchase_payments":         true,
	"stocks/record_stock_purchase":                  true,
}

// claimRequestID records the request id, or returns the legacy answer: 11
// when there is none, 13 unless it starts with a unix time from today
// (Nairobi), 12 when it was seen before.
func (s *Server) claimRequestID(ctx context.Context, id string) (map[string]any, error) {
	if id == "" {
		return status(11, "You are currently using an outdated application. Update to the latest version"), nil
	}
	head, _, _ := strings.Cut(id, "_")
	ts, err := strconv.ParseInt(head, 10, 64)
	now := s.now().In(nairobi)
	if err != nil || time.Unix(ts, 0).In(nairobi).Format("020106") != now.Format("020106") {
		return status(13, "Request id format is invalid. Kindly try again"), nil
	}
	n, err := s.q.ClaimMobileRequestID(ctx, id)
	if err != nil {
		return nil, err
	}
	if n == 0 {
		return status(12, "Duplicate request"), nil
	}
	return nil, nil
}

// failed reports whether an action answered with status 0.
func failed(resp any) bool {
	m, ok := resp.(map[string]any)
	if !ok {
		return false
	}
	switch v := m["status"].(type) {
	case int:
		return v == 0
	case string:
		return v == "0"
	}
	return false
}
