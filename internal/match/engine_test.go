package match

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"

	"github.com/google/go-cmp/cmp"

	"github.com/mehmetkr/recon-go/internal/domain"
)

var defaults = Params{DateTolerance: 3, FuzzyWindow: 7}

const day0 = domain.Date(20000) // 2024-10-04

// rec is a compact test record with sensible defaults.
type rec struct {
	id     string
	day    int
	ref    string
	occ    int
	amount int64
	acct   string
}

// rawRef writes a reference the way a bank might, so tests catch any use of the raw text.
func rawRef(ref string) string {
	if ref == "" {
		return ""
	}
	h := len(ref) / 2
	return strings.ToLower(ref[:h]) + "-" + strings.ToLower(ref[h:])
}

// build creates test records, numbering lines backwards to catch any reliance on file order.
func build(src domain.Source, rs ...rec) []domain.Transaction {
	out := make([]domain.Transaction, 0, len(rs))
	for i, r := range rs {
		t := domain.Transaction{
			ID: r.id, Source: src, Account: cmpOr(r.acct, "ACC-1"), Currency: "EUR",
			Date: day0 + domain.Date(r.day), Amount: cmpOr(r.amount, 100),
			Reference: rawRef(r.ref), RefNorm: r.ref, Occurrence: r.occ, Line: 1000 - i,
		}
		t.Raw = rawRow(t)
		out = append(out, t)
	}
	return out
}

func cmpOr[T comparable](v, fallback T) T {
	var zero T
	if v == zero {
		return fallback
	}
	return v
}

// rawRow writes a realistic CSV row for a test record.
func rawRow(t domain.Transaction) []string {
	amount := fmt.Sprintf("%d.%02d", max(t.Amount, -t.Amount)/100, max(t.Amount, -t.Amount)%100)
	ref := t.Reference + strings.Repeat(" ", t.Occurrence)
	if t.Source == domain.Bank {
		if t.Amount < 0 {
			amount = "-" + amount
		}
		return []string{t.Account, t.Date.String(), amount, t.Currency, ref}
	}
	debit, credit := amount, ""
	if t.Amount < 0 {
		debit, credit = "", amount
	}
	return []string{t.Account, t.Date.String(), debit, credit, t.Currency, ref}
}

func banks(rs ...rec) []domain.Transaction   { return build(domain.Bank, rs...) }
func ledgers(rs ...rec) []domain.Transaction { return build(domain.Ledger, rs...) }

// summarize renders a result as short lines: "bank-ledger rule days" and "id reason candidates".
func summarize(t *testing.T, r Result) []string {
	t.Helper()
	var out []string
	for _, m := range r.Matches {
		if m.MatchID != domain.MatchID(m.BankID, m.LedgerID) {
			t.Errorf("match %s-%s has identity %s", m.BankID, m.LedgerID, m.MatchID)
		}
		out = append(out, fmt.Sprintf("%s-%s %s %d", m.BankID, m.LedgerID, m.Rule, m.DayDelta))
	}
	for _, e := range r.Exceptions {
		if e.Candidates == nil {
			t.Errorf("exception %s has no candidate list", e.ID)
		}
		out = append(out, strings.TrimSpace(fmt.Sprintf("%s %s %s", e.ID, e.Reason, strings.Join(e.Candidates, ","))))
	}
	return out
}

type scenario struct {
	name                string
	bank, ledger, taken []domain.Transaction
	p                   Params // defaults when zero
	want                []string
}

func run(t *testing.T, scenarios []scenario) {
	t.Helper()
	for _, sc := range scenarios {
		t.Run(sc.name, func(t *testing.T) {
			if diff := cmp.Diff(sc.want, summarize(t, Reconcile(sc.bank, sc.ledger, sc.taken, cmpOr(sc.p, defaults)))); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

func TestExactPassEligibility(t *testing.T) {
	var scenarios []scenario
	for _, tt := range []struct {
		bref, lref string
		rule       domain.Rule
	}{
		{"INV1", "INV1", domain.RuleDateTolerance},
		{"ABCD", "ABCD", domain.RuleDateTolerance},
		{"INV1", "INV10001", domain.RuleDateTolerance},
		{"ABCDEFGHIJ", "ZZZZZZZZ", domain.RuleDateTolerance},
		{"INV10023", "XINV10023Y", domain.RuleDateTolerance},
		{"INV10023", "INV10X23", domain.RuleDateTolerance},
		{"INV10001", "INV10001", domain.RuleExact},
		{"ABCDE", "ABCDE", domain.RuleExact},
	} {
		scenarios = append(scenarios, scenario{name: tt.bref + " vs " + tt.lref,
			bank: banks(rec{id: "B1", ref: tt.bref}), ledger: ledgers(rec{id: "L1", ref: tt.lref}),
			want: []string{fmt.Sprintf("B1-L1 %s 0", tt.rule)}})
	}
	run(t, scenarios)
}

func TestScenarios(t *testing.T) {
	run(t, []scenario{
		// Unrelated references, so a later step cannot change the outcome.
		{name: "date tolerance is inclusive on both sides",
			bank: banks(rec{id: "B1", day: 3, ref: "AAAAAAAA"}, rec{id: "B2", day: 4, ref: "CCCCCCCC", amount: 200},
				rec{id: "B3", day: -3, ref: "EEEEEEEE", amount: 300}, rec{id: "B4", day: -4, ref: "GGGGGGGG", amount: 400}),
			ledger: ledgers(rec{id: "L1", ref: "BBBBBBBB"}, rec{id: "L2", ref: "DDDDDDDD", amount: 200},
				rec{id: "L3", ref: "FFFFFFFF", amount: 300}, rec{id: "L4", ref: "HHHHHHHH", amount: 400}),
			want: []string{"B1-L1 date_tolerance 3", "B3-L3 date_tolerance -3",
				"B2 no_rule_match", "B4 no_rule_match", "L2 no_rule_match", "L4 no_rule_match"}},
		{name: "account, currency and amount must agree",
			bank:   banks(rec{id: "B1", amount: 100}, rec{id: "B2", acct: "ACC-2"}),
			ledger: ledgers(rec{id: "L1", amount: -100}, rec{id: "L2", acct: "ACC-3"}),
			want:   []string{"B1 no_amount_match", "B2 no_amount_match", "L1 no_amount_match", "L2 no_amount_match"}},
		{name: "the closer claim wins", bank: banks(rec{id: "B1"}, rec{id: "B2", day: 2}), ledger: ledgers(rec{id: "L1"}),
			want: []string{"B1-L1 date_tolerance 0", "B2 counterpart_taken L1"}},
		// A chain in which each record has a clearly best partner.
		{name: "chain", bank: banks(rec{id: "B1"}, rec{id: "B2", day: 1}), ledger: ledgers(rec{id: "L1"}, rec{id: "L2", day: -1}),
			want: []string{"B1-L1 date_tolerance 0", "B2-L2 date_tolerance 2"}},
		// Neither reference resembles the bank's, so a slight difference must not decide.
		{name: "weak likeness never breaks a tie", bank: banks(rec{id: "B1", ref: "ACMEPAYMENT"}),
			ledger: ledgers(rec{id: "L1", day: -1, ref: "INV44821"}, rec{id: "L2", day: 1, ref: "ZZZZZZZZ"}),
			want:   []string{"B1 ambiguous L1,L2", "L1 ambiguous B1", "L2 ambiguous B1"}},
		{name: "a strong reference beats no evidence", bank: banks(rec{id: "B1", ref: "INV20240017"}),
			ledger: ledgers(rec{id: "L1", day: -1, ref: "INV20240017X"}, rec{id: "L2", day: 1, ref: "ZZZZZZZZ"}),
			want:   []string{"B1-L1 date_tolerance 1", "L2 counterpart_taken B1"}},
		{name: "fewer edits win", bank: banks(rec{id: "B1", ref: "INV20240017"}),
			ledger: ledgers(rec{id: "L1", day: 1, ref: "INV20240018"}, rec{id: "L2", day: -1, ref: "INV20240017"}),
			want:   []string{"B1-L2 date_tolerance 1", "L1 counterpart_taken B1"}},
		{name: "a closer date beats a farther strong reference", bank: banks(rec{id: "B1", ref: "INV10001"}),
			ledger: ledgers(rec{id: "L1", day: -1}, rec{id: "L2", day: 2, ref: "INV10001"}),
			want:   []string{"B1-L1 date_tolerance 1", "L2 counterpart_taken B1"}},
		{name: "one edit in ten beats one in five", bank: banks(rec{id: "B1", ref: "ABCDEFGHIJ"}),
			ledger: ledgers(rec{id: "L1", day: -1, ref: "ABCDEFGHXJ"}, rec{id: "L2", day: 1, ref: "ABXDE"}),
			want:   []string{"B1-L1 date_tolerance 1", "L2 counterpart_taken B1"}},
		// Both references are equally close to the bank's.
		{name: "equal likeness ties", bank: banks(rec{id: "B1", ref: "ABCDEFGHIJ"}),
			ledger: ledgers(rec{id: "L1", day: 1, ref: "ABXDE"}, rec{id: "L2", day: -1, ref: "ABXDEFGHYJ"}),
			want:   []string{"B1 ambiguous L1,L2", "L1 ambiguous B1", "L2 ambiguous B1"}},
		{name: "identical duplicates pair up",
			bank:   banks(rec{id: "B1", ref: "INV10001"}, rec{id: "B2", ref: "INV10001", occ: 1}),
			ledger: ledgers(rec{id: "L1", ref: "INV10001"}, rec{id: "L2", ref: "INV10001", occ: 1}),
			want:   []string{"B1-L1 exact 0", "B2-L2 exact 0"}},
		{name: "duplicates pair in order of appearance, not by name",
			bank: banks(rec{id: "B1", occ: 1}, rec{id: "B2"}), ledger: ledgers(rec{id: "L1"}, rec{id: "L2", occ: 1}),
			want: []string{"B1-L2 date_tolerance 0", "B2-L1 date_tolerance 0"}},
		{name: "ledger duplicates pair in order of appearance",
			bank: banks(rec{id: "B1"}), ledger: ledgers(rec{id: "L1", occ: 1}, rec{id: "L2"}),
			want: []string{"B1-L2 date_tolerance 0", "L1 counterpart_taken B1"}},
		{name: "three by three duplicates",
			bank:   banks(rec{id: "B1", ref: "INV10001", occ: 2}, rec{id: "B2", ref: "INV10001"}, rec{id: "B3", ref: "INV10001", occ: 1}),
			ledger: ledgers(rec{id: "L1", ref: "INV10001", occ: 1}, rec{id: "L2", ref: "INV10001", occ: 2}, rec{id: "L3", ref: "INV10001"}),
			want:   []string{"B1-L2 exact 0", "B2-L3 exact 0", "B3-L1 exact 0"}},
		{name: "one bank record, two identical ledger records",
			bank: banks(rec{id: "B1"}), ledger: ledgers(rec{id: "L1"}, rec{id: "L2", occ: 1}),
			want: []string{"B1-L1 date_tolerance 0", "L2 counterpart_taken B1"}},
		{name: "a leftover duplicate matches at a greater distance",
			bank: banks(rec{id: "B1"}, rec{id: "B2", occ: 1}), ledger: ledgers(rec{id: "L1"}, rec{id: "L2", day: 2}),
			want: []string{"B1-L1 date_tolerance 0", "B2-L2 date_tolerance -2"}},
		// Only one side is identical, so the tie is reported rather than guessed.
		{name: "one identical side is still a tie",
			bank: banks(rec{id: "B1"}, rec{id: "B2", occ: 1}), ledger: ledgers(rec{id: "L1", day: -1}, rec{id: "L2", day: 1}),
			want: []string{"B1 ambiguous L1,L2", "B2 ambiguous L1,L2", "L1 ambiguous B1,B2", "L2 ambiguous B1,B2"}},
		{name: "same day but different references is a tie",
			bank: banks(rec{id: "B1"}), ledger: ledgers(rec{id: "L1", ref: "AAAAAAA"}, rec{id: "L2", ref: "BBBBBBB"}),
			want: []string{"B1 ambiguous L1,L2", "L1 ambiguous B1", "L2 ambiguous B1"}},
		// Two separate ties of equal cost, handled independently.
		{name: "a tie does not block its neighbours",
			bank:   banks(rec{id: "B1"}, rec{id: "B2", day: 10}),
			ledger: ledgers(rec{id: "L1", day: -1}, rec{id: "L2", day: 1}, rec{id: "L3", day: 11}),
			want:   []string{"B2-L3 date_tolerance -1", "B1 ambiguous L1,L2", "L1 ambiguous B1", "L2 ambiguous B1"}},
		{name: "four bank records tie for one ledger record",
			bank:   banks(rec{id: "B1", day: 1}, rec{id: "B2", day: 1, occ: 1}, rec{id: "B3", day: -1}, rec{id: "B4", day: 1, occ: 2}),
			ledger: ledgers(rec{id: "L1"}),
			want:   []string{"B1 ambiguous L1", "B2 ambiguous L1", "B3 ambiguous L1", "B4 ambiguous L1", "L1 ambiguous B1,B2,B3,B4"}},
		{name: "one bank record ties three ledger records",
			bank: banks(rec{id: "B1"}), ledger: ledgers(rec{id: "L1", day: 1}, rec{id: "L2", day: -1}, rec{id: "L3", day: 1, occ: 1}),
			want: []string{"B1 ambiguous L1,L2,L3", "L1 ambiguous B1", "L2 ambiguous B1", "L3 ambiguous B1"}},
		{name: "partners taken now and earlier are all listed",
			bank:   banks(rec{id: "B0", day: 3}, rec{id: "B1", ref: "INV10001"}, rec{id: "B2", ref: "INV10002"}, rec{id: "B3", ref: "INV10003"}),
			ledger: ledgers(rec{id: "L1", ref: "INV10001"}, rec{id: "L2", ref: "INV10002"}, rec{id: "L3", ref: "INV10003"}),
			taken:  ledgers(rec{id: "T1", day: 1}, rec{id: "T2", day: 2}, rec{id: "T3", day: 3}),
			want:   []string{"B1-L1 exact 0", "B2-L2 exact 0", "B3-L3 exact 0", "B0 counterpart_taken L1,L2,L3,T1,T2,T3"}},
		{name: "a narrower window leaves both losers explained",
			bank: banks(rec{id: "B1"}, rec{id: "B2", day: 1}), ledger: ledgers(rec{id: "L1"}, rec{id: "L2", day: -1}),
			p:    Params{DateTolerance: 1, FuzzyWindow: 7},
			want: []string{"B1-L1 date_tolerance 0", "B2 counterpart_taken L1", "L2 counterpart_taken B1"}},
		// Two bank records tie for one ledger record, blocking the only other partner.
		{name: "a tie blocks a neighbour",
			bank: banks(rec{id: "B1", day: 1}, rec{id: "B2", day: -1}), ledger: ledgers(rec{id: "L1"}, rec{id: "L2", day: 3}),
			want: []string{"B1 ambiguous L1", "B2 ambiguous L1", "L1 ambiguous B1,B2", "L2 counterpart_taken B1"}},
		{name: "a partner matched in an earlier run", bank: banks(rec{id: "B1"}), taken: ledgers(rec{id: "L1"}),
			want: []string{"B1 counterpart_taken L1"}},
		{name: "earlier matches are never matched or reported again", ledger: ledgers(rec{id: "L1"}), taken: banks(rec{id: "B1"}),
			want: []string{"L1 counterpart_taken B1"}},
		// Given out of order and mixed across sides.
		{name: "several earlier partners on each side",
			bank: banks(rec{id: "B1"}), ledger: ledgers(rec{id: "L1", day: 20}),
			taken: slices.Concat(banks(rec{id: "T4", day: 20, occ: 1}), ledgers(rec{id: "T2", occ: 1}),
				banks(rec{id: "T3", day: 20}), ledgers(rec{id: "T1"})),
			want: []string{"B1 counterpart_taken T1,T2", "L1 counterpart_taken T3,T4"}},
		{name: "an earlier match too far away is no excuse",
			bank: banks(rec{id: "B1", day: 10}), taken: ledgers(rec{id: "L1"}), want: []string{"B1 no_rule_match"}},
		// Given out of order; every partner of the last ledger record is taken.
		{name: "taken partners are listed in order",
			bank:   banks(rec{id: "B2", ref: "INV10002"}, rec{id: "B1", ref: "INV10001"}),
			ledger: ledgers(rec{id: "L2", ref: "INV10002"}, rec{id: "L1", ref: "INV10001"}, rec{id: "L3", day: 1}),
			taken:  banks(rec{id: "B0", day: 2}),
			want:   []string{"B1-L1 exact 0", "B2-L2 exact 0", "L3 counterpart_taken B0,B1,B2"}},
		{name: "a net payout has no partner", bank: banks(rec{id: "B1", amount: 980}), ledger: ledgers(rec{id: "L1", amount: 1000}),
			want: []string{"B1 no_amount_match", "L1 no_amount_match"}},
		// Real identities are fingerprints, so the two sides mix when sorted.
		{name: "bank exceptions come first", bank: banks(rec{id: "Z1"}), ledger: ledgers(rec{id: "A1", amount: 7}),
			want: []string{"Z1 no_amount_match", "A1 no_amount_match"}},
	})
}

func TestFuzzyPass(t *testing.T) {
	run(t, []scenario{
		{name: "a matching reference widens the window",
			bank:   banks(rec{id: "B1", day: 7, ref: "PAYMENT INV10023 ACME"}, rec{id: "B2", day: -7, ref: "INV20023", amount: 200}),
			ledger: ledgers(rec{id: "L1", ref: "INV10023"}, rec{id: "L2", ref: "INV20O23", amount: 200}),
			want:   []string{"B1-L1 fuzzy_reference 7", "B2-L2 fuzzy_reference -7"}},
		{name: "but only so far",
			bank:   banks(rec{id: "B1", day: 8, ref: "INV10023"}, rec{id: "B2", day: -8, ref: "INV20023", amount: 200}),
			ledger: ledgers(rec{id: "L1", ref: "INV10023"}, rec{id: "L2", ref: "INV20023", amount: 200}),
			want:   []string{"B1 no_rule_match", "B2 no_rule_match", "L1 no_rule_match", "L2 no_rule_match"}},
		{name: "no evidence never widens the window",
			bank:   banks(rec{id: "B1", day: 6}, rec{id: "B2", day: 5, ref: "INV10023", amount: 200}),
			ledger: ledgers(rec{id: "L1"}, rec{id: "L2", ref: "ZZZZZZZZ", amount: 200}),
			want:   []string{"B1 no_rule_match", "B2 no_rule_match", "L1 no_rule_match", "L2 no_rule_match"}},
		// Two edits in eight characters is not close enough.
		{name: "weak likeness is not enough", bank: banks(rec{id: "B1", day: 5, ref: "INV10023"}), ledger: ledgers(rec{id: "L1", ref: "INV19923"}),
			want: []string{"B1 no_rule_match", "L1 no_rule_match"}},
		{name: "a tie from the date step stays a tie", bank: banks(rec{id: "B1", ref: "INV10001"}),
			ledger: ledgers(rec{id: "L1", day: -1, ref: "ZZZZZZZZ"}, rec{id: "L2", day: 1, ref: "YYYYYYYY"}, rec{id: "L3", day: 6, ref: "INV10001"}),
			want:   []string{"B1 ambiguous L1,L2", "L1 ambiguous B1", "L2 ambiguous B1", "L3 counterpart_taken B1"}},
		{name: "the date step wins over a better reference", bank: banks(rec{id: "B1", ref: "INV10001"}),
			ledger: ledgers(rec{id: "L1", day: 2, ref: "ZZZZZZZZ"}, rec{id: "L2", day: 5, ref: "INV10001"}),
			want:   []string{"B1-L1 date_tolerance -2", "L2 counterpart_taken B1"}},
		{name: "the closer strong candidate wins", bank: banks(rec{id: "B1", ref: "INV10001"}),
			ledger: ledgers(rec{id: "L1", day: 6, ref: "INV10001"}, rec{id: "L2", day: -5, ref: "INV10001"}),
			want:   []string{"B1-L2 fuzzy_reference 5", "L1 counterpart_taken B1"}},
		{name: "the better reference wins at equal distance", bank: banks(rec{id: "B1", ref: "INV10001"}),
			ledger: ledgers(rec{id: "L1", day: 5, ref: "INV10X01"}, rec{id: "L2", day: -5, ref: "INV10001"}),
			want:   []string{"B1-L2 fuzzy_reference 5", "L1 counterpart_taken B1"}},
		{name: "equally strong candidates tie", bank: banks(rec{id: "B1", ref: "INV10001"}),
			ledger: ledgers(rec{id: "L1", day: 5, ref: "INV10001"}, rec{id: "L2", day: -5, ref: "XINV10001"}),
			want:   []string{"B1 ambiguous L1,L2", "L1 ambiguous B1", "L2 ambiguous B1"}},
		{name: "the window follows the setting", bank: banks(rec{id: "B1", day: 5, ref: "INV10023"}), ledger: ledgers(rec{id: "L1", ref: "INV10023"}),
			p: Params{DateTolerance: 1, FuzzyWindow: 4}, want: []string{"B1 no_rule_match", "L1 no_rule_match"}},
	})
}

func TestOutputOrderIsFixed(t *testing.T) {
	// The output order must hold even if identities were to repeat.
	var br, lr []rec
	for i := 1; i <= 40; i++ {
		br = append(br, rec{id: "X", amount: int64(i)})
		if i <= 20 {
			lr = append(lr, rec{id: "Y", amount: int64(i), day: 10})
		}
	}
	bank, ledger := banks(br...), ledgers(lr...)
	want := Reconcile(bank, ledger, nil, defaults)
	for range 20 {
		if diff := cmp.Diff(want, Reconcile(bank, ledger, nil, defaults)); diff != "" {
			t.Fatalf("a repeated call changed the output (-want +got):\n%s", diff)
		}
	}
}

// shuffleFixture mixes every outcome, with partners already taken.
func shuffleFixture() (bank, ledger, taken []domain.Transaction) {
	bank = banks(rec{id: "B01", ref: "INV10001"}, rec{id: "B02", ref: "INV10001", occ: 1}, rec{id: "B03", day: 1, ref: "ACMEPAYMENT"},
		rec{id: "B04", day: 5, amount: 300}, rec{id: "B05", day: 1, amount: 400}, rec{id: "B06", amount: 500},
		rec{id: "B07", day: 2, amount: 500}, rec{id: "B08", amount: 999}, rec{id: "B09", day: 1, amount: 600},
		rec{id: "B10", day: -1, amount: 600}, rec{id: "Z01", amount: 700, ref: "INV70001"}, rec{id: "B11", amount: 700, ref: "INV70002"})
	ledger = ledgers(rec{id: "L01", ref: "INV10001"}, rec{id: "L02", ref: "INV10001", occ: 1}, rec{id: "L03", ref: "INV44821"},
		rec{id: "L04", day: 2, ref: "ZZZZZZZZ"}, rec{id: "L05", amount: 300}, rec{id: "L06", amount: 400},
		rec{id: "L07", amount: 400, occ: 1}, rec{id: "L08", amount: 500}, rec{id: "L09", amount: 600}, rec{id: "L10", day: 3, amount: 600},
		rec{id: "A01", amount: 700, ref: "INV70001"}, rec{id: "L11", amount: 700, day: 1}, rec{id: "L12", amount: 700, ref: "INV70002"})
	// Reference cases: a late match, a tie and a match too far apart.
	bank = append(bank, banks(rec{id: "B21", amount: 800, day: 6, ref: "PAYMENT INV80001"},
		rec{id: "B22", amount: 900, ref: "INV90001"}, rec{id: "B23", amount: 950, day: 9, ref: "INV95001"})...)
	ledger = append(ledger, ledgers(rec{id: "L21", amount: 800, ref: "INV80001"}, rec{id: "L22", amount: 900, day: 5, ref: "INV90001"},
		rec{id: "L23", amount: 900, day: -5, ref: "XINV90001"}, rec{id: "L24", amount: 950, ref: "INV95001"})...)
	return bank, ledger, banks(rec{id: "T01", amount: 700, day: 2})
}

func TestShuffleDeterminism(t *testing.T) {
	bank, ledger, taken := shuffleFixture()
	want := Reconcile(bank, ledger, taken, defaults)
	rng := rand.New(rand.NewPCG(7, 11))
	for i := range 50 {
		b, l := slices.Clone(bank), slices.Clone(ledger)
		rng.Shuffle(len(b), func(x, y int) { b[x], b[y] = b[y], b[x] })
		rng.Shuffle(len(l), func(x, y int) { l[x], l[y] = l[y], l[x] })
		// A reordered file also renumbers its lines.
		for j := range b {
			b[j].Line = j + 2
		}
		for j := range l {
			l[j].Line = j + 2
		}
		if diff := cmp.Diff(want, Reconcile(b, l, taken, defaults)); diff != "" {
			t.Fatalf("permutation %d changed the result (-want +got):\n%s", i, diff)
		}
	}
	if len(want.Matches) == 0 || len(want.Exceptions) == 0 {
		t.Fatal("the fixture should produce both matches and exceptions")
	}
}

func TestParamsValidate(t *testing.T) {
	for p, ok := range map[Params]bool{{-1, 7}: false, {3, 3}: false, {7, 3}: false, defaults: true, {0, 1}: true} {
		if (p.Validate() == nil) != ok {
			t.Errorf("Validate(%+v) accepted = %v, want %v", p, !ok, ok)
		}
	}
}

// BenchmarkReconcileLargePartition measures a year of records sharing one amount.
func BenchmarkReconcileLargePartition(b *testing.B) {
	var br, lr []rec
	for i := range 2000 {
		br = append(br, rec{id: fmt.Sprintf("B%05d", i), day: i % 365, ref: fmt.Sprintf("PAYMENT REFERENCE INV%08d", i)})
		lr = append(lr, rec{id: fmt.Sprintf("L%05d", i), day: (i * 7) % 365, ref: fmt.Sprintf("INV%08d", i)})
	}
	bank, ledger := banks(br...), ledgers(lr...)
	for b.Loop() {
		Reconcile(bank, ledger, nil, defaults)
	}
}
