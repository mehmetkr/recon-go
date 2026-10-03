package domain

import (
	"encoding/json"
	"slices"
	"testing"
	"time"
)

func zone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

func TestDateOf(t *testing.T) {
	ny, tokyo := zone(t, "America/New_York"), zone(t, "Asia/Tokyo")
	for _, tt := range []struct {
		in   time.Time
		want string
	}{
		{time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC), "1970-01-01"},
		{time.Date(1969, 12, 31, 23, 59, 0, 0, time.UTC), "1969-12-31"},
		{time.Date(2024, 2, 29, 12, 0, 0, 0, time.UTC), "2024-02-29"},
		{time.Date(2024, 1, 15, 23, 30, 0, 0, ny), "2024-01-15"},
		{time.Date(2024, 1, 15, 0, 30, 0, 0, tokyo), "2024-01-15"},
	} {
		if got := DateOf(tt.in).String(); got != tt.want {
			t.Errorf("DateOf(%v) = %s, want %s", tt.in, got, tt.want)
		}
	}
	// The clocks jump forward this weekend, yet the dates stay two days apart.
	if d := DateOf(time.Date(2026, 3, 9, 0, 0, 0, 0, ny)) - DateOf(time.Date(2026, 3, 7, 0, 0, 0, 0, ny)); d != 2 {
		t.Errorf("days across the clock change = %d, want 2", d)
	}
}

func TestDateJSON(t *testing.T) {
	d, err := ParseDate("2026-09-29")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(d)
	var back Date
	if string(b) != `"2026-09-29"` || json.Unmarshal(b, &back) != nil || back != d {
		t.Errorf("round trip: %s -> %v, want %v", b, back, d)
	}
	if json.Unmarshal([]byte(`"2026-02-30"`), &back) == nil {
		t.Error("an invalid date was accepted")
	}
	if json.Unmarshal([]byte(`123`), &back) == nil {
		t.Error("a non-string JSON value was accepted")
	}
}

func TestJSONShapes(t *testing.T) {
	for _, tt := range []struct {
		v    any
		want string
	}{
		{Match{MatchID: "m", BankID: "b", LedgerID: "l", Rule: RuleDateTolerance, DayDelta: -2},
			`{"match_id":"m","bank_id":"b","ledger_id":"l","rule":"date_tolerance","day_delta":-2}`},
		{Exception{Source: Ledger, ID: "x", Reason: ReasonCounterpartTaken, Candidates: []string{"a"}},
			`{"source":"ledger","id":"x","reason":"counterpart_taken","candidates":["a"]}`},
		{RowError{Source: Bank, Line: 3, Reason: RowCSVParse, Detail: "d", Raw: []string{"r"}},
			`{"source":"bank","line":3,"reason":"csv_parse","detail":"d","raw":["r"]}`},
		// Empty lists still appear in the output.
		{RowError{Source: Bank, Line: 6, Reason: RowCSVParse, Raw: []string{}},
			`{"source":"bank","line":6,"reason":"csv_parse","detail":"","raw":[]}`},
		{Exception{Source: Bank, ID: "x", Reason: ReasonNoAmountMatch, Candidates: []string{}},
			`{"source":"bank","id":"x","reason":"no_amount_match","candidates":[]}`},
	} {
		if b, _ := json.Marshal(tt.v); string(b) != tt.want {
			t.Errorf("got  %s\nwant %s", b, tt.want)
		}
	}
}

func TestCodeValues(t *testing.T) {
	// These words appear in the output files, so they must never drift.
	got := []string{
		string(Bank), string(Ledger),
		string(RuleExact), string(RuleDateTolerance), string(RuleFuzzyReference),
		string(ReasonAmbiguous), string(ReasonCounterpartTaken), string(ReasonNoRuleMatch), string(ReasonNoAmountMatch),
		string(RowCSVParse), string(RowFieldCount), string(RowBadAccount), string(RowBadDate), string(RowBadAmount), string(RowBadCurrency),
	}
	want := []string{
		"bank", "ledger",
		"exact", "date_tolerance", "fuzzy_reference",
		"ambiguous", "counterpart_taken", "no_rule_match", "no_amount_match",
		"csv_parse", "field_count", "bad_account", "bad_date", "bad_amount", "bad_currency",
	}
	if !slices.Equal(got, want) {
		t.Errorf("got  %v\nwant %v", got, want)
	}
}

func TestContentKeyUsesEveryField(t *testing.T) {
	base := Transaction{Account: "A", Currency: "EUR", Date: 1, Amount: 100, RefNorm: "R"}
	for i, change := range []func(*Transaction){
		func(t *Transaction) { t.Account = "B" },
		func(t *Transaction) { t.Currency = "USD" },
		func(t *Transaction) { t.Date = 2 },
		func(t *Transaction) { t.Amount = 200 },
		func(t *Transaction) { t.RefNorm = "S" },
	} {
		v := base
		change(&v)
		if v.ContentKey() == base.ContentKey() {
			t.Errorf("change %d left the content key unchanged", i)
		}
	}
}

func TestDigest(t *testing.T) {
	// Computed independently from the published formula.
	if got := MatchID("a", "b"); got != "facdde7abf1eac5b301273ab2e282f79" {
		t.Errorf("MatchID(a, b) = %s", got)
	}
	if Digest(32, "A|B", "C") == Digest(32, "A", "B|C") || Digest(32, "ab", "") == Digest(32, "a", "b") {
		t.Error("different field lists share a fingerprint")
	}
	if n := len(Digest(32, "x")); n != 32 {
		t.Errorf("length = %d, want 32", n)
	}
}
