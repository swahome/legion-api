package books

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"

	"github.com/swahome/legion-api/internal/store"
)

// Milestone 8: what a group owns besides cash. Assets, stocks and money
// market investments are paid for with withdrawals and pay back with
// deposits, both linked to the holding; voiding the holding voids them all.

// Money is one movement in or out of an account for an investment.
type Money struct {
	AccountID   int64
	Method      *string
	Amount      string
	Date        *time.Time
	Description string
}

func (m Money) open(ctx context.Context, q *store.Queries, groupID int64, noun string, out bool) (store.GroupAccount, error) {
	if err := CheckAmountDate(m.Amount, m.Date, m.Method, noun); err != nil {
		return store.GroupAccount{}, err
	}
	acct, err := OpenAccount(ctx, q, groupID, m.AccountID)
	if err != nil {
		return acct, err
	}
	if out {
		if err := CheckBalance(ctx, q, acct, m.Amount); err != nil {
			return acct, err
		}
	}
	return acct, nil
}

func (m Money) withdraw(ctx context.Context, q *store.Queries, acct store.GroupAccount, kind store.WithdrawalKind,
	userID int64, link store.SetWithdrawalLinksParams) (int64, error) {
	id, err := WriteWithdrawal(ctx, q, acct, Withdrawal{Kind: kind, Amount: m.Amount}, m.Method, *m.Date,
		strings.TrimSpace(m.Description), userID)
	if err != nil {
		return 0, err
	}
	link.ID = id
	return id, q.SetWithdrawalLinks(ctx, link)
}

func (m Money) deposit(ctx context.Context, q *store.Queries, acct store.GroupAccount, kind store.DepositKind,
	userID int64, link store.SetDepositLinksParams) (int64, error) {
	id, err := writeDeposit(ctx, q, acct, kind, m.Method, m.Amount, *m.Date, strings.TrimSpace(m.Description), nil, userID)
	if err != nil {
		return 0, err
	}
	link.ID = id
	return id, q.SetDepositLinks(ctx, link)
}

// ---- Asset categories and assets ----

// Slug is the legacy slug of a name: lower case words joined by hyphens.
func Slug(name string) string {
	var b strings.Builder
	dash := false
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			dash = false
		case !dash && b.Len() > 0:
			b.WriteByte('-')
			dash = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// SaveAssetCategory creates (id 0) or renames an asset category.
func SaveAssetCategory(ctx context.Context, q *store.Queries, groupID, userID, id int64, name, description string) (int64, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return 0, Error{"missing_fields", "Asset category name is required"}
	}
	slug := Slug(name)
	taken, err := q.AssetCategorySlugTaken(ctx, store.AssetCategorySlugTakenParams{GroupID: groupID, Slug: slug, ExceptID: id})
	if err != nil {
		return 0, err
	}
	if taken {
		return 0, Error{"duplicate_name", "Asset category name already exists"}
	}
	if id == 0 {
		return q.CreateAssetCategory(ctx, store.CreateAssetCategoryParams{GroupID: groupID, Name: name, Slug: slug,
			Description: strings.TrimSpace(description), CreatedBy: By(userID)})
	}
	if _, err := GetAssetCategory(ctx, q, groupID, id); err != nil {
		return 0, err
	}
	return id, q.UpdateAssetCategory(ctx, store.UpdateAssetCategoryParams{GroupID: groupID, ID: id, Name: name, Slug: slug,
		Description: strings.TrimSpace(description)})
}

func GetAssetCategory(ctx context.Context, q *store.Queries, groupID, id int64) (store.AssetCategory, error) {
	c, err := q.GetAssetCategory(ctx, store.GetAssetCategoryParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return c, Error{"not_found", "Could not find asset category"}
	}
	return c, err
}

// NewAsset is something the group owns.
type NewAsset struct {
	Name        string
	Cost        string
	CategoryID  int64
	Description string
}

func CreateAsset(ctx context.Context, q *store.Queries, groupID, userID int64, a NewAsset) (int64, error) {
	a.Name = strings.TrimSpace(a.Name)
	if a.Name == "" {
		return 0, Error{"missing_fields", "Asset name is required"}
	}
	if a.Cost == "" || !ValidAmount(a.Cost) || Cents(a.Cost) <= 0 {
		return 0, Error{"invalid_amount", "Asset cost must be more than zero"}
	}
	cat, err := GetAssetCategory(ctx, q, groupID, a.CategoryID)
	if err != nil {
		return 0, err
	}
	return q.CreateAsset(ctx, store.CreateAssetParams{GroupID: groupID, AssetCategoryID: &cat.ID, Name: a.Name,
		Description: strings.TrimSpace(a.Description), CreatedBy: By(userID), Cost: numeric(a.Cost)})
}

func getAsset(ctx context.Context, q *store.Queries, groupID, id int64) (store.Asset, error) {
	a, err := q.GetAsset(ctx, store.GetAssetParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return a, Error{"not_found", "Could not find group asset"}
	}
	return a, err
}

// PayForAsset records an asset_purchase withdrawal.
func PayForAsset(ctx context.Context, q *store.Queries, groupID, userID, assetID int64, m Money) (int64, error) {
	a, err := getAsset(ctx, q, groupID, assetID)
	if err != nil {
		return 0, err
	}
	acct, err := m.open(ctx, q, groupID, "payment", true)
	if err != nil {
		return 0, err
	}
	if m.Description == "" {
		m.Description = "Payment for " + a.Name
	}
	return m.withdraw(ctx, q, acct, store.WithdrawalKindAssetPurchase, userID, store.SetWithdrawalLinksParams{AssetID: &a.ID})
}

// SellAsset records an asset_sale deposit.
func SellAsset(ctx context.Context, q *store.Queries, groupID, userID, assetID int64, m Money) (int64, error) {
	a, err := getAsset(ctx, q, groupID, assetID)
	if err != nil {
		return 0, err
	}
	acct, err := m.open(ctx, q, groupID, "sale", false)
	if err != nil {
		return 0, err
	}
	if m.Description == "" {
		m.Description = "Sale of " + a.Name
	}
	return m.deposit(ctx, q, acct, store.DepositKindAssetSale, userID, store.SetDepositLinksParams{AssetID: &a.ID})
}

// voidLinked voids every live deposit and withdrawal tied to a holding.
func voidLinked(ctx context.Context, q *store.Queries, groupID, userID int64, asset, stock, mmi *int64) error {
	deps, err := q.LinkedDepositIDs(ctx, store.LinkedDepositIDsParams{GroupID: groupID, AssetID: asset, StockID: stock, MmiID: mmi})
	if err != nil {
		return err
	}
	for _, id := range deps {
		if err := voidDeposit(ctx, q, groupID, id, userID); err != nil {
			return err
		}
	}
	wds, err := q.LinkedWithdrawalIDs(ctx, store.LinkedWithdrawalIDsParams{GroupID: groupID, AssetID: asset, StockID: stock, MmiID: mmi})
	if err != nil {
		return err
	}
	for _, id := range wds {
		if err := voidWithdrawal(ctx, q, groupID, id, userID); err != nil {
			return err
		}
	}
	return nil
}

func VoidAsset(ctx context.Context, q *store.Queries, groupID, userID, id int64) error {
	n, err := q.VoidAsset(ctx, store.VoidAssetParams{GroupID: groupID, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return Error{"not_found", "Could not find group asset"}
	}
	return voidLinked(ctx, q, groupID, userID, &id, nil, nil)
}

// ---- Stocks ----

// shares parses a positive share count or price with up to four places.
func shares(s string) (float64, bool) {
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	return f, err == nil && f > 0 && f < 1e11
}

func fixed4(f float64) string { return strconv.FormatFloat(f, 'f', 4, 64) }

// BuyStock records the shares and the stock_purchase withdrawal that paid
// for them (shares × price).
func BuyStock(ctx context.Context, q *store.Queries, groupID, userID int64, name, count, price string, m Money) (int64, error) {
	name = strings.TrimSpace(name)
	n, okN := shares(count)
	pr, okP := shares(price)
	switch {
	case name == "":
		return 0, Error{"missing_fields", "Stock name is required"}
	case !okN:
		return 0, Error{"invalid_shares", "Number of shares must be more than zero"}
	case !okP:
		return 0, Error{"invalid_price", "Price per share must be more than zero"}
	}
	m.Amount = strconv.FormatFloat(n*pr, 'f', 2, 64)
	acct, err := m.open(ctx, q, groupID, "stock purchase", true)
	if err != nil {
		return 0, err
	}
	id, err := q.CreateStock(ctx, store.CreateStockParams{GroupID: groupID, Name: name, PurchaseDate: *m.Date,
		AccountID: &acct.ID, CreatedBy: By(userID), Shares: numeric(fixed4(n)), Price: numeric(fixed4(pr))})
	if err != nil {
		return 0, err
	}
	if m.Description == "" {
		m.Description = "Purchase of " + strconv.FormatFloat(n, 'f', -1, 64) + " " + name + " shares"
	}
	_, err = m.withdraw(ctx, q, acct, store.WithdrawalKindStockPurchase, userID, store.SetWithdrawalLinksParams{StockID: &id})
	return id, err
}

func SetStockPrice(ctx context.Context, q *store.Queries, groupID, id int64, price string) error {
	pr, ok := shares(price)
	if !ok {
		return Error{"invalid_price", "Current price must be more than zero"}
	}
	if _, err := lockStock(ctx, q, groupID, id); err != nil {
		return err
	}
	return q.SetStockPrice(ctx, store.SetStockPriceParams{GroupID: groupID, ID: id, Price: numeric(fixed4(pr))})
}

func lockStock(ctx context.Context, q *store.Queries, groupID, id int64) (store.LockStockRow, error) {
	s, err := q.LockStock(ctx, store.LockStockParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return s, Error{"not_found", "Could not find stock"}
	}
	return s, err
}

// SellStock records a sale of some of a stock's shares and its stock_sale
// deposit.
func SellStock(ctx context.Context, q *store.Queries, groupID, userID, stockID int64, count, price string, m Money) (int64, error) {
	s, err := lockStock(ctx, q, groupID, stockID)
	if err != nil {
		return 0, err
	}
	n, okN := shares(count)
	pr, okP := shares(price)
	if !okN {
		return 0, Error{"invalid_shares", "Number of shares to be sold must be more than zero"}
	}
	if !okP {
		return 0, Error{"invalid_price", "Sale price per share must be more than zero"}
	}
	held, _ := strconv.ParseFloat(s.Shares, 64)
	sold, _ := strconv.ParseFloat(s.SharesSold, 64)
	if n > held-sold+1e-9 {
		return 0, Error{"too_many_shares", "You can only sell up to " + strconv.FormatFloat(held-sold, 'f', -1, 64) + " shares"}
	}
	m.Amount = strconv.FormatFloat(n*pr, 'f', 2, 64)
	acct, err := m.open(ctx, q, groupID, "stock sale", false)
	if err != nil {
		return 0, err
	}
	saleID, err := q.CreateStockSale(ctx, store.CreateStockSaleParams{GroupID: groupID, StockID: s.ID, AccountID: &acct.ID,
		SaleDate: *m.Date, CreatedBy: By(userID), Shares: numeric(fixed4(n)), Price: numeric(fixed4(pr))})
	if err != nil {
		return 0, err
	}
	if err := q.AddStockSharesSold(ctx, store.AddStockSharesSoldParams{GroupID: groupID, ID: s.ID, Shares: numeric(fixed4(n))}); err != nil {
		return 0, err
	}
	if m.Description == "" {
		m.Description = "Sale of " + strconv.FormatFloat(n, 'f', -1, 64) + " " + s.Name + " shares"
	}
	_, err = m.deposit(ctx, q, acct, store.DepositKindStockSale, userID, store.SetDepositLinksParams{StockID: &s.ID, StockSaleID: &saleID})
	return saleID, err
}

func VoidStock(ctx context.Context, q *store.Queries, groupID, userID, id int64) error {
	if _, err := lockStock(ctx, q, groupID, id); err != nil {
		return err
	}
	if _, err := q.VoidStock(ctx, store.VoidStockParams{GroupID: groupID, ID: id}); err != nil {
		return err
	}
	if err := q.VoidStockSales(ctx, store.VoidStockSalesParams{GroupID: groupID, StockID: id}); err != nil {
		return err
	}
	return voidLinked(ctx, q, groupID, userID, nil, &id, nil)
}

// ---- Money market investments ----

func Invest(ctx context.Context, q *store.Queries, groupID, userID int64, institution string, m Money) (int64, error) {
	institution = strings.TrimSpace(institution)
	if institution == "" {
		return 0, Error{"missing_fields", "Investment institution name is required"}
	}
	acct, err := m.open(ctx, q, groupID, "investment", true)
	if err != nil {
		return 0, err
	}
	desc := strings.TrimSpace(m.Description)
	id, err := q.CreateMoneyMarketInvestment(ctx, store.CreateMoneyMarketInvestmentParams{GroupID: groupID,
		Institution: institution, InvestmentDate: *m.Date, Description: desc, AccountID: &acct.ID,
		CreatedBy: By(userID), Amount: numeric(m.Amount)})
	if err != nil {
		return 0, err
	}
	if m.Description == "" {
		m.Description = "Money market investment in " + institution
	}
	_, err = m.withdraw(ctx, q, acct, store.WithdrawalKindMoneyMarketInvestment, userID, store.SetWithdrawalLinksParams{MmiID: &id})
	return id, err
}

func lockInvestment(ctx context.Context, q *store.Queries, groupID, id int64) (store.LockMoneyMarketInvestmentRow, error) {
	mm, err := q.LockMoneyMarketInvestment(ctx, store.LockMoneyMarketInvestmentParams{GroupID: groupID, ID: id})
	if errors.Is(err, pgx.ErrNoRows) {
		return mm, Error{"not_found", "Could not find money market investment"}
	}
	if err == nil && mm.IsClosed {
		return mm, Error{"investment_closed", "This money market investment has already been cashed in"}
	}
	return mm, err
}

func TopUpInvestment(ctx context.Context, q *store.Queries, groupID, userID, id int64, m Money) error {
	mm, err := lockInvestment(ctx, q, groupID, id)
	if err != nil {
		return err
	}
	acct, err := m.open(ctx, q, groupID, "top up", true)
	if err != nil {
		return err
	}
	if m.Date.Before(mm.InvestmentDate) {
		return Error{"invalid_date", "A top up cannot be dated before the investment"}
	}
	if err := q.TopUpMoneyMarketInvestment(ctx, store.TopUpMoneyMarketInvestmentParams{GroupID: groupID, ID: id, Amount: numeric(m.Amount)}); err != nil {
		return err
	}
	if m.Description == "" {
		m.Description = "Top up of money market investment in " + mm.Institution
	}
	_, err = m.withdraw(ctx, q, acct, store.WithdrawalKindMoneyMarketInvestment, userID, store.SetWithdrawalLinksParams{MmiID: &id})
	return err
}

// CashIn records the money coming back (a money_market deposit) and closes
// the investment.
func CashIn(ctx context.Context, q *store.Queries, groupID, userID, id int64, m Money) error {
	mm, err := lockInvestment(ctx, q, groupID, id)
	if err != nil {
		return err
	}
	acct, err := m.open(ctx, q, groupID, "cash in", false)
	if err != nil {
		return err
	}
	if m.Date.Before(mm.InvestmentDate) {
		return Error{"invalid_date", "A cash in cannot be dated before the investment"}
	}
	if err := q.CashInMoneyMarketInvestment(ctx, store.CashInMoneyMarketInvestmentParams{Amount: numeric(m.Amount),
		CashInDate: m.Date, GroupID: groupID, ID: id}); err != nil {
		return err
	}
	if m.Description == "" {
		m.Description = "Cash in from " + mm.Institution
	}
	_, err = m.deposit(ctx, q, acct, store.DepositKindMoneyMarket, userID, store.SetDepositLinksParams{MmiID: &id})
	return err
}

func VoidInvestment(ctx context.Context, q *store.Queries, groupID, userID, id int64) error {
	n, err := q.VoidMoneyMarketInvestment(ctx, store.VoidMoneyMarketInvestmentParams{GroupID: groupID, ID: id})
	if err != nil {
		return err
	}
	if n == 0 {
		return Error{"not_found", "Could not find money market investment"}
	}
	return voidLinked(ctx, q, groupID, userID, nil, nil, &id)
}
