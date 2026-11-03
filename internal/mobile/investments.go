package mobile

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/swahome/legion-api/internal/books"
	"github.com/swahome/legion-api/internal/store"
)

// Milestone 8: asset categories, assets, stocks and money market
// investments. Writes are for officials (the legacy app checked that only
// for asset categories; here it holds for all, as on the web). Wording and
// shapes follow the legacy modules.

// rule is one CodeIgniter form rule: the field, its label and what it must
// hold ("" just required, "date", "amount", "number", "optional").
type rule struct{ field, label, kind string }

// checkForm answers "Form validation failed" with every failing field, the
// way the legacy form_validation did.
func (s *Server) checkForm(c *call, g *member, rules ...rule) map[string]any {
	errs := map[string]string{}
	for _, r := range rules {
		v := c.req.Str(r.field)
		if v == "" {
			if r.kind != "optional" {
				errs[r.field] = "The " + r.label + " field is required."
			}
			continue
		}
		switch r.kind {
		case "date":
			if appDate(v, g.loc) == nil {
				errs[r.field] = "The " + r.label + " field must contain a valid date."
			}
		case "amount":
			if appAmount(v) == "" {
				errs[r.field] = "The " + r.label + " field must be a valid amount."
			}
		case "number":
			if _, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err != nil {
				errs[r.field] = "The " + r.label + " field must contain only numbers."
			}
		}
	}
	if len(errs) > 0 {
		return s.formFailed(errs)
	}
	return nil
}

// checkAccount is the legacy _valid_account_id callback.
func (s *Server) checkAccount(ctx context.Context, g *member, field, v string) (map[string]any, error) {
	_, accounts, err := s.groupAccounts(ctx, g)
	if err != nil {
		return nil, err
	}
	if _, ok := accounts[appAccount(v)]; !ok {
		return s.formFailed(map[string]string{field: "Group account does not exist"}), nil
	}
	return nil, nil
}

// money reads one movement of money from the request.
func (s *Server) money(c *call, g *member, account, date, amount, description, method string) books.Money {
	m := books.Money{AccountID: appAccount(c.req.Str(account)), Date: appDate(c.req.Str(date), g.loc),
		Amount: appAmount(c.req.Str(amount))}
	if description != "" {
		m.Description = c.req.Str(description)
	}
	if method != "" {
		m.Method = appMethod(withdrawalMethodCodes, c.req.Str(method))
	}
	return m
}

// write runs a books call in a transaction and answers its outcome: a books
// error's message with status 0, or ok.
func (s *Server) write(ctx context.Context, fn func(q *store.Queries) error, ok string) (any, error) {
	err := s.inTx(ctx, fn)
	if be, isBooks := asBooksError(err); isBooks {
		return timed(status(0, be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return timed(status(1, ok), s.now()), nil
}

func (s *Server) accountNames(ctx context.Context, g *member) (func(*int64) string, error) {
	_, accounts, err := s.groupAccounts(ctx, g)
	if err != nil {
		return nil, err
	}
	return func(id *int64) string {
		if id == nil {
			return ""
		}
		if a, ok := accounts[*id]; ok {
			return accountLabel(a)
		}
		return ""
	}, nil
}

// ---- Asset categories ----

func (s *Server) getGroupAssetCategories(ctx context.Context, c *call, g *member) (any, error) {
	limit, offset := pageOf(c, 100)
	rows, err := s.q.ListAssetCategories(ctx, store.ListAssetCategoriesParams{GroupID: g.group.ID, Lim: limit, Off: offset})
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		out = append(out, map[string]any{"id": r.ID, "name": r.Name, "description": r.Description,
			"is_hidden": boolInt(r.IsHidden), "active": boolInt(r.IsActive)})
	}
	return map[string]any{"status": 1, "message": "success", "asset_categories": out}, nil
}

const officialsOperation = "Only group officials are allowed to perform this operation"

// assetOfficials is the asset categories' own wording for non-officials.
func (s *Server) assetOfficials(fn groupFn) groupFn {
	return func(ctx context.Context, c *call, g *member) (any, error) {
		if !g.official() {
			return status(0, officialsOperation), nil
		}
		return fn(ctx, c, g)
	}
}

func (s *Server) saveAssetCategory(edit bool) groupFn {
	return s.assetOfficials(func(ctx context.Context, c *call, g *member) (any, error) {
		if reply := s.checkForm(c, g, rule{"name", "Category Name", ""}); reply != nil {
			return reply, nil
		}
		var id int64
		if edit {
			id = c.req.Int("id")
			if id <= 0 {
				return status(0, "Could not find group asset category to edit"), nil
			}
		}
		name := c.req.Str("name")
		err := s.inTx(ctx, func(q *store.Queries) error {
			_, err := books.SaveAssetCategory(ctx, q, g.group.ID, g.user.ID, id, name, c.req.Str("description"))
			return err
		})
		if be, ok := asBooksError(err); ok {
			switch be.Code {
			case "duplicate_name":
				return s.formFailed(map[string]string{"slug": "Another Asset Category by the name " + name + " already exists"}), nil
			case "not_found":
				return status(0, "Could not find group asset category to edit"), nil
			}
			return status(0, be.Msg), nil
		}
		if err != nil {
			return nil, err
		}
		if edit {
			return status(1, name+" successfully updated"), nil
		}
		return status(1, name+" as a asset category was successfully"), nil
	})
}

func (s *Server) hideAssetCategory(hide bool) groupFn {
	return s.assetOfficials(func(ctx context.Context, c *call, g *member) (any, error) {
		cat, err := books.GetAssetCategory(ctx, s.q, g.group.ID, c.req.Int("id"))
		if _, ok := asBooksError(err); ok {
			return status(0, "Could not find group asset category to edit"), nil
		}
		if err != nil {
			return nil, err
		}
		switch {
		case hide && cat.IsHidden:
			return status(0, "Asset category is already headen"), nil
		case !hide && !cat.IsHidden:
			return status(0, "Asset category is visible and cannot be hidden"), nil
		}
		if err := s.q.SetAssetCategoryHidden(ctx, store.SetAssetCategoryHiddenParams{GroupID: g.group.ID, ID: cat.ID, IsHidden: hide}); err != nil {
			return nil, err
		}
		if hide {
			return status(1, "Asset category successfully hidden"), nil
		}
		return status(1, "Asset category successfully unhidden"), nil
	})
}

// ---- Assets ----

func (s *Server) createAsset(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.checkForm(c, g, rule{"name", "Asset Name", ""}, rule{"cost", "Asset Cost", "amount"},
		rule{"asset_category_id", "Asset Category", "number"}); reply != nil {
		return reply, nil
	}
	a := books.NewAsset{Name: c.req.Str("name"), Cost: appAmount(c.req.Str("cost")),
		CategoryID: c.req.Int("asset_category_id"), Description: c.req.Str("description")}
	err := s.inTx(ctx, func(q *store.Queries) error {
		_, err := books.CreateAsset(ctx, q, g.group.ID, g.user.ID, a)
		return err
	})
	if be, ok := asBooksError(err); ok {
		if be.Code == "not_found" {
			return s.formFailed(map[string]string{"asset_category_id": "Asset Category does not exist"}), nil
		}
		return status(0, be.Msg), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, "Successfully added asset"), nil
}

func (s *Server) getGroupAssets(ctx context.Context, c *call, g *member) (any, error) {
	limit, offset := pageOf(c, 100)
	rows, err := s.q.ListAssets(ctx, store.ListAssetsParams{GroupID: g.group.ID, Lim: limit, Off: offset})
	if err != nil {
		return nil, err
	}
	out := []any{}
	var total int64
	for _, r := range rows {
		out = append(out, map[string]any{"id": r.ID, "name": r.Name, "cost": r.Cost, "Category": r.CategoryName,
			"description": r.Description, "active": 1})
		total += books.Cents(r.Cost)
	}
	return map[string]any{"status": 1, "message": "success", "asset": out, "total_cost": float64(total) / 100}, nil
}

func (s *Server) getGroupAssetOptions(ctx context.Context, c *call, g *member) (any, error) {
	rows, err := s.q.ListAssets(ctx, store.ListAssetsParams{GroupID: g.group.ID, Lim: 10000})
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		out = append(out, map[string]any{"id": r.ID, "name": r.Name})
	}
	return map[string]any{"status": 1, "message": "success", "group_asset_options": out}, nil
}

func (s *Server) assetAccountCheck(ctx context.Context, c *call, g *member) (map[string]any, error) {
	if c.req.Int("asset_id") > 0 {
		if _, err := s.q.GetAsset(ctx, store.GetAssetParams{GroupID: g.group.ID, ID: c.req.Int("asset_id")}); err != nil {
			return s.formFailed(map[string]string{"asset_id": "Asset selected does not exist"}), nil
		}
	}
	return s.checkAccount(ctx, g, "account_id", c.req.Str("account_id"))
}

func (s *Server) recordAssetPurchasePayment(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.checkForm(c, g, rule{"payment_date", "Payment Date", "date"}, rule{"asset_id", "Asset", ""},
		rule{"account_id", "Account", ""}, rule{"payment_method", "Payment Method", "number"},
		rule{"amount", "Amount", "amount"}); reply != nil {
		return reply, nil
	}
	if reply, err := s.assetAccountCheck(ctx, c, g); reply != nil || err != nil {
		return reply, err
	}
	m := s.money(c, g, "account_id", "payment_date", "amount", "description", "payment_method")
	return s.write(ctx, func(q *store.Queries) error {
		_, err := books.PayForAsset(ctx, q, g.group.ID, g.user.ID, c.req.Int("asset_id"), m)
		return err
	}, "Successfully recorded asset purchase payment")
}

func (s *Server) sellAsset(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.checkForm(c, g, rule{"amount", "Sale Amount", "amount"}, rule{"sale_date", "Sale Date", "date"},
		rule{"account_id", "Account", ""}); reply != nil {
		return reply, nil
	}
	if reply, err := s.checkAccount(ctx, g, "account_id", c.req.Str("account_id")); reply != nil || err != nil {
		return reply, err
	}
	m := s.money(c, g, "account_id", "sale_date", "amount", "", "")
	err := s.inTx(ctx, func(q *store.Queries) error {
		_, err := books.SellAsset(ctx, q, g.group.ID, g.user.ID, c.req.Int("id"), m)
		return err
	})
	if be, ok := asBooksError(err); ok {
		if be.Code == "not_found" {
			return map[string]any{"status": 0, "time": s.now().Unix(), "error": "Error. Could not find asset"}, nil
		}
		return status(0, be.Msg), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, "Asset sale Successfully sold"), nil
}

func (s *Server) voidAsset(ctx context.Context, c *call, g *member) (any, error) {
	err := s.inTx(ctx, func(q *store.Queries) error { return books.VoidAsset(ctx, q, g.group.ID, g.user.ID, c.req.Int("id")) })
	if be, ok := asBooksError(err); ok {
		if be.Code == "not_found" {
			return status(0, "Could not find selected group asset to void details."), nil
		}
		return status(0, be.Msg), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, "Successfully voided payment"), nil
}

func (s *Server) assetPurchasePayments(ctx context.Context, c *call, g *member) (any, error) {
	a, err := s.q.GetAsset(ctx, store.GetAssetParams{GroupID: g.group.ID, ID: c.req.Int("id")})
	if err != nil {
		return status(0, "Sorry, the selected asset could not be found. Select a different asset"), nil
	}
	rows, err := s.q.AssetPurchasePayments(ctx, store.AssetPurchasePaymentsParams{GroupID: g.group.ID, AssetID: &a.ID})
	if err != nil {
		return nil, err
	}
	name, err := s.accountNames(ctx, g)
	if err != nil {
		return nil, err
	}
	out := []any{}
	var total int64
	for _, r := range rows {
		total += books.Cents(r.Amount)
		out = append(out, map[string]any{"id": r.ID, "account": name(r.AccountID), "amount": r.Amount,
			"description": r.Description, "date": mobileShortTime(r.WithdrawalDate)})
	}
	return map[string]any{"payments": out, "status": 1, "message": "success", "total_paid_amount": float64(total) / 100}, nil
}

func (s *Server) investmentDeposits(ctx context.Context, c *call, g *member, kind store.DepositKind, def int64) ([]store.InvestmentDepositsRow, func(*int64) string, error) {
	limit, offset := pageOf(c, def)
	rows, err := s.q.InvestmentDeposits(ctx, store.InvestmentDepositsParams{GroupID: g.group.ID, Kind: kind, Lim: limit, Off: offset})
	if err != nil {
		return nil, nil, err
	}
	name, err := s.accountNames(ctx, g)
	return rows, name, err
}

func (s *Server) assetSalesList(ctx context.Context, c *call, g *member) (any, error) {
	rows, name, err := s.investmentDeposits(ctx, c, g, store.DepositKindAssetSale, 20)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		out = append(out, map[string]any{"id": r.ID, "date": mobileShortTime(r.DepositDate), "name": r.AssetName,
			"account": name(r.AccountID), "amount": r.Amount, "description": r.Description})
	}
	return map[string]any{"status": 1, "message": "success", "asset_sales": out}, nil
}

// ---- Stocks ----

func (s *Server) recordStockPurchase(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.checkForm(c, g, rule{"purchase_date", "Purchase Date", "date"}, rule{"name", "Stock Purchase Name", ""},
		rule{"number_of_shares", "Number of Shares", "number"}, rule{"account_id", "Account", ""},
		rule{"price_per_share", "Price per Share", "amount"}); reply != nil {
		return reply, nil
	}
	if reply, err := s.checkAccount(ctx, g, "account_id", c.req.Str("account_id")); reply != nil || err != nil {
		return reply, err
	}
	m := s.money(c, g, "account_id", "purchase_date", "", "", "")
	return s.write(ctx, func(q *store.Queries) error {
		_, err := books.BuyStock(ctx, q, g.group.ID, g.user.ID, c.req.Str("name"), c.req.Str("number_of_shares"),
			appAmount(c.req.Str("price_per_share")), m)
		return err
	}, "Stock Purchase successfully recorded")
}

func (s *Server) updateStockCurrentPrice(ctx context.Context, c *call, g *member) (any, error) {
	price := appAmount(c.req.Str("current_price"))
	err := s.inTx(ctx, func(q *store.Queries) error {
		if price == "" || books.Cents(price) <= 0 {
			return books.Error{Code: "invalid_price", Msg: "Current price is not valid"}
		}
		return books.SetStockPrice(ctx, q, g.group.ID, c.req.Int("id"), price)
	})
	if be, ok := asBooksError(err); ok {
		if be.Code == "not_found" {
			return status(0, "You are trying to alter Stock Price which is currently unavailable"), nil
		}
		return status(0, "Current price is not valid"), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, "Stock current price successfully updated"), nil
}

func (s *Server) sellStocks(ctx context.Context, c *call, g *member) (any, error) {
	if _, err := s.q.LockStock(ctx, store.LockStockParams{GroupID: g.group.ID, ID: c.req.Int("id")}); err != nil {
		return status(0, "You are trying to sell a Stock which is not available"), nil
	}
	if reply := s.checkForm(c, g, rule{"account_id", "Account", ""}, rule{"sale_date", "Stocks Sale Date", "date"},
		rule{"number_of_shares_to_be_sold", "Number of Shares Sold", "number"},
		rule{"sale_price_per_share", "Sale Price per Share", "amount"}); reply != nil {
		return reply, nil
	}
	if reply, err := s.checkAccount(ctx, g, "account_id", c.req.Str("account_id")); reply != nil || err != nil {
		return reply, err
	}
	m := s.money(c, g, "account_id", "sale_date", "", "", "")
	err := s.inTx(ctx, func(q *store.Queries) error {
		_, err := books.SellStock(ctx, q, g.group.ID, g.user.ID, c.req.Int("id"), c.req.Str("number_of_shares_to_be_sold"),
			appAmount(c.req.Str("sale_price_per_share")), m)
		return err
	})
	if be, ok := asBooksError(err); ok {
		switch be.Code {
		case "too_many_shares":
			return s.formFailed(map[string]string{"number_of_shares_to_be_sold": "Number of sold shares cannot exceed available shares"}), nil
		case "not_found":
			return status(0, "You are trying to sell a Stock which is not available"), nil
		}
		return timed(status(0, be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, "Stock sale successfully added"), nil
}

func (s *Server) voidStock(ctx context.Context, c *call, g *member) (any, error) {
	err := s.inTx(ctx, func(q *store.Queries) error { return books.VoidStock(ctx, q, g.group.ID, g.user.ID, c.req.Int("id")) })
	if be, ok := asBooksError(err); ok {
		if be.Code == "not_found" {
			return status(0, "Stock details not found"), nil
		}
		return status(0, be.Msg), nil
	}
	if err != nil {
		return nil, err
	}
	return status(1, "successfully voided stock"), nil
}

func (s *Server) getGroupStocksList(ctx context.Context, c *call, g *member) (any, error) {
	limit, offset := pageOf(c, 100)
	rows, err := s.q.ListStocks(ctx, store.ListStocksParams{GroupID: g.group.ID, Lim: limit, Off: offset})
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		out = append(out, map[string]any{"id": r.ID, "date": mobileShortTime(r.PurchaseDate), "name": r.Name,
			"price": trimZeros(r.PurchasePrice), "shares": trimZeros(r.Shares), "current_price": trimZeros(r.CurrentPrice)})
	}
	return map[string]any{"status": 1, "message": "success", "group_stocks": out}, nil
}

func (s *Server) getGroupStockSalesList(ctx context.Context, c *call, g *member) (any, error) {
	rows, name, err := s.investmentDeposits(ctx, c, g, store.DepositKindStockSale, 100)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		out = append(out, map[string]any{"id": r.ID, "date": mobileShortTime(r.DepositDate), "name": r.StockName,
			"account": name(r.AccountID), "shares_sold": trimZeros(r.SharesSold), "per_share_sale_price": trimZeros(r.PricePerShare)})
	}
	return map[string]any{"status": 1, "message": "success", "stock_sales": out}, nil
}

// trimZeros drops trailing decimal zeros: "100.0000" -> "100", "2.5000" -> "2.5".
func trimZeros(v string) string {
	if !strings.Contains(v, ".") {
		return v
	}
	v = strings.TrimRight(v, "0")
	return strings.TrimSuffix(v, ".")
}

// ---- Money market investments ----

func (s *Server) createMoneyMarketInvestment(ctx context.Context, c *call, g *member) (any, error) {
	if reply := s.checkForm(c, g, rule{"investment_institution_name", "Investment Institution Name", ""},
		rule{"investment_date", "Investment Date", "date"}, rule{"investment_amount", "Investment Amount", "amount"},
		rule{"withdrawal_account_id", "Account", ""}); reply != nil {
		return reply, nil
	}
	if reply, err := s.checkAccount(ctx, g, "withdrawal_account_id", c.req.Str("withdrawal_account_id")); reply != nil || err != nil {
		return reply, err
	}
	m := s.money(c, g, "withdrawal_account_id", "investment_date", "investment_amount", "description", "")
	return s.write(ctx, func(q *store.Queries) error {
		_, err := books.Invest(ctx, q, g.group.ID, g.user.ID, c.req.Str("investment_institution_name"), m)
		return err
	}, "Money market investment successfully added")
}

func (s *Server) investmentExists(ctx context.Context, g *member, id int64) bool {
	_, err := s.q.LockMoneyMarketInvestment(ctx, store.LockMoneyMarketInvestmentParams{GroupID: g.group.ID, ID: id})
	return err == nil
}

func (s *Server) topUpMoneyMarketInvestment(ctx context.Context, c *call, g *member) (any, error) {
	if !s.investmentExists(ctx, g, c.req.Int("id")) {
		return status(0, "Money market investment to topup not found"), nil
	}
	if reply := s.checkForm(c, g, rule{"top_up_date", "Top Up Date", "date"}, rule{"top_up_amount", "Top Up Amount", "amount"},
		rule{"top_up_withdrawal_account_id", "Account", ""}); reply != nil {
		return reply, nil
	}
	if reply, err := s.checkAccount(ctx, g, "top_up_withdrawal_account_id", c.req.Str("top_up_withdrawal_account_id")); reply != nil || err != nil {
		return reply, err
	}
	m := s.money(c, g, "top_up_withdrawal_account_id", "top_up_date", "top_up_amount", "top_up_description", "")
	return s.write(ctx, func(q *store.Queries) error {
		return books.TopUpInvestment(ctx, q, g.group.ID, g.user.ID, c.req.Int("id"), m)
	}, "Money market investment topup successfully added")
}

func (s *Server) cashInMoneyMarketInvestment(ctx context.Context, c *call, g *member) (any, error) {
	if !s.investmentExists(ctx, g, c.req.Int("id")) {
		return status(0, "Money market investment to Cashin not found"), nil
	}
	if reply := s.checkForm(c, g, rule{"cash_in_date", "Cash in Date", "date"}, rule{"cash_in_amount", "Cash In Amount", "amount"},
		rule{"cash_in_deposit_account_id", "Account", ""}); reply != nil {
		return reply, nil
	}
	if reply, err := s.checkAccount(ctx, g, "cash_in_deposit_account_id", c.req.Str("cash_in_deposit_account_id")); reply != nil || err != nil {
		return reply, err
	}
	m := s.money(c, g, "cash_in_deposit_account_id", "cash_in_date", "cash_in_amount", "", "")
	return s.write(ctx, func(q *store.Queries) error {
		return books.CashIn(ctx, q, g.group.ID, g.user.ID, c.req.Int("id"), m)
	}, "Money market investment Cashin successfully added")
}

func (s *Server) voidMoneyMarketInvestment(ctx context.Context, c *call, g *member) (any, error) {
	err := s.inTx(ctx, func(q *store.Queries) error {
		return books.VoidInvestment(ctx, q, g.group.ID, g.user.ID, c.req.Int("id"))
	})
	if be, ok := asBooksError(err); ok {
		if be.Code == "not_found" {
			return timed(status(0, "Money Market Investment details are missing"), s.now()), nil
		}
		return timed(status(0, be.Msg), s.now()), nil
	}
	if err != nil {
		return nil, err
	}
	return timed(status(1, "successfully voided money market investment"), s.now()), nil
}

func (s *Server) getMoneyMarketInvestmentList(ctx context.Context, c *call, g *member) (any, error) {
	limit, offset := pageOf(c, 100)
	rows, err := s.q.ListMoneyMarketInvestments(ctx, store.ListMoneyMarketInvestmentsParams{GroupID: g.group.ID, Lim: limit, Off: offset})
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		desc := ""
		if r.Description != "" {
			desc = ": " + r.Description
		}
		out = append(out, map[string]any{"id": r.ID, "date": mobileShortTime(r.InvestmentDate),
			"description": r.Institution + " " + desc, "is_closed": boolInt(r.IsClosed), "amount": r.Amount,
			"investment_institution_name": r.Institution})
	}
	return map[string]any{"status": 1, "message": "success", "investments": out}, nil
}

func (s *Server) getMoneyMarketInvestmentCashins(ctx context.Context, c *call, g *member) (any, error) {
	rows, name, err := s.investmentDeposits(ctx, c, g, store.DepositKindMoneyMarket, 100)
	if err != nil {
		return nil, err
	}
	out := []any{}
	for _, r := range rows {
		label := ""
		if r.Institution != "" {
			label = fmt.Sprintf("%s ( %s %s invested on %s)", r.Institution, g.group.Currency, commaMoney(r.Invested),
				shortDate(*r.InvestedOn))
		}
		out = append(out, map[string]any{"id": r.ID, "date": mobileShortTime(r.DepositDate), "name": label,
			"account": name(r.AccountID), "amount": r.Amount})
	}
	return map[string]any{"status": 1, "message": "success", "cashins": out}, nil
}
