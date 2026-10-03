package match

import (
	"fmt"
	"testing"

	"github.com/mehmetkr/recon-go/internal/domain"
)

var simSink simScore

func BenchmarkScoreRefs(b *testing.B) {
	for _, tt := range []struct {
		name string
		a, b string
	}{
		{"identical_short", "INV10023", "INV10023"},
		{"identical_long", "PAYMENT-REF-2026-09-29-ABCDEF", "PAYMENT-REF-2026-09-29-ABCDEF"},
		{"contained", "INV10023", "PAYMENT INV10023 CONFIRMED"},
		{"distant", "ABCDEFGHIJ", "KLMNOPQRST"},
	} {
		b.Run(tt.name, func(b *testing.B) {
			for range b.N {
				simSink = scoreRefs(tt.a, tt.b)
			}
		})
	}
}

var reconcileSink Result

func BenchmarkReconcile(b *testing.B) {
	for _, n := range []int{10, 100, 1000} {
		b.Run(fmt.Sprintf("n=%d", n), func(b *testing.B) {
			bank := make([]domain.Transaction, n)
			ledger := make([]domain.Transaction, n)
			for i := range n {
				bank[i] = domain.Transaction{
					ID:       fmt.Sprintf("B-%04d", i),
					Source:   domain.Bank,
					Account:  "ACC-1",
					Currency: "EUR",
					Amount:   int64(1000 + i),
					Date:     domain.Date(20000 + int32(i)),
					RefNorm:  fmt.Sprintf("REF%04d", i),
				}
				ledger[i] = domain.Transaction{
					ID:       fmt.Sprintf("L-%04d", i),
					Source:   domain.Ledger,
					Account:  "ACC-1",
					Currency: "EUR",
					Amount:   int64(1000 + i),
					Date:     domain.Date(20000 + int32(i)),
					RefNorm:  fmt.Sprintf("REF%04d", i),
				}
			}
			p := Params{DateTolerance: 3, FuzzyWindow: 7}
			b.ResetTimer()
			for range b.N {
				reconcileSink = Reconcile(bank, ledger, nil, p)
			}
		})
	}
}
