// Package domain holds the shared vocabulary of the reconciliation.
package domain

// Source tells which side of the reconciliation a record comes from.
type Source string

const (
	Bank   Source = "bank"
	Ledger Source = "ledger"
)

// Transaction is one cleaned-up record, with money counted as cash in or out.
type Transaction struct {
	ID         string
	Source     Source
	Account    string
	Currency   string
	Date       Date
	Amount     int64
	Reference  string // as written in the file
	RefNorm    string // the reference reduced to letters and digits
	Occurrence int    // which copy this is among identical rows
	Line       int
	Raw        []string
}

// ContentKey is what a transaction says, whatever file it came from.
type ContentKey struct {
	Account  string
	Currency string
	Date     Date
	Amount   int64
	RefNorm  string
}

// ContentKey returns what a transaction says, independent of which file it came from.
func (t Transaction) ContentKey() ContentKey {
	return ContentKey{t.Account, t.Currency, t.Date, t.Amount, t.RefNorm}
}

// PartitionKey groups the records that could ever match each other.
type PartitionKey struct {
	Account  string
	Currency string
	Amount   int64
}

// PartitionKey returns the group this record belongs to for matching.
func (t Transaction) PartitionKey() PartitionKey {
	return PartitionKey{t.Account, t.Currency, t.Amount}
}

// Rule names the matching step that paired two records.
type Rule string

const (
	RuleExact          Rule = "exact"
	RuleDateTolerance  Rule = "date_tolerance"
	RuleFuzzyReference Rule = "fuzzy_reference"
)

// Rules lists the matching steps in the order they run.
var Rules = []Rule{RuleExact, RuleDateTolerance, RuleFuzzyReference}

// Match pairs one bank record with one ledger record.
type Match struct {
	MatchID  string `json:"match_id"`
	BankID   string `json:"bank_id"`
	LedgerID string `json:"ledger_id"`
	Rule     Rule   `json:"rule"`
	DayDelta int    `json:"day_delta"` // days between the bank and ledger dates
}

// Reason explains why a record was left unmatched.
type Reason string

const (
	ReasonAmbiguous        Reason = "ambiguous"
	ReasonCounterpartTaken Reason = "counterpart_taken"
	ReasonNoRuleMatch      Reason = "no_rule_match"
	ReasonNoAmountMatch    Reason = "no_amount_match"
)

// Reasons lists the explanations from most to least specific.
var Reasons = []Reason{ReasonAmbiguous, ReasonCounterpartTaken, ReasonNoRuleMatch, ReasonNoAmountMatch}

// Exception is an unmatched record and the reason it stayed that way.
type Exception struct {
	Source     Source   `json:"source"`
	ID         string   `json:"id"`
	Reason     Reason   `json:"reason"`
	Candidates []string `json:"candidates"`
}

// RowErrorReason says what is wrong with an unreadable row.
type RowErrorReason string

const (
	RowCSVParse    RowErrorReason = "csv_parse"
	RowFieldCount  RowErrorReason = "field_count"
	RowBadAccount  RowErrorReason = "bad_account"
	RowBadDate     RowErrorReason = "bad_date"
	RowBadAmount   RowErrorReason = "bad_amount"
	RowBadCurrency RowErrorReason = "bad_currency"
)

// RowError is an unreadable row, always reported and never silently dropped.
type RowError struct {
	Source Source         `json:"source"`
	Line   int            `json:"line"`
	Reason RowErrorReason `json:"reason"`
	Detail string         `json:"detail"`
	Raw    []string       `json:"raw"`
}
