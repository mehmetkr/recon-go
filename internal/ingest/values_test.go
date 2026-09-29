package ingest

import (
	"math"
	"testing"
)

// check compares a result and whether an error came with it.
func check[T comparable](t *testing.T, name string, got T, err error, want T, wantErr bool) {
	t.Helper()
	if got != want || (err != nil) != wantErr {
		t.Errorf("%s = %v, %v; want %v (error expected: %v)", name, got, err, want, wantErr)
	}
}

func TestParseAmount(t *testing.T) {
	for _, tt := range []struct {
		in   string
		want int64
		bad  bool
	}{
		{"100", 10000, false}, {"100.5", 10050, false}, {"100.05", 10005, false},
		{"-0.50", -50, false}, {"+1.00", 100, false}, {"-0.00", 0, false}, {"007.10", 710, false},
		{"92233720368547758.07", math.MaxInt64, false}, {"-92233720368547758.08", math.MinInt64, false},
		{"92233720368547758.08", 0, true}, {"1,000.00", 0, true}, {"(5.00)", 0, true}, {"1.234", 0, true},
		{"1.", 0, true}, {".5", 0, true}, {"", 0, true}, {"abc", 0, true}, {"1e3", 0, true}, {"--1", 0, true},
	} {
		got, err := parseAmount(tt.in)
		check(t, "parseAmount("+tt.in+")", got, err, tt.want, tt.bad)
	}
}

func TestLedgerAmount(t *testing.T) {
	for _, tt := range []struct {
		debit, credit string
		want          int64
		bad           bool
	}{
		{"100.00", "", 10000, false}, // money arriving
		{"", "5.00", -500, false},    // money leaving
		{"100.00", "0.00", 10000, false},
		{"-0.00", "5.00", -500, false},
		{"100.00", "5.00", 0, true},
		{"", "", 0, true},
		{"0", "0.00", 0, true},
		{"-100.00", "", 0, true},
		{"", "-5.00", 0, true},
		{"-100.00", "5.00", 0, true},
		{"5.00", "-1.00", 0, true},
		{"-0.01", "5.00", 0, true},
		{"1,00", "", 0, true},
	} {
		got, err := ledgerAmount(tt.debit, tt.credit)
		check(t, "ledgerAmount("+tt.debit+", "+tt.credit+")", got, err, tt.want, tt.bad)
	}
}

func TestParseCurrency(t *testing.T) {
	for _, tt := range []struct {
		in, want string
		bad      bool
	}{
		{"EUR", "EUR", false}, {"eur", "EUR", false}, {"Usd", "USD", false}, {"gbp", "GBP", false},
		{"JPY", "", true}, {"EURO", "", true}, {"", "", true},
		{"uſd", "", true}, {"ÜSD", "", true}, // look-alike letters must not pass
	} {
		got, err := parseCurrency(tt.in)
		check(t, "parseCurrency("+tt.in+")", got, err, tt.want, tt.bad)
	}
}

func TestNormalizeRef(t *testing.T) {
	for in, want := range map[string]string{
		"INV-1042":                     "INV1042",
		"inv 1042":                     "INV1042",
		"Payment ref: INV/1042 (ACME)": "PAYMENTREFINV1042ACME",
		"ınv10023":                     "NV10023", // look-alike letters are dropped, not converted
		"ſ":                            "",
		"aAzZ09":                       "AAZZ09",
		"`{@[/:":                       "", // characters just outside the accepted ranges
		"":                             "",
	} {
		check(t, "normalizeRef("+in+")", normalizeRef(in), nil, want, false)
	}
}
