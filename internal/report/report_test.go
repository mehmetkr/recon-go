package report

import (
	"bytes"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/match"
	"github.com/mehmetkr/recon-go/internal/store"
)

var cfg = store.Config{
	AlgoVersion: 1, TZ: "UTC",
	BankLayouts: []string{"2006-01-02"}, LedgerLayouts: []string{"2006-01-02"},
	DateTolerance: 3, FuzzyWindow: 7, MinRefLen: 5, ThresholdNum: 4, ThresholdDen: 5,
}

func TestEncode(t *testing.T) {
	in := Input{
		RunID: "r", Config: cfg,
		// Unreadable rows and unexplained records may arrive without lists.
		Malformed: []domain.RowError{{Source: domain.Bank, Line: 2, Reason: domain.RowCSVParse}},
		Result:    match.Result{Exceptions: []domain.Exception{{Source: domain.Bank, ID: "x", Reason: domain.ReasonNoAmountMatch}}},
	}
	b, err := Encode(Build(in))
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := Encode(Build(in)); !bytes.Equal(b, again) {
		t.Error("encoding the same report twice gave different bytes")
	}
	if bytes.Contains(b, []byte("null")) || !bytes.HasSuffix(b, []byte("}\n")) ||
		!bytes.Contains(b, []byte(`"raw": []`)) || !bytes.Contains(b, []byte(`"candidates": []`)) {
		t.Errorf("every list must be present and the file must end with a newline:\n%s", b)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatal(err)
	}
	var keys []string
	for k := range got {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if want := []string{"config", "exceptions", "excluded", "malformed", "matches", "run_id", "summary"}; !slices.Equal(keys, want) {
		t.Errorf("top-level keys = %v, want %v", keys, want)
	}
	zeros := map[string]any{"bank": 0.0, "ledger": 0.0}
	if diff := cmp.Diff(zeros, got["excluded"]); diff != "" || fmt.Sprint(got["matches"]) != "[]" {
		t.Errorf("empty parts must still appear: excluded %v, matches %v", got["excluded"], got["matches"])
	}
	conf := got["config"].(map[string]any)
	if conf["config_hash"] != store.ConfigHash(cfg) || conf["tz"] != "UTC" || conf["date_tolerance"] != 3.0 || len(conf) != 10 {
		t.Errorf("config = %v, want exactly the settings and their fingerprint", conf)
	}
	summary := got["summary"].(map[string]any)
	reasons := map[string]any{"ambiguous": 0.0, "counterpart_taken": 0.0, "no_rule_match": 0.0, "no_amount_match": 0.0}
	withOne := map[string]any{"ambiguous": 0.0, "counterpart_taken": 0.0, "no_rule_match": 0.0, "no_amount_match": 1.0}
	want := map[string]any{
		"read": zeros, "malformed": map[string]any{"bank": 1.0, "ledger": 0.0}, "excluded": zeros,
		"matches":    map[string]any{"exact": 0.0, "date_tolerance": 0.0, "fuzzy_reference": 0.0},
		"exceptions": map[string]any{"bank": withOne, "ledger": reasons},
	}
	if diff := cmp.Diff(want, summary); diff != "" {
		t.Errorf("summary (-want +got):\n%s", diff)
	}
}

func TestSummary(t *testing.T) {
	exc := func(src domain.Source, id string, r domain.Reason) domain.Exception {
		return domain.Exception{Source: src, ID: id, Reason: r}
	}
	r := Build(Input{
		Config:   cfg,
		Read:     map[domain.Source]int{domain.Bank: 8, domain.Ledger: 4},
		Excluded: map[domain.Source]int{domain.Bank: 2},
		Result: match.Result{
			Matches: []domain.Match{{BankID: "b0", LedgerID: "l0", Rule: domain.RuleFuzzyReference}, {BankID: "b5", LedgerID: "l5", Rule: domain.RuleExact}},
			Exceptions: []domain.Exception{
				exc(domain.Bank, "b1", domain.ReasonNoAmountMatch), exc(domain.Bank, "b2", domain.ReasonNoRuleMatch),
				exc(domain.Bank, "b3", domain.ReasonAmbiguous), exc(domain.Bank, "b4", domain.ReasonAmbiguous),
				exc(domain.Ledger, "l1", domain.ReasonCounterpartTaken), exc(domain.Ledger, "l2", domain.ReasonNoRuleMatch),
			},
		},
		Malformed: []domain.RowError{{Source: domain.Ledger}, {Source: domain.Ledger}, {Source: domain.Bank}},
	})
	// Every record is accounted for: read = excluded + matched + exceptions.
	want := Summary{
		Read:     map[domain.Source]int{domain.Bank: 8, domain.Ledger: 4},
		Matches:  map[domain.Rule]int{domain.RuleExact: 1, domain.RuleDateTolerance: 0, domain.RuleFuzzyReference: 1},
		Excluded: map[domain.Source]int{domain.Bank: 2, domain.Ledger: 0},
		Exceptions: map[domain.Source]map[domain.Reason]int{
			domain.Bank:   {domain.ReasonAmbiguous: 2, domain.ReasonCounterpartTaken: 0, domain.ReasonNoRuleMatch: 1, domain.ReasonNoAmountMatch: 1},
			domain.Ledger: {domain.ReasonAmbiguous: 0, domain.ReasonCounterpartTaken: 1, domain.ReasonNoRuleMatch: 1, domain.ReasonNoAmountMatch: 0},
		},
		Malformed: map[domain.Source]int{domain.Bank: 1, domain.Ledger: 2},
	}
	if diff := cmp.Diff(want, r.Summary); diff != "" {
		t.Errorf("summary (-want +got):\n%s", diff)
	}
	if diff := cmp.Diff(want.Excluded, r.Excluded); diff != "" {
		t.Errorf("excluded (-want +got):\n%s", diff)
	}
}

func TestMalformedOrder(t *testing.T) {
	// Each neighbouring pair of sort keys disagrees, so any swap would show.
	b, l := domain.Bank, domain.Ledger
	rows := []domain.RowError{
		{Source: l, Raw: []string{"a"}, Reason: domain.RowBadAccount, Detail: "a", Line: 1}, // side first
		{Source: b, Raw: []string{"b"}, Reason: domain.RowBadAmount, Detail: "a", Line: 1},  // row text before reason
		{Source: b, Raw: []string{"a"}, Reason: domain.RowFieldCount, Detail: "a", Line: 1},
		{Source: b, Raw: []string{"c"}, Reason: domain.RowBadDate, Detail: "z", Line: 1}, // reason before detail
		{Source: b, Raw: []string{"c"}, Reason: domain.RowCSVParse, Detail: "y", Line: 9},
		{Source: b, Raw: []string{"d"}, Reason: domain.RowBadDate, Detail: "b", Line: 1}, // detail before line
		{Source: b, Raw: []string{"d"}, Reason: domain.RowBadDate, Detail: "a", Line: 9},
		{Source: b, Raw: []string{"d"}, Reason: domain.RowBadDate, Detail: "a", Line: 3}, // line last
		{Source: b, Raw: nil, Reason: domain.RowCSVParse, Line: 5},                       // no row text sorts first
	}
	var got []string
	for _, m := range Build(Input{Config: cfg, Malformed: rows}).Malformed {
		got = append(got, fmt.Sprintf("%s/%v/%s/%s/%d", m.Source, m.Raw, m.Reason, m.Detail, m.Line))
	}
	want := []string{"bank/[]/csv_parse//5", "bank/[a]/field_count/a/1", "bank/[b]/bad_amount/a/1", "bank/[c]/bad_date/z/1",
		"bank/[c]/csv_parse/y/9", "bank/[d]/bad_date/a/3", "bank/[d]/bad_date/a/9", "bank/[d]/bad_date/b/1", "ledger/[a]/bad_account/a/1"}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("(-want +got):\n%s", diff)
	}
	// Many rows equal in every other way are ordered by line.
	rows = rows[:0]
	for _, line := range []int{9, 4, 17, 2, 30, 11, 6, 25, 3, 14, 8, 21, 5, 19} {
		rows = append(rows, domain.RowError{Source: b, Line: line, Reason: domain.RowFieldCount, Raw: []string{"broken"}})
	}
	var lines []int
	for _, m := range Build(Input{Config: cfg, Malformed: rows}).Malformed {
		lines = append(lines, m.Line)
	}
	if !slices.IsSorted(lines) {
		t.Errorf("lines = %v, want ascending", lines)
	}
}

func TestBuildDoesNotModifyItsInput(t *testing.T) {
	malformed := []domain.RowError{{Source: domain.Ledger, Raw: nil}, {Source: domain.Bank}}
	exceptions := []domain.Exception{{Source: domain.Bank, ID: "x"}}
	Build(Input{Config: cfg, Malformed: malformed, Result: match.Result{Exceptions: exceptions}})
	if malformed[0].Source != domain.Ledger || malformed[0].Raw != nil || exceptions[0].Candidates != nil {
		t.Error("Build changed the caller's lists")
	}
}
