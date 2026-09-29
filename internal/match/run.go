package match

import (
	"cmp"
	"context"
	"errors"
	"maps"
	"slices"

	"golang.org/x/sync/errgroup"

	"github.com/mehmetkr/recon-go/internal/domain"
)

// minUnitRecords is the batch size that keeps small groups from drowning in overhead.
const minUnitRecords = 64

// unitStarted lets tests watch each batch begin.
var unitStarted func()

// unit is a batch of whole groups handled together.
type unit struct {
	bank, ledger, taken []domain.Transaction
}

func (u unit) size() int { return len(u.bank) + len(u.ledger) + len(u.taken) }

// Run reconciles groups in parallel and gives exactly the result Reconcile gives.
func Run(ctx context.Context, bank, ledger, taken []domain.Transaction, p Params, workers int) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}
	if workers < 1 {
		return Result{}, errors.New("workers must be at least 1")
	}

	units := packUnits(partitionInputs(bank, ledger, taken))
	results := make([]Result, len(units))
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(workers)
	for i, u := range units {
		g.Go(func() error {
			if err := gctx.Err(); err != nil {
				return err
			}
			if unitStarted != nil {
				unitStarted()
			}
			results[i] = Reconcile(u.bank, u.ledger, u.taken, p) // each batch writes only its own slot
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return Result{}, err
	}
	// A run cancelled part-way must never pass for a finished one.
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	var res Result
	for _, r := range results {
		res.Matches = append(res.Matches, r.Matches...)
		res.Exceptions = append(res.Exceptions, r.Exceptions...)
	}
	sortResult(&res)
	return res, nil
}

// partitionInputs groups the records that could match, in a fixed order.
func partitionInputs(bank, ledger, taken []domain.Transaction) []unit {
	parts := make(map[domain.PartitionKey]*unit)
	get := func(k domain.PartitionKey) *unit {
		if parts[k] == nil {
			parts[k] = &unit{}
		}
		return parts[k]
	}
	for _, t := range bank {
		u := get(t.PartitionKey())
		u.bank = append(u.bank, t)
	}
	for _, t := range ledger {
		u := get(t.PartitionKey())
		u.ledger = append(u.ledger, t)
	}
	for _, t := range taken {
		u := get(t.PartitionKey())
		u.taken = append(u.taken, t)
	}
	keys := slices.SortedFunc(maps.Keys(parts), func(a, b domain.PartitionKey) int {
		return cmp.Or(cmp.Compare(a.Account, b.Account), cmp.Compare(a.Currency, b.Currency), cmp.Compare(a.Amount, b.Amount))
	})
	out := make([]unit, 0, len(keys))
	for _, k := range keys {
		out = append(out, *parts[k])
	}
	return out
}

// packUnits bundles whole groups into batches of at least minUnitRecords.
func packUnits(parts []unit) []unit {
	var units []unit
	var cur unit
	for _, pt := range parts {
		cur.bank = append(cur.bank, pt.bank...)
		cur.ledger = append(cur.ledger, pt.ledger...)
		cur.taken = append(cur.taken, pt.taken...)
		if cur.size() >= minUnitRecords {
			units = append(units, cur)
			cur = unit{}
		}
	}
	if cur.size() > 0 {
		units = append(units, cur)
	}
	return units
}
