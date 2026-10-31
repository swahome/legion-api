package mobile

import (
	"math"
	"strconv"
	"strings"

	"github.com/swahome/legion-api/internal/store"
)

// Legacy money codes (libraries/Transactions.php). The legacy type number
// encodes both what a deposit or withdrawal was for and the kind of account,
// in blocks of four (bank, sacco, mobile money, petty cash); contributions
// and fines are the exception (1, 2, 3, 7 and 4, 5, 6, 8).

var accountKindOffset = map[store.AccountKind]int{"bank": 0, "sacco": 1, "mobile_money": 2, "petty_cash": 3}

var depositBlocks = map[store.DepositKind]int{
	"misc": 9, "income": 13, "loan_repayment": 17, "bank_loan": 21, "stock_sale": 25,
	"money_market": 29, "asset_sale": 33, "funds_transfer": 37, "loan_processing_income": 41,
	"external_lending_processing_income": 45, "external_lending_repayment": 49,
}

// depositType is a deposit's legacy type number.
func depositType(legacy *int16, kind store.DepositKind, account store.AccountKind) int {
	if legacy != nil && *legacy > 0 {
		return int(*legacy)
	}
	off := accountKindOffset[account]
	switch kind {
	case "contribution":
		return []int{1, 2, 3, 7}[off]
	case "fine":
		return []int{4, 5, 6, 8}[off]
	}
	return depositBlocks[kind] + off
}

var withdrawalBlocks = map[store.WithdrawalKind]int{
	"expense": 1, "asset_purchase": 5, "loan_disbursement": 9, "stock_purchase": 13,
	"money_market_investment": 17, "contribution_refund": 21, "bank_loan_repayment": 25,
	"funds_transfer": 29, "external_loan_disbursement": 33, "dividend": 37,
}

func withdrawalType(legacy *int16, kind store.WithdrawalKind, account store.AccountKind) int {
	if legacy != nil && *legacy > 0 {
		return int(*legacy)
	}
	return withdrawalBlocks[kind] + accountKindOffset[account]
}

// depositNames is deposit_transaction_names, by block.
var depositNames = map[store.DepositKind]string{
	"contribution": "Contribution payment", "fine": "Fine payment", "misc": "Miscellaneous payment",
	"income": "Income ", "loan_repayment": "Loan Repayment ", "bank_loan": "Bank Loan Disbursement ",
	"stock_sale": "Stock Sale ", "money_market": "Money Market Investment Cash in ", "asset_sale": "Asset Sale ",
	"funds_transfer": "Funds Transfer ", "loan_processing_income": "Loan Processing Income",
	"external_lending_processing_income": "External Lending Processing Income",
	"external_lending_repayment":         "External Lending Loan Repayment ",
}

// withdrawalNames is withdrawal_transaction_names, by block.
var withdrawalNames = map[store.WithdrawalKind]string{
	"expense": "Expense", "asset_purchase": "Asset Purchase Payment", "loan_disbursement": "Loan Disbursement",
	"stock_purchase": "Stock Purchase", "money_market_investment": "Money Market Investment",
	"contribution_refund": "Contribution Refund", "bank_loan_repayment": "Bank Loan Repayment",
	"funds_transfer": "Funds Transfer", "external_loan_disbursement": "External Lending",
	"dividend": "Dividend",
}

// depositMethodNames is deposit_method_options; withdrawalMethodNames is withdrawal_method_options.
var depositMethodNames = map[store.PaymentMethod]string{
	"mpesa": "MPesa", "cash": "Cash", "cheque": "Cheque", "funds_transfer": "Fund Transfer",
	"equitel": "Equitel", "standing_order": "Standing Order",
}

var withdrawalMethodNames = map[store.PaymentMethod]string{
	"cash": "Cash", "mpesa": "Mpesa", "cheque": "Cheque", "funds_transfer": "Account to Account Transfer", "equitel": "Equitel",
}

// The app filters deposit and withdrawal lists by these "status" numbers
// (deposit_pairing and withdrawal_pairing in the legacy controllers).
var depositFilters = map[string]store.DepositKind{
	"1": "contribution", "2": "fine", "3": "loan_repayment", "4": "misc", "5": "income", "6": "bank_loan",
	"7": "stock_sale", "8": "money_market", "9": "asset_sale", "10": "funds_transfer",
	"11": "loan_processing_income", "12": "external_lending_processing_income", "13": "external_lending_repayment",
}

var withdrawalFilters = map[string]store.WithdrawalKind{
	"1": "expense", "2": "asset_purchase", "3": "money_market_investment", "4": "loan_disbursement",
	"5": "stock_purchase", "6": "contribution_refund", "7": "bank_loan_repayment", "8": "funds_transfer",
	"9": "external_loan_disbursement",
}

// accountPrefix is how the app names an account: bank-12, sacco-3, mobile-7, petty-2.
var accountPrefix = map[store.AccountKind]string{"bank": "bank", "sacco": "sacco", "mobile_money": "mobile", "petty_cash": "petty"}

// parseAccountID reads the app's "bank-12" style account id.
func parseAccountID(v string) (store.AccountKind, int64, bool) {
	for kind, p := range accountPrefix {
		if rest, ok := strings.CutPrefix(v, p+"-"); ok {
			id, err := strconv.ParseInt(rest, 10, 64)
			return kind, id, err == nil && id > 0
		}
	}
	return "", 0, false
}

var smallNumbers = []string{"zero", "one", "two", "three", "four", "five", "six", "seven", "eight", "nine", "ten",
	"eleven", "twelve", "thirteen", "fourteen", "fifteen", "sixteen", "seventeen", "eighteen", "nineteen", "twenty"}

var tensNames = map[int64]string{20: "twenty", 30: "thirty", 40: "fourty", 50: "fifty", 60: "sixty", 70: "seventy", 80: "eighty", 90: "ninety"}

var scaleNames = []string{"", "thousand", "million", "billion", "trillion"}

// numberToWords is the legacy number_to_words, quirks included ("fourty",
// "and" before a remainder under a hundred, ", " otherwise, "point five zero").
func numberToWords(amount string) string {
	f, err := strconv.ParseFloat(amount, 64)
	if err != nil {
		return ""
	}
	neg := f < 0
	f = math.Abs(f)
	whole := int64(f)
	cents := int64(math.Round((f - float64(whole)) * 100))
	s := intWords(whole)
	if cents > 0 {
		frac := strconv.FormatFloat(float64(cents)/100, 'f', -1, 64)[2:]
		words := make([]string, 0, len(frac))
		for _, d := range frac {
			words = append(words, smallNumbers[d-'0'])
		}
		s += " point " + strings.Join(words, " ")
	}
	if neg {
		return "negative " + s
	}
	return s
}

func intWords(n int64) string {
	switch {
	case n < 21:
		return smallNumbers[n]
	case n < 100:
		s := tensNames[n/10*10]
		if n%10 != 0 {
			s += "-" + smallNumbers[n%10]
		}
		return s
	case n < 1000:
		s := smallNumbers[n/100] + " hundred"
		if n%100 != 0 {
			s += " and " + intWords(n%100)
		}
		return s
	}
	base, scale := int64(1), 0
	for base*1000 <= n && scale < len(scaleNames)-1 {
		base *= 1000
		scale++
	}
	s := intWords(n/base) + " " + scaleNames[scale]
	if r := n % base; r != 0 {
		if r < 100 {
			s += " and "
		} else {
			s += ", "
		}
		s += intWords(r)
	}
	return s
}

// inWords is the receipts' "Two Thousand, Five Hundred KESs only".
func inWords(amount, currency string) string {
	return ucwords(numberToWords(amount)) + " " + currency + "s only"
}

// ucwords upper-cases the first letter of each space-separated word, as PHP does.
func ucwords(s string) string {
	b := []byte(s)
	for i := range b {
		if (i == 0 || b[i-1] == ' ') && b[i] >= 'a' && b[i] <= 'z' {
			b[i] -= 'a' - 'A'
		}
	}
	return string(b)
}
