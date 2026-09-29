package match

import (
	"cmp"
	"fmt"
	"math/rand/v2"
	"slices"
	"testing"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/mehmetkr/recon-go/internal/domain"
)

// This file checks the engine against a slow, independent version of the same rules.

type refCost struct{ days, dist, len int } // zero means no evidence

func refSim(a, b string) (dist, n int, evidence bool) {
	if len(a) < MinRefLen || len(b) < MinRefLen {
		return 0, 0, false
	}
	pattern, text := a, b
	if len(b) < len(a) || (len(b) == len(a) && b < a) {
		pattern, text = b, a
	}
	best := len(pattern)
	for i := 0; i <= len(text); i++ {
		for j := i; j <= len(text); j++ {
			best = min(best, levenshtein(pattern, text[i:j]))
		}
	}
	return best, len(pattern), true
}

func refCostOf(b, l domain.Transaction) refCost {
	days := int(b.Date - l.Date)
	days = max(days, -days)
	d, n, ev := refSim(b.RefNorm, l.RefNorm)
	if !ev || 5*d > n { // too dissimilar to count
		return refCost{days, 0, 0}
	}
	return refCost{days, d, n}
}

// refCompare ranks costs the way the rules describe.
func refCompare(x, y refCost) int {
	if x.days != y.days {
		return cmp.Compare(x.days, y.days)
	}
	switch {
	case x.len == 0 && y.len == 0:
		return 0
	case x.len == 0:
		return 1
	case y.len == 0:
		return -1
	}
	return cmp.Compare(x.dist*y.len, y.dist*x.len)
}

func refEligible(rule int, b, l domain.Transaction, p Params) bool {
	days := int(b.Date - l.Date)
	days = max(days, -days)
	switch rule {
	case 0:
		return days == 0 && len(b.RefNorm) >= MinRefLen && b.RefNorm == l.RefNorm
	case 1:
		return days <= p.DateTolerance
	default:
		d, n, ev := refSim(b.RefNorm, l.RefNorm)
		return days <= p.FuzzyWindow && ev && 5*d <= n
	}
}

const refRules = 3

func refReconcile(bank, ledger, taken []domain.Transaction, p Params) Result {
	const (
		free = iota
		matchedS
		ambiguousS
	)
	state := map[string]int{} // keyed by side and identity
	cands := map[string][]string{}
	key := func(t domain.Transaction) string { return string(t.Source) + "/" + t.ID }
	sameKey := func(a, b domain.Transaction) bool { return a.PartitionKey() == b.PartitionKey() }
	var res Result

	for rule := 0; rule < refRules; rule++ {
		for {
			// Every pairing still open, and the lowest cost among them.
			type edge struct{ b, l domain.Transaction }
			var live []edge
			for _, b := range bank {
				for _, l := range ledger {
					if sameKey(b, l) && state[key(b)] == free && state[key(l)] == free && refEligible(rule, b, l, p) {
						live = append(live, edge{b, l})
					}
				}
			}
			if len(live) == 0 {
				break
			}
			best := refCostOf(live[0].b, live[0].l)
			for _, e := range live[1:] {
				if c := refCostOf(e.b, e.l); refCompare(c, best) < 0 {
					best = c
				}
			}
			var group []edge
			for _, e := range live {
				if refCompare(refCostOf(e.b, e.l), best) == 0 {
					group = append(group, e)
				}
			}
			// Group tied pairings into clusters.
			seen := map[string]bool{}
			for _, start := range group {
				if seen[key(start.b)] {
					continue
				}
				comp := map[string]bool{key(start.b): true}
				for changed := true; changed; {
					changed = false
					for _, e := range group {
						if comp[key(e.b)] != comp[key(e.l)] {
							comp[key(e.b)], comp[key(e.l)] = true, true
							changed = true
						}
					}
				}
				var bs, ls []domain.Transaction
				var edges []edge
				for _, e := range group {
					if comp[key(e.b)] {
						edges = append(edges, e)
						if !slices.ContainsFunc(bs, func(t domain.Transaction) bool { return t.ID == e.b.ID }) {
							bs = append(bs, e.b)
						}
						if !slices.ContainsFunc(ls, func(t domain.Transaction) bool { return t.ID == e.l.ID }) {
							ls = append(ls, e.l)
						}
					}
				}
				for k := range comp {
					seen[k] = true
				}
				identical := func(ts []domain.Transaction) bool {
					for _, t := range ts {
						if t.ContentKey() != ts[0].ContentKey() {
							return false
						}
					}
					return true
				}
				if identical(bs) && identical(ls) {
					byOcc := func(x, y domain.Transaction) int { return cmp.Compare(x.Occurrence, y.Occurrence) }
					slices.SortFunc(bs, byOcc)
					slices.SortFunc(ls, byOcc)
					for i := 0; i < len(bs) && i < len(ls); i++ {
						state[key(bs[i])], state[key(ls[i])] = matchedS, matchedS
						res.Matches = append(res.Matches, domain.Match{
							MatchID: domain.MatchID(bs[i].ID, ls[i].ID), BankID: bs[i].ID, LedgerID: ls[i].ID,
							Rule: domain.Rules[rule], DayDelta: int(bs[i].Date - ls[i].Date),
						})
					}
					continue
				}
				for _, e := range edges {
					state[key(e.b)], state[key(e.l)] = ambiguousS, ambiguousS
					cands[key(e.b)] = append(cands[key(e.b)], e.l.ID)
					cands[key(e.l)] = append(cands[key(e.l)], e.b.ID)
				}
			}
		}
	}

	passesAny := func(b, l domain.Transaction) bool {
		for rule := 0; rule < refRules; rule++ {
			if sameKey(b, l) && refEligible(rule, b, l, p) {
				return true
			}
		}
		return false
	}
	explain := func(t domain.Transaction, others []domain.Transaction, isBank bool) {
		st := state[key(t)]
		if st == matchedS {
			return
		}
		e := domain.Exception{Source: t.Source, ID: t.ID, Candidates: []string{}}
		if st == ambiguousS {
			e.Reason = domain.ReasonAmbiguous
			e.Candidates = cands[key(t)]
			slices.Sort(e.Candidates)
		} else {
			anySameKey := false
			for _, o := range others {
				if !sameKey(t, o) {
					continue
				}
				anySameKey = true
				isTaken := o.Source != t.Source && slices.ContainsFunc(taken, func(x domain.Transaction) bool { return x.ID == o.ID })
				available := state[key(o)] == free && !isTaken
				pass := (isBank && passesAny(t, o)) || (!isBank && passesAny(o, t))
				if pass && !available {
					e.Candidates = append(e.Candidates, o.ID)
				}
			}
			slices.Sort(e.Candidates)
			switch {
			case len(e.Candidates) > 0:
				e.Reason = domain.ReasonCounterpartTaken
			case anySameKey:
				e.Reason = domain.ReasonNoRuleMatch
			default:
				e.Reason = domain.ReasonNoAmountMatch
			}
		}
		res.Exceptions = append(res.Exceptions, e)
	}
	var takenBank, takenLedger []domain.Transaction
	for _, t := range taken {
		if t.Source == domain.Bank {
			takenBank = append(takenBank, t)
		} else {
			takenLedger = append(takenLedger, t)
		}
	}
	for _, b := range bank {
		explain(b, append(slices.Clone(ledger), takenLedger...), true)
	}
	for _, l := range ledger {
		explain(l, append(slices.Clone(bank), takenBank...), false)
	}
	slices.SortFunc(res.Matches, func(a, b domain.Match) int { return cmp.Compare(a.BankID, b.BankID) })
	slices.SortFunc(res.Exceptions, func(a, b domain.Exception) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.ID, b.ID))
	})
	return res
}

// randomCase builds small, crowded cases that reach every outcome.
func randomCase(rng *rand.Rand) (bank, ledger, taken []domain.Transaction, p Params) {
	refs := []string{"", "INV1", "INV10001", "INV10002", "XINV10001Y", "ABCDEFGHIJ", "ABXDE", "ABXDEFGHYJ", "ZZZZZZZZ"}
	gen := func(src domain.Source, prefix string, n int) []domain.Transaction {
		var rs []rec
		occ := map[string]int{}
		for i := range n {
			r := rec{
				id:     fmt.Sprintf("%s%02d", prefix, i),
				day:    rng.IntN(11) - 5,
				ref:    refs[rng.IntN(len(refs))],
				amount: int64(100 * (1 + rng.IntN(2))),
			}
			k := fmt.Sprint(r.day, r.ref, r.amount)
			r.occ = occ[k]
			occ[k]++
			rs = append(rs, r)
		}
		rng.Shuffle(len(rs), func(i, j int) { rs[i], rs[j] = rs[j], rs[i] })
		return build(src, rs...)
	}
	bank = gen(domain.Bank, "B", rng.IntN(7))
	ledger = gen(domain.Ledger, "L", rng.IntN(7))
	taken = append(gen(domain.Bank, "TB", rng.IntN(3)), gen(domain.Ledger, "TL", rng.IntN(3))...)
	n := rng.IntN(4)
	return bank, ledger, taken, Params{DateTolerance: n, FuzzyWindow: n + 1 + rng.IntN(6)}
}

func TestReconcileMatchesReference(t *testing.T) {
	rng := rand.New(rand.NewPCG(2024, 10))
	for i := range 20000 {
		bank, ledger, taken, p := randomCase(rng)
		want := refReconcile(bank, ledger, taken, p)
		got := Reconcile(bank, ledger, taken, p)
		if diff := gocmp.Diff(want, got); diff != "" {
			t.Fatalf("case %d (%+v)\nbank=%v\nledger=%v\ntaken=%v\n(-reference +engine):\n%s",
				i, p, summarizeTx(bank), summarizeTx(ledger), summarizeTx(taken), diff)
		}
	}
}

func summarizeTx(ts []domain.Transaction) []string {
	var out []string
	for _, t := range ts {
		out = append(out, fmt.Sprintf("%s(d%d,%s,%d,o%d)", t.ID, t.Date-day0, t.RefNorm, t.Amount, t.Occurrence))
	}
	return out
}
