package match

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	gocmp "github.com/google/go-cmp/cmp"

	"github.com/mehmetkr/recon-go/internal/domain"
)

// manyPartitions builds records spread across hundreds of groups.
func manyPartitions(seed uint64) (bank, ledger, taken []domain.Transaction) {
	rng := rand.New(rand.NewPCG(seed, 1))
	refs := []string{"", "INV10001", "INV10002", "PAYMENT INV10001", "ZZZZZZZZ"}
	var side [3][]rec
	for i := range 1200 {
		r := rec{
			day:    rng.IntN(9) - 4,
			ref:    refs[rng.IntN(len(refs))],
			amount: int64(1 + rng.IntN(300)),
			acct:   fmt.Sprintf("ACC-%d", rng.IntN(2)),
			occ:    i, // unique copies
		}
		s := []int{0, 0, 1, 1, 2}[rng.IntN(5)]
		r.id = fmt.Sprintf("%c%05d", "BLT"[s], i)
		side[s] = append(side[s], r)
	}
	tr := side[2]
	return banks(side[0]...), ledgers(side[1]...), append(banks(tr[:len(tr)/2]...), ledgers(tr[len(tr)/2:]...)...)
}

// watchUnits calls f as each batch starts, for the rest of the test.
func watchUnits(t *testing.T, f func()) {
	unitStarted = f
	t.Cleanup(func() { unitStarted = nil })
}

func TestRunMatchesReconcile(t *testing.T) {
	bank, ledger, taken := manyPartitions(42)
	parts := partitionInputs(bank, ledger, taken)
	if len(parts) < 200 || len(packUnits(parts)) <= 8 {
		t.Fatal("the fixture needs at least 200 groups in more than 8 batches")
	}
	want := Reconcile(bank, ledger, taken, defaults)
	if len(want.Matches) == 0 || len(want.Exceptions) == 0 {
		t.Fatal("the fixture should produce matches and exceptions")
	}
	for _, workers := range []int{1, 8, 64} {
		got, err := Run(context.Background(), bank, ledger, taken, defaults, workers)
		if diff := gocmp.Diff(want, got); err != nil || diff != "" {
			t.Fatalf("workers=%d: %v (-want +got):\n%s", workers, err, diff)
		}
	}
	if _, err := Run(context.Background(), nil, nil, nil, defaults, 0); err == nil {
		t.Error("zero workers was accepted")
	}
}

func TestPackUnits(t *testing.T) {
	// Batches close at 64, groups are never split, and the last batch may be smaller.
	for _, tt := range []struct{ in, want []int }{
		{[]int{10, 60, 1, 70, 3, 2}, []int{70, 71, 5}},
		{[]int{64, 1}, []int{64, 1}},
		{[]int{63, 1, 1}, []int{64, 1}},
		{[]int{60, 4, 1}, []int{64, 1}},
		{[]int{63}, []int{63}},
		{[]int{62, 1}, []int{63}},
		{nil, nil},
	} {
		var parts []unit
		for _, n := range tt.in {
			parts = append(parts, unit{bank: make([]domain.Transaction, n)})
		}
		var got []int
		for _, u := range packUnits(parts) {
			got = append(got, u.size())
		}
		if !slices.Equal(got, tt.want) {
			t.Errorf("packUnits(%v) sizes = %v, want %v", tt.in, got, tt.want)
		}
	}
	bank, ledger, taken := manyPartitions(7)
	seen := map[domain.PartitionKey]int{}
	for i, u := range packUnits(partitionInputs(bank, ledger, taken)) {
		for _, tx := range slices.Concat(u.bank, u.ledger, u.taken) {
			if prev, ok := seen[tx.PartitionKey()]; ok && prev != i {
				t.Fatalf("group %+v split across batches %d and %d", tx.PartitionKey(), prev, i)
			}
			seen[tx.PartitionKey()] = i
		}
	}
}

func TestRunCancelled(t *testing.T) {
	bank, ledger, taken := manyPartitions(3)
	for _, tt := range []struct {
		name                string
		bank, ledger, taken []domain.Transaction
		workers, started    int
		before              bool // cancel before the run, rather than as the first batch starts
	}{
		{"before the run", bank, ledger, taken, 4, 0, true},
		{"before an empty run", nil, nil, nil, 4, 0, true},
		// Cancellation is noticed before anything else.
		{"before a run with invalid settings", nil, nil, nil, 0, 0, true},
		{"during the first of many batches", bank, ledger, taken, 1, 1, false},
		// With a single batch, only the final check can notice the cancellation.
		{"during the only batch", bank[:5], ledger[:5], nil, 4, 1, false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if tt.before {
				cancel()
			}
			started := 0
			watchUnits(t, func() { started++; cancel() })
			res, err := Run(ctx, tt.bank, tt.ledger, tt.taken, defaults, tt.workers)
			if !errors.Is(err, context.Canceled) || res.Matches != nil || res.Exceptions != nil || started != tt.started {
				t.Errorf("err %v, %d matches, %d batches started; want cancelled, no result, %d started",
					err, len(res.Matches), started, tt.started)
			}
		})
	}
}

func TestRunUsesUpToWorkersUnitsAtOnce(t *testing.T) {
	bank, ledger, taken := manyPartitions(42)
	for _, workers := range []int32{2, 8} {
		var inFlight, peak atomic.Int32
		watchUnits(t, func() {
			n := inFlight.Add(1)
			for p := peak.Load(); n > p && !peak.CompareAndSwap(p, n); p = peak.Load() {
			}
			// Each batch lingers, so running more than allowed at once would show.
			time.Sleep(20 * time.Millisecond)
			inFlight.Add(-1)
		})
		if _, err := Run(context.Background(), bank, ledger, taken, defaults, int(workers)); err != nil {
			t.Fatal(err)
		}
		if p := peak.Load(); p != workers {
			t.Errorf("workers=%d: peak %d batches at once, want exactly %d", workers, p, workers)
		}
	}
}
