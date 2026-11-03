package mobile

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/mail"
	"github.com/swahome/legion-api/internal/pdf"
	"github.com/swahome/legion-api/internal/store"
)

// Statements beyond the contribution statement: the fine statement, the
// group's total savings, and the statement files members ask to download
// or have emailed. A file request is queued; RunStatements writes the PDF,
// then either leaves a "File Type Ready" notification with its link or
// emails it, as the legacy cron did.

// WithStatements sets where statement links point (the public address of
// the uploads folder, e.g. https://legion.com/v1/legacy/uploads) and how
// statements are emailed. mailer may be nil: emailed statements then fail
// with a note in the log.
func (s *Server) WithStatements(publicUploads string, mailer mail.Sender, log *slog.Logger) *Server {
	s.publicUploads, s.mailer, s.log = strings.TrimRight(publicUploads, "/"), mailer, log
	return s
}

// statementNames are the legacy statement_transaction_names.
var statementNames = map[int16]string{
	1: "Contribution invoice", 2: "Contribution fine invoice", 3: "Fine invoice", 4: "Miscellaneous invoice",
	9: "Contribution payment", 10: "Contribution payment", 11: "Contribution payment", 15: "Contribution payment",
	12: "Fine payment", 13: "Fine payment", 14: "Fine payment", 16: "Fine payment",
	21: "Contribution refund", 22: "Contribution refund", 23: "Contribution refund", 24: "Contribution refund",
	25: "Contribution transfer from ", 26: "Contribution transfer to", 27: "Contribution transfer to contribution",
	28: "Contribution transfer to fine", 29: "Loan payment transfer to contribution", 30: "Contribution transfer to loan",
	31: "Loan payment transfer to fine",
}

var defaultFineType = map[store.LedgerEntryType]int16{
	"contribution_fine_invoice": 2, "fine_invoice": 3, "fine_payment": 12, "contribution_to_fine_transfer": 28,
}

// ledgerLine is one row of a ledger statement.
type ledgerLine struct {
	date                   time.Time
	desc                   string
	payable, paid, balance float64
}

type ledgerStatement struct {
	opening              float64
	lines                []ledgerLine
	payable, paid, total float64
}

// memberLedger is a member's contribution or fine statement from from to
// to: the balance brought forward, each entry, and totals. Invoices and
// transfers out are payable; everything else that adds to what is owed
// takes from paid, as the legacy totals did.
func (s *Server) memberLedger(ctx context.Context, g *member, membershipID int64, bucket string, from, to time.Time) (ledgerStatement, error) {
	var st ledgerStatement
	open, err := s.q.MemberBucketBalanceBefore(ctx, store.MemberBucketBalanceBeforeParams{Bucket: bucket, GroupID: g.group.ID,
		MembershipID: &membershipID, Before: from})
	if err != nil {
		return st, err
	}
	st.opening = amount(open)
	st.total = st.opening
	rows, err := s.q.MemberLedgerEntries(ctx, store.MemberLedgerEntriesParams{Bucket: bucket, GroupID: g.group.ID,
		MembershipID: &membershipID, DateFrom: from, DateTo: to})
	if err != nil {
		return st, err
	}
	for _, e := range rows {
		t := defaultLegacyType[e.EntryType]
		if bucket == "fine" {
			t = defaultFineType[e.EntryType]
		}
		if e.LegacyType != nil {
			t = *e.LegacyType
		}
		amt := amount(e.Amount)
		l := ledgerLine{date: e.EntryDate, desc: statementNames[t]}
		switch {
		case e.Sign > 0 && (t <= 4 || t == 25):
			l.payable = amt
		case e.Sign > 0:
			l.paid = -amt
		default:
			l.paid = amt
		}
		switch {
		case t == 3:
			l.desc += " Fine for " + e.FineName
		case bucket == "fine" && t != 2 && e.ContributionName != "":
			l.desc += " for " + e.ContributionName
		case bucket == "fine" && t != 2 && e.FineName != "":
			l.desc += " for " + e.FineName
		case e.ContributionName != "":
			l.desc += " " + e.ContributionName
		}
		if l.desc == "" {
			l.desc = e.Description
		}
		st.payable += l.payable
		st.paid += l.paid
		st.total += float64(e.Sign) * amt
		l.balance = st.total
		st.lines = append(st.lines, l)
	}
	return st, nil
}

// statementMember finds a member of the caller's group for a statement.
func (s *Server) statementMember(ctx context.Context, g *member, membershipID int64) (*store.MobileGroupMembersRow, error) {
	rows, err := s.q.MobileGroupMembers(ctx, store.MobileGroupMembersParams{GroupID: g.group.ID, MembershipID: &membershipID, Lim: 1})
	if err != nil || len(rows) == 0 {
		return nil, err
	}
	return &rows[0], nil
}

func memberFullName(m *store.MobileGroupMembersRow) string {
	name := m.FirstName + " "
	if m.MiddleName != "" {
		name += m.MiddleName + " "
	}
	return name + m.LastName
}

func (s *Server) fineStatement(ctx context.Context, c *call, g *member) (any, error) {
	memberID := g.m.ID
	if !g.flag("enable_member_information_privacy") {
		if id := c.req.Int("member_id"); id != 0 {
			memberID = id
		}
	}
	m, err := s.statementMember(ctx, g, memberID)
	if err != nil {
		return nil, err
	}
	if m == nil {
		return timed(status(0, "Could not find group member details"), s.now()), nil
	}
	now := s.now().In(g.loc)
	from := now.AddDate(0, -3, 0)
	fromDay := time.Date(from.Year(), from.Month(), from.Day(), 0, 0, 0, 0, time.UTC)
	toDay := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	st, err := s.memberLedger(ctx, g, memberID, "fine", fromDay, toDay)
	if err != nil {
		return nil, err
	}
	body := []any{}
	for _, l := range st.lines {
		body = append(body, map[string]any{"description": l.desc, "date": shortDate(l.date), "invoice_id": nil,
			"deposit_id": nil, "payable": l.payable, "paid": l.paid, "balance": l.balance})
	}
	return map[string]any{
		"status": 1, "message": "Contribution statement", "time": s.now().Unix(),
		"member_datails": map[string]any{"name": memberFullName(m), "role": roleName(*m, "Member"), "email": deref(m.Email),
			"phone": legacyPhone(m.Phone), "avatar": nullable(m.Avatar)},
		"statement_details": map[string]any{"statement_as_at": shortDate(now), "statement_period_from": shortDate(from),
			"statement_period_to": shortDate(now)},
		"statement_header": map[string]any{"description": "Balance B/F", "date": shortDate(from), "payable": 0, "paid": 0,
			"balance": st.opening},
		"statement_body":   body,
		"statement_footer": map[string]any{"description": "Totals", "payable": st.payable, "paid": st.paid, "balance": st.total},
	}, nil
}

func (s *Server) totalGroupSavings(ctx context.Context, c *call, g *member) (any, error) {
	paid, err := s.q.GroupContributionsPaid(ctx, g.group.ID)
	if err != nil {
		return nil, err
	}
	return timed(map[string]any{"status": 1, "message": "Total Group contributions",
		"total_group_contributions": commaMoney(paid)}, s.now()), nil
}

const (
	statementByEmail    int16 = 1
	statementByDownload int16 = 2
)

var statementKinds = map[int16]string{1: "Contribution", 2: "Fine", 3: "Loan"}

// requestStatement is statements/send_email_statement (byEmail) and
// statements/download_statement: from and to default to the last three
// months, type is 1 contribution, 2 fine or 3 loan (with loan_id).
func (s *Server) requestStatement(action int16) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		r := c.req
		now := s.now().In(g.loc)
		to := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
		from := to.AddDate(0, -3, 0)
		if d := appDate(strings.ReplaceAll(r.Str("from"), ",", ""), g.loc); d != nil {
			from = *d
		}
		if d := appDate(strings.ReplaceAll(r.Str("to"), ",", ""), g.loc); d != nil {
			to = *d
		}
		kind := int16(r.Int("type"))
		if statementKinds[kind] == "" {
			kind = 1
		}
		fileType := int16(r.Int("file_type"))
		if fileType == 0 {
			fileType = 1
		}
		p := store.CreateStatementRequestParams{GroupID: g.group.ID, MembershipID: g.m.ID, UserID: g.user.ID, Kind: kind,
			Action: action, FileType: fileType, DateFrom: from, DateTo: to}
		if kind == 3 {
			p.LoanID = appID(r.Str("loan_id"))
		}
		msg := "Request received. You will receive a notification once the download is ready"
		if action == statementByEmail {
			email, own := strings.TrimSpace(r.Str("email")), deref(g.user.Email)
			switch {
			case email != "" && !mail.Valid(email):
				return timed(status(0, "Kindly enter a valid email address"), s.now()), nil
			case email == "" && !mail.Valid(own):
				return timed(status(0, "Kindly update profile and use a valid email address"), s.now()), nil
			case email == "":
				email = own
			case !mail.Valid(own):
				// As the legacy app did, a member without an email address
				// gets this one on their profile, unless someone has it.
				taken, err := s.q.EmailTaken(ctx, store.EmailTakenParams{Email: email, ExceptID: g.user.ID})
				if err != nil {
					return nil, err
				}
				if !taken {
					if err := s.q.UpdateUserEmail(ctx, store.UpdateUserEmailParams{Email: &email, ID: g.user.ID}); err != nil {
						return nil, err
					}
				}
			}
			p.Email = email
			msg = "Request received. Statement will be sent to " + email + "."
		}
		if _, err := s.q.CreateStatementRequest(ctx, p); err != nil {
			return nil, err
		}
		s.kickStatements()
		return timed(status(1, msg), s.now()), nil
	}
}

func (s *Server) kickStatements() {
	select {
	case s.statementKick <- struct{}{}:
	default:
	}
}

// RunStatements works through statement requests until ctx ends, waking
// on each new request and once a minute for retries. Requests are claimed
// with SKIP LOCKED, so more than one process can run it.
func (s *Server) RunStatements(ctx context.Context) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		for s.ProcessStatements(ctx) > 0 && ctx.Err() == nil {
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		case <-s.statementKick:
		}
	}
}

// ProcessStatements handles up to ten waiting requests and says how many
// it claimed.
func (s *Server) ProcessStatements(ctx context.Context) int {
	reqs, err := s.q.ClaimStatementRequests(ctx, 10)
	if err != nil {
		if ctx.Err() == nil {
			s.logger().Error("statements: claim", "err", err)
		}
		return 0
	}
	for _, r := range reqs {
		path, err := s.processStatement(ctx, r)
		if err != nil {
			s.logger().Error("statements: request failed", "id", r.ID, "attempt", r.Attempts, "err", err)
			if err := s.q.RetryStatementRequest(ctx, store.RetryStatementRequestParams{ID: r.ID, Error: err.Error()}); err != nil {
				s.logger().Error("statements: retry", "id", r.ID, "err", err)
			}
			continue
		}
		if err := s.q.FinishStatementRequest(ctx, store.FinishStatementRequestParams{ID: r.ID, Status: "done", FilePath: path}); err != nil {
			s.logger().Error("statements: finish", "id", r.ID, "err", err)
		}
	}
	return len(reqs)
}

func (s *Server) logger() *slog.Logger {
	if s.log == nil {
		return slog.Default()
	}
	return s.log
}

func (s *Server) processStatement(ctx context.Context, r store.StatementRequest) (string, error) {
	if r.Action == statementByEmail && s.mailer == nil {
		return "", fmt.Errorf("email is not set up (LEGION_SMTP_HOST)")
	}
	if s.uploads == "" {
		return "", fmt.Errorf("no uploads folder (LEGION_UPLOADS_DIR)")
	}
	g, reply, err := s.inGroup(ctx, &call{userID: r.UserID, req: request{"group_id": strconv.FormatInt(r.GroupID, 10)}}, 0)
	if err != nil {
		return "", err
	}
	if reply != nil || g.m.ID != r.MembershipID {
		return "", fmt.Errorf("member %d is no longer in group %d", r.MembershipID, r.GroupID)
	}
	m, err := s.statementMember(ctx, g, r.MembershipID)
	if err != nil {
		return "", err
	}
	if m == nil {
		return "", fmt.Errorf("member %d not found", r.MembershipID)
	}
	kind := statementKinds[r.Kind]
	doc := statementDoc{title: kind + " Statement", group: g.group.Name, member: memberFullName(m),
		period: shortDate(r.DateFrom) + " to " + shortDate(r.DateTo), asAt: shortDate(s.now().In(g.loc))}
	switch r.Kind {
	case 1, 2:
		bucket := "contribution"
		if r.Kind == 2 {
			bucket = "fine"
		}
		st, err := s.memberLedger(ctx, g, r.MembershipID, bucket, r.DateFrom, r.DateTo)
		if err != nil {
			return "", err
		}
		doc.columns = []string{"Date", "Description", "Payable", "Paid", "Balance"}
		doc.rows = append(doc.rows, []string{shortDate(r.DateFrom), "Balance B/F", "", "", money(st.opening)})
		for _, l := range st.lines {
			doc.rows = append(doc.rows, []string{shortDate(l.date), l.desc, money(l.payable), money(l.paid), money(l.balance)})
		}
		doc.footer = []string{"", "Totals", money(st.payable), money(st.paid), money(st.total)}
	case 3:
		if r.LoanID == nil {
			return "", fmt.Errorf("loan statement without a loan")
		}
		d, err := books.GetLoanDetail(ctx, s.q, g.group.ID, *r.LoanID, s.today(g))
		if err != nil {
			return "", err
		}
		if !books.Is(d.Loan.MembershipID, r.MembershipID) {
			return "", fmt.Errorf("loan %d is not this member's", *r.LoanID)
		}
		doc.subtitle = "Loan disbursed on " + mobileTime(d.Loan.DisbursedOn) + " at " +
			loanInterestText(books.NumText(d.Loan.InterestRate), d.Loan.RatePer, d.Loan.InterestType)
		doc.columns = []string{"Date", "Description", "Payable", "Paid", "Balance"}
		doc.rows = append(doc.rows, []string{"", "Total Amount Payable", money(float64(d.Payable) / 100), "", money(float64(d.Payable) / 100)})
		var paid int64
		for _, rp := range d.Repayments {
			paid += books.Cents(rp.Amount)
			doc.rows = append(doc.rows, []string{shortDate(rp.PaidOn), "Payment", "", money(amount(rp.Amount)), money(float64(d.Payable-paid) / 100)})
		}
		doc.footer = []string{"", "Totals", money(float64(d.Payable) / 100), money(float64(d.Paid) / 100), money(float64(d.Payable-d.Paid) / 100)}
	}
	file := pdfFileName(m, kind, g.group.Name, r.ID)
	data := doc.render()
	dir := filepath.Join(s.uploads, "statements")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dir, file), data, 0o644); err != nil {
		return "", err
	}
	rel := "statements/" + file
	if r.Action == statementByEmail {
		body := fmt.Sprintf("Dear %s,\n\nAttached is your %s statement for %s, from %s to %s.\n",
			m.FirstName, strings.ToLower(kind), g.group.Name, shortDate(r.DateFrom), shortDate(r.DateTo))
		err := s.mailer.Send(ctx, mail.Message{To: r.Email, Subject: kind + " Statement - " + g.group.Name, Body: body,
			FileName: file, File: data})
		return rel, err
	}
	uid := r.UserID
	link := s.publicUploads + "/" + rel
	return rel, notify(ctx, s.q, g.group.ID, r.MembershipID, &uid, &uid, "File Type Ready", kind+" Statement ready for download",
		categoryStatement, nil, &notificationFile{path: link, kind: "pdf", size: int64(len(data)), cta: "View File"})
}

func money(v float64) string {
	if v == 0 {
		return "0.00"
	}
	return commaMoney(fmt.Sprintf("%.2f", v))
}

// pdfFileName is like the legacy "<first><last><id> Contribution Statement
// - <group>", made safe for a URL and unique per request.
func pdfFileName(m *store.MobileGroupMembersRow, kind, group string, id int64) string {
	safe := func(s string) string {
		var b strings.Builder
		for _, r := range s {
			switch {
			case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
				b.WriteRune(r)
			case r == ' ' || r == '-' || r == '_':
				b.WriteByte('-')
			}
		}
		return strings.Trim(b.String(), "-")
	}
	// The folder is served without a login, as the old one was, so the name
	// carries a random part nobody can guess.
	var r [8]byte
	_, _ = rand.Read(r[:])
	return fmt.Sprintf("%s-%s-statement-%s-%d-%s.pdf", safe(m.FirstName+m.LastName), strings.ToLower(kind), safe(group), id, hex.EncodeToString(r[:]))
}

// statementDoc lays a statement out on A4 pages.
type statementDoc struct {
	title, subtitle, group, member, period, asAt string
	columns                                      []string
	rows                                         [][]string
	footer                                       []string
}

func (d statementDoc) render() []byte {
	var doc pdf.Doc
	const left, right = 40.0, pdf.PageWidth - 40
	// Column right edges; the description column is left aligned.
	edges := []float64{left, left + 70, right - 180, right - 90, right}
	y := 0.0
	header := func() {
		doc.AddPage()
		doc.Text(left, 50, 16, true, d.title)
		doc.Text(left, 70, 10, false, d.group)
		doc.Text(left, 86, 10, false, "Member: "+d.member)
		doc.Text(left, 102, 10, false, "Period: "+d.period+"    Statement as at "+d.asAt)
		y = 118
		if d.subtitle != "" {
			doc.Text(left, y, 10, false, d.subtitle)
			y += 16
		}
		y += 10
		d.row(&doc, edges, y, d.columns, true)
		doc.Line(left, y+5, right, y+5)
		y += 20
	}
	header()
	for _, r := range d.rows {
		if y > pdf.PageHeight-60 {
			header()
		}
		d.row(&doc, edges, y, r, false)
		y += 15
	}
	if d.footer != nil {
		doc.Line(left, y-10, right, y-10)
		d.row(&doc, edges, y+2, d.footer, true)
	}
	return doc.Bytes()
}

func (d statementDoc) row(doc *pdf.Doc, edges []float64, y float64, cells []string, bold bool) {
	const size = 9
	for i, c := range cells {
		switch i {
		case 0:
			doc.Text(edges[0], y, size, bold, c)
		case 1:
			doc.Text(edges[1], y, size, bold, pdf.Fit(c, edges[2]-edges[1]-70, size, bold))
		default:
			doc.TextRight(edges[i], y, size, bold, c)
		}
	}
}
