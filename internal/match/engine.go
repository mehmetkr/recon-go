// Package match pairs bank records with ledger records.
package match

import (
	"cmp"
	"errors"
	"slices"

	"github.com/mehmetkr/recon-go/internal/domain"
)

// Params are the matching windows, in days.
type Params struct {
	DateTolerance int // days allowed by the date step
	FuzzyWindow   int // days allowed when references agree
}

// Validate checks that the reference window is wider than the date window.
func (p Params) Validate() error {
	if p.DateTolerance < 0 {
		return errors.New("date tolerance must not be negative")
	}
	if p.DateTolerance >= p.FuzzyWindow {
		return errors.New("fuzzy window must be larger than the date tolerance")
	}
	return nil
}

// Result is the outcome of one reconciliation.
type Result struct {
	Matches    []domain.Match     // ordered by bank record
	Exceptions []domain.Exception // bank first, then ledger
}

// pair is one possible pairing of a bank and a ledger record.
type pair struct {
	b, l  *domain.Transaction
	delta int // days between the bank and ledger dates

	// Comparing references is costly, so it happens only when needed.
	sim     simScore
	simDone bool
	key     simKey // set once the pair is a candidate
}

func newPair(b, l *domain.Transaction) pair {
	return pair{b: b, l: l, delta: int(b.Date - l.Date)}
}

func (p *pair) absDelta() int { return max(p.delta, -p.delta) }

func (p *pair) similarity() simScore {
	if !p.simDone {
		p.sim, p.simDone = scoreRefs(p.b.RefNorm, p.l.RefNorm), true
	}
	return p.sim
}

// cost ranks candidates: closer dates first, then closer references.
func (p *pair) cost() (int, simKey) { return p.absDelta(), p.key }

// rule is one matching step.
type rule struct {
	name     domain.Rule
	eligible func(*pair) bool
}

func rules(p Params) []rule {
	return []rule{
		{domain.RuleExact, func(c *pair) bool {
			return c.delta == 0 && len(c.b.RefNorm) >= MinRefLen && c.b.RefNorm == c.l.RefNorm
		}},
		{domain.RuleDateTolerance, func(c *pair) bool {
			return c.absDelta() <= p.DateTolerance
		}},
	}
}

// Reconcile pairs bank and ledger records one to one, whatever order they arrive in.
func Reconcile(bank, ledger, taken []domain.Transaction, p Params) Result {
	parts := make(map[domain.PartitionKey]*partition)
	get := func(k domain.PartitionKey) *partition {
		if parts[k] == nil {
			parts[k] = &partition{}
		}
		return parts[k]
	}
	for i := range bank {
		pt := get(bank[i].PartitionKey())
		pt.bank = append(pt.bank, &bank[i])
	}
	for i := range ledger {
		pt := get(ledger[i].PartitionKey())
		pt.ledger = append(pt.ledger, &ledger[i])
	}
	for i := range taken {
		pt := get(taken[i].PartitionKey())
		pt.taken = append(pt.taken, &taken[i])
	}

	var res Result
	rs := rules(p)
	for _, pt := range parts {
		pt.run(rs, &res)
	}
	// The final order never depends on processing order.
	slices.SortFunc(res.Matches, func(a, b domain.Match) int {
		return cmp.Or(cmp.Compare(a.BankID, b.BankID), cmp.Compare(a.LedgerID, b.LedgerID),
			cmp.Compare(a.Rule, b.Rule), cmp.Compare(a.DayDelta, b.DayDelta), cmp.Compare(a.MatchID, b.MatchID))
	})
	slices.SortFunc(res.Exceptions, func(a, b domain.Exception) int {
		return cmp.Or(cmp.Compare(a.Source, b.Source), cmp.Compare(a.ID, b.ID),
			cmp.Compare(a.Reason, b.Reason), slices.Compare(a.Candidates, b.Candidates))
	})
	return res
}

type status int

const (
	free status = iota
	matched
	ambiguous
)

type partition struct {
	bank, ledger, taken []*domain.Transaction

	status     map[*domain.Transaction]status
	candidates map[*domain.Transaction][]string // candidates for tied records
}

func (pt *partition) run(rs []rule, res *Result) {
	pt.status = make(map[*domain.Transaction]status)
	pt.candidates = make(map[*domain.Transaction][]string)
	for _, r := range rs {
		pt.pass(r, res)
	}
	pt.explain(rs, res)
}

// pass pairs the closest candidates first and reports genuine ties instead of guessing.
func (pt *partition) pass(r rule, res *Result) {
	var cands []pair
	c := new(pair) // one reusable pair keeps the loop light
	for _, b := range pt.bank {
		if pt.status[b] != free {
			continue
		}
		for _, l := range pt.ledger {
			if pt.status[l] != free {
				continue
			}
			*c = newPair(b, l)
			if r.eligible(c) {
				c.key = c.similarity().tieKey()
				cands = append(cands, *c)
			}
		}
	}
	// Identities only settle the listing order; they never pick a winner.
	slices.SortFunc(cands, func(x, y pair) int {
		xd, xk := x.cost()
		yd, yk := y.cost()
		return cmp.Or(cmp.Compare(xd, yd), xk.compare(yk), cmp.Compare(x.b.ID, y.b.ID), cmp.Compare(x.l.ID, y.l.ID))
	})
	for start := 0; start < len(cands); {
		d, k := cands[start].cost()
		end := start + 1
		for end < len(cands) {
			ed, ek := cands[end].cost()
			if ed != d || ek != k {
				break
			}
			end++
		}
		pt.assignGroup(r.name, cands[start:end], res)
		start = end
	}
}

func (pt *partition) assignGroup(rule domain.Rule, group []pair, res *Result) {
	var live []pair
	for _, c := range group {
		if pt.status[c.b] == free && pt.status[c.l] == free {
			live = append(live, c)
		}
	}
	for _, comp := range components(live) {
		banks, ledgers := endpoints(comp)
		if !contentIdentical(banks) || !contentIdentical(ledgers) {
			pt.markAmbiguous(comp)
			continue
		}
		// Identical records are interchangeable, so pair them in order.
		byOccurrence := func(x, y *domain.Transaction) int {
			return cmp.Or(cmp.Compare(x.Occurrence, y.Occurrence), cmp.Compare(x.ID, y.ID), cmp.Compare(x.Source, y.Source))
		}
		slices.SortFunc(banks, byOccurrence)
		slices.SortFunc(ledgers, byOccurrence)
		for i := range min(len(banks), len(ledgers)) {
			b, l := banks[i], ledgers[i]
			pt.status[b], pt.status[l] = matched, matched
			res.Matches = append(res.Matches, domain.Match{
				MatchID:  domain.MatchID(b.ID, l.ID),
				BankID:   b.ID,
				LedgerID: l.ID,
				Rule:     rule,
				DayDelta: int(b.Date - l.Date),
			})
		}
	}
}

func (pt *partition) markAmbiguous(comp []pair) {
	neighbours := make(map[*domain.Transaction][]string)
	for _, c := range comp {
		neighbours[c.b] = append(neighbours[c.b], c.l.ID)
		neighbours[c.l] = append(neighbours[c.l], c.b.ID)
	}
	for t, ids := range neighbours {
		pt.status[t] = ambiguous
		slices.Sort(ids)
		pt.candidates[t] = ids
	}
}

// explain gives every unmatched record the most specific reason that applies.
func (pt *partition) explain(rs []rule, res *Result) {
	var takenBank, takenLedger []*domain.Transaction
	for _, t := range pt.taken {
		if t.Source == domain.Bank {
			takenBank = append(takenBank, t)
		} else {
			takenLedger = append(takenLedger, t)
		}
	}
	pt.explainSide(pt.bank, pt.ledger, takenLedger, rs, res)
	pt.explainSide(pt.ledger, pt.bank, takenBank, rs, res)
}

func (pt *partition) explainSide(own, others, othersTaken []*domain.Transaction, rs []rule, res *Result) {
	c := new(pair) // reused, as in pass
	passesAny := func(t, o *domain.Transaction) bool {
		*c = pairOf(t, o)
		return slices.ContainsFunc(rs, func(r rule) bool { return r.eligible(c) })
	}
	for _, t := range own {
		var reason domain.Reason
		cands := []string{}
		switch pt.status[t] {
		case matched:
			continue
		case ambiguous:
			reason, cands = domain.ReasonAmbiguous, pt.candidates[t]
		default:
			// Every step has run, so any remaining partner is already spoken for.
			for _, o := range others {
				if pt.status[o] != free && passesAny(t, o) {
					cands = append(cands, o.ID)
				}
			}
			for _, o := range othersTaken {
				if passesAny(t, o) {
					cands = append(cands, o.ID)
				}
			}
			switch {
			case len(cands) > 0:
				reason = domain.ReasonCounterpartTaken
				slices.Sort(cands)
			case len(others)+len(othersTaken) > 0:
				reason = domain.ReasonNoRuleMatch
			default:
				reason = domain.ReasonNoAmountMatch
			}
		}
		res.Exceptions = append(res.Exceptions, domain.Exception{
			Source: t.Source, ID: t.ID, Reason: reason, Candidates: cands,
		})
	}
}

// pairOf builds a pair from two records on opposite sides.
func pairOf(t, o *domain.Transaction) pair {
	if t.Source == domain.Bank {
		return newPair(t, o)
	}
	return newPair(o, t)
}

// components splits candidates into independent clusters.
func components(cands []pair) [][]pair {
	parent := make(map[*domain.Transaction]*domain.Transaction)
	var find func(*domain.Transaction) *domain.Transaction
	find = func(t *domain.Transaction) *domain.Transaction {
		if parent[t] == nil || parent[t] == t {
			parent[t] = t
			return t
		}
		root := find(parent[t])
		parent[t] = root
		return root
	}
	for _, c := range cands {
		rb, rl := find(c.b), find(c.l)
		if rb != rl {
			parent[rl] = rb
		}
	}
	var order []*domain.Transaction
	byRoot := make(map[*domain.Transaction][]pair)
	for _, c := range cands {
		root := find(c.b)
		if byRoot[root] == nil {
			order = append(order, root)
		}
		byRoot[root] = append(byRoot[root], c)
	}
	out := make([][]pair, 0, len(order))
	for _, root := range order {
		out = append(out, byRoot[root])
	}
	return out
}

// endpoints lists the bank and ledger records in a cluster.
func endpoints(comp []pair) (banks, ledgers []*domain.Transaction) {
	seen := make(map[*domain.Transaction]bool)
	for _, c := range comp {
		if !seen[c.b] {
			seen[c.b] = true
			banks = append(banks, c.b)
		}
		if !seen[c.l] {
			seen[c.l] = true
			ledgers = append(ledgers, c.l)
		}
	}
	return banks, ledgers
}

func contentIdentical(ts []*domain.Transaction) bool {
	for _, t := range ts[1:] {
		if t.ContentKey() != ts[0].ContentKey() {
			return false
		}
	}
	return true
}
