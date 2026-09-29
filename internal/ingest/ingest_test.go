package ingest

import (
	"fmt"
	"io"
	"math/rand/v2"
	"slices"
	"strings"
	"testing"
	"testing/iotest"
	"time"

	"github.com/google/go-cmp/cmp"

	"github.com/mehmetkr/recon-go/internal/domain"
)

const bankHeader = "account,date,amount,currency,reference\n"
const ledgerHeader = "account,date,debit,credit,currency,reference\n"

func cfgIn(t *testing.T, zone string, layouts ...string) Config {
	t.Helper()
	loc, err := LoadZone(zone)
	if err != nil {
		t.Fatal(err)
	}
	if len(layouts) == 0 {
		layouts = DefaultLayouts()
	}
	return Config{Layouts: layouts, Location: loc}
}

// parse reads an export in UTC with the default layouts.
func parse(t *testing.T, src domain.Source, csv string) ([]domain.Transaction, []domain.RowError) {
	t.Helper()
	txs, rowErrs, err := Parse(strings.NewReader(csv), src, cfgIn(t, "UTC"))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return txs, rowErrs
}

// show renders parsed records and row errors as short comparable lines.
func show(txs []domain.Transaction, rowErrs []domain.RowError) []string {
	var out []string
	for _, tx := range txs {
		out = append(out, fmt.Sprintf("%d %s %s %s %s %d %s", tx.Line, tx.Source, tx.Account, tx.Currency, tx.Date, tx.Amount, tx.RefNorm))
	}
	for _, e := range rowErrs {
		out = append(out, fmt.Sprintf("%d %s %s: %s %q", e.Line, e.Source, e.Reason, e.Detail, e.Raw))
	}
	return out
}

func TestParse(t *testing.T) {
	row := "ACC-1,2026-09-01,1.00,EUR,A\n"
	bare := `bare " in non-quoted-field`
	for _, tt := range []struct {
		name string
		src  domain.Source
		csv  string
		want []string
	}{
		{"bank", domain.Bank, bankHeader + "ACC-1,2026-09-01,-150.00,EUR,INV-1001\nACC-1,2026-09-02,75.5,eur, Payment INV 1002 \n",
			[]string{"2 bank ACC-1 EUR 2026-09-01 -15000 INV1001", "3 bank ACC-1 EUR 2026-09-02 7550 PAYMENTINV1002"}},
		{"ledger debit is money in, credit is money out", domain.Ledger, ledgerHeader +
			"ACC-1,2026-09-01,150.00,,EUR,INV-1001\nACC-1,2026-09-01,,20.00,EUR,FEE\nACC-1,2026-09-01,10.00,0.00,EUR,X\n" +
			"ACC-1,2026-09-01,10.00,5.00,EUR,BOTH\nACC-1,2026-09-01,,,EUR,A\n",
			[]string{"2 ledger ACC-1 EUR 2026-09-01 15000 INV1001", "3 ledger ACC-1 EUR 2026-09-01 -2000 FEE",
				"4 ledger ACC-1 EUR 2026-09-01 1000 X",
				`5 ledger bad_amount: both debit and credit are set ["ACC-1" "2026-09-01" "10.00" "5.00" "EUR" "BOTH"]`,
				`6 ledger bad_amount: neither debit nor credit is set ["ACC-1" "2026-09-01" "" "" "EUR" "A"]`}},
		{"every value is trimmed; account case is kept", domain.Bank, bankHeader +
			" ACC-1 , 2026-09-01 , 1.00 , eur , A \nacc-1,2026-09-01,1.00,EUR,A\n   ,2026-09-01,1.00,EUR,A\n",
			[]string{"2 bank ACC-1 EUR 2026-09-01 100 A", "3 bank acc-1 EUR 2026-09-01 100 A",
				`4 bank bad_account: account is empty ["   " "2026-09-01" "1.00" "EUR" "A"]`}},
		{"padded ledger", domain.Ledger, ledgerHeader + " ACC-1 , 2026-09-01 , 5.00 ,  , EUR , A \n",
			[]string{"2 ledger ACC-1 EUR 2026-09-01 500 A"}},
		{"row validation", domain.Bank, bankHeader + ",2026-09-01,1.00,EUR,A\nACC-1,2026-09-01,1.00,JPY,A\n" +
			"ACC-1,2026-13-01,1.00,EUR,A\nACC-1,2026-09-01,0.00,EUR,A\nACC-1,2026-09-01,1.234,EUR,A\n" +
			"ACC-1,2026-09-01,92233720368547758.08,EUR,A\n",
			[]string{`2 bank bad_account: account is empty ["" "2026-09-01" "1.00" "EUR" "A"]`,
				`3 bank bad_currency: unsupported currency "JPY" ["ACC-1" "2026-09-01" "1.00" "JPY" "A"]`,
				`4 bank bad_date: date "2026-13-01" matches none of the layouts ["2006-01-02" "2006-01-02T15:04:05Z07:00"] ["ACC-1" "2026-13-01" "1.00" "EUR" "A"]`,
				`5 bank bad_amount: amount is zero ["ACC-1" "2026-09-01" "0.00" "EUR" "A"]`,
				`6 bank bad_amount: invalid amount "1.234" ["ACC-1" "2026-09-01" "1.234" "EUR" "A"]`,
				`7 bank bad_amount: amount "92233720368547758.08" out of range ["ACC-1" "2026-09-01" "92233720368547758.08" "EUR" "A"]`}},
		{"unreadable rows are reported and reading continues", domain.Bank, bankHeader +
			"ACC-1,2026-09-01,1.00,EUR,OK1\nACC-1,2026-09-01\nACC-1,2026-09-01,1.00,EUR,A,extra\n" +
			"ACC-1,2026-09-01,1\"0,EUR,A\nA\"CC,2026-09-01,1.00,EUR,A\nACC-1,2026-09-01,2.00,EUR,OK2\n",
			[]string{"2 bank ACC-1 EUR 2026-09-01 100 OK1", "7 bank ACC-1 EUR 2026-09-01 200 OK2",
				`3 bank field_count: expected 5 fields, got 2 ["ACC-1" "2026-09-01"]`,
				`4 bank field_count: expected 5 fields, got 6 ["ACC-1" "2026-09-01" "1.00" "EUR" "A" "extra"]`,
				// An unreadable row keeps whatever was read before the problem.
				`5 bank csv_parse: ` + bare + ` ["ACC-1" "2026-09-01"]`,
				`6 bank csv_parse: ` + bare + ` []`}},
		{"ledger unreadable rows", domain.Ledger, ledgerHeader + "ACC-1,2026-09-01\nA\"C,2026-09-01,1.00,,EUR,A\n",
			[]string{`2 ledger field_count: expected 6 fields, got 2 ["ACC-1" "2026-09-01"]`, `3 ledger csv_parse: ` + bare + ` []`}},
		{"a row spanning lines reports its first line", domain.Bank, bankHeader +
			"ACC-1,2026-09-01,1.00,EUR,\"X\nY\"\nACC-1,2026-09-01,1.00,EUR,\"X\nY\",extra\n",
			[]string{"2 bank ACC-1 EUR 2026-09-01 100 XY", `4 bank field_count: expected 5 fields, got 6 ["ACC-1" "2026-09-01" "1.00" "EUR" "X\nY" "extra"]`}},
		{"a quote never closed swallows the rest", domain.Bank, bankHeader +
			"ACC-1,2026-09-01,1.00,EUR,OK\nACC-1,2026-09-02,1.00,EUR,\"INV1\nACC-1,2026-09-03,1.00,EUR,LOST1\n",
			[]string{"2 bank ACC-1 EUR 2026-09-01 100 OK", `3 bank csv_parse: extraneous or missing " in quoted-field ["ACC-1" "2026-09-02" "1.00" "EUR"]`}},
		{"reordered, case-insensitive header with an extra column", domain.Bank,
			" Reference ,CURRENCY,memo,amount,date,account\nA,EUR,note,1.00,2026-09-01,ACC-1\n",
			[]string{"2 bank ACC-1 EUR 2026-09-01 100 A"}},
		{"byte-order mark", domain.Bank, "\xEF\xBB\xBF" + bankHeader + row, []string{"2 bank ACC-1 EUR 2026-09-01 100 A"}},
		{"byte-order mark before a quoted header", domain.Bank,
			"\xEF\xBB\xBF\"account\",\"date\",\"amount\",\"currency\",\"reference\"\n" + row, []string{"2 bank ACC-1 EUR 2026-09-01 100 A"}},
		{"trailing commas on header and rows", domain.Bank, "account,date,amount,currency,reference,,\nACC-1,2026-09-01,1.00,EUR,A,,\n",
			[]string{"2 bank ACC-1 EUR 2026-09-01 100 A"}},
		{"trailing comma on the header only", domain.Bank, "account,date,amount,currency,reference,\n" + row,
			[]string{`2 bank field_count: expected 6 fields, got 5 ["ACC-1" "2026-09-01" "1.00" "EUR" "A"]`}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if diff := cmp.Diff(tt.want, show(parse(t, tt.src, tt.csv))); diff != "" {
				t.Errorf("(-want +got):\n%s", diff)
			}
		})
	}
}

func TestParseKeepsTheRowAsWritten(t *testing.T) {
	txs, _ := parse(t, domain.Bank, bankHeader+"ACC-1,2026-09-02,75.5,eur, Payment INV 1002 \n")
	want := domain.Transaction{Reference: "Payment INV 1002", Raw: []string{"ACC-1", "2026-09-02", "75.5", "eur", " Payment INV 1002 "}}
	if got := (domain.Transaction{Reference: txs[0].Reference, Raw: txs[0].Raw}); !cmp.Equal(got, want) {
		t.Errorf("got %+v, want %+v", got, want)
	}
}

func TestParseFileErrors(t *testing.T) {
	type fileCase struct {
		name, want string
		src        domain.Source
		cfg        *Config
		in         io.Reader
	}
	valid := cfgIn(t, "UTC")
	row := "ACC-1,2026-09-01,1.00,EUR,A\n"
	cases := []fileCase{
		{"duplicate column", "duplicate", domain.Bank, nil, strings.NewReader("account,date,amount,Amount ,currency,reference\n")},
		{"duplicate column differing in Z", "duplicate", domain.Bank, nil, strings.NewReader("account,date,amount,currency,reference,Zone,zone\n")},
		{"empty file", "empty file", domain.Bank, nil, strings.NewReader("")},
		{"unreadable header", "reading header", domain.Bank, nil, strings.NewReader("account,da\"te,amount,currency,reference\n" + row)},
		{"look-alike letters in the header", "debit, credit", domain.Ledger, nil,
			strings.NewReader("account,date,DEBİT,credİt,currency,reference\nACC-1,2026-09-01,1.00,,EUR,A\n")},
		{"no time zone", "time zone", domain.Bank, &Config{Layouts: DefaultLayouts()}, strings.NewReader(bankHeader + row)},
		{"zone abbreviation", "abbreviation", domain.Bank, &Config{Layouts: []string{"2006-01-02 15:04 MST"}, Location: time.UTC},
			strings.NewReader(bankHeader + row)},
		{"unknown source", "unknown source", "other", nil, strings.NewReader(bankHeader)},
		{"read failure", "unexpected EOF", domain.Bank, nil,
			io.MultiReader(strings.NewReader(bankHeader+row), iotest.ErrReader(io.ErrUnexpectedEOF))},
	}
	for src, header := range map[domain.Source][]string{
		domain.Bank:   {"account", "date", "amount", "currency", "reference"},
		domain.Ledger: {"account", "date", "debit", "credit", "currency", "reference"},
	} {
		for i, name := range header {
			h := slices.Delete(slices.Clone(header), i, i+1)
			cases = append(cases, fileCase{string(src) + " without " + name, name, src, nil, strings.NewReader(strings.Join(h, ",") + "\n")})
		}
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			cfg := valid
			if tt.cfg != nil {
				cfg = *tt.cfg
			}
			txs, rowErrs, err := Parse(tt.in, tt.src, cfg)
			if err == nil || !strings.Contains(err.Error(), tt.want) || txs != nil || rowErrs != nil {
				t.Errorf("got %v, %v, %v; want only an error mentioning %q", txs, rowErrs, err, tt.want)
			}
		})
	}
}

func TestParseDates(t *testing.T) {
	iso := []string{"2006-01-02 15:04"}
	for _, tt := range []struct {
		zone    string
		layouts []string
		in      string
		want    string // empty means the date must be rejected
	}{
		{"UTC", nil, "2024-01-15", "2024-01-15"},
		{"UTC", nil, "2024-01-15T23:30:00-05:00", "2024-01-16"},
		{"America/New_York", nil, "2024-01-15T23:30:00-05:00", "2024-01-15"},
		{"America/New_York", nil, "2024-07-15T23:30:00-05:00", "2024-07-16"},
		{"UTC", nil, "2024-07-15T23:30:00-05:00", "2024-07-16"},
		{"America/New_York", nil, "2024-01-15", "2024-01-15"},
		{"Asia/Tokyo", nil, "2024-01-15", "2024-01-15"},
		// Zones where the clocks skip midnight on these days.
		{"America/Santiago", nil, "2024-09-08", "2024-09-08"},
		{"America/Havana", nil, "2024-03-10", "2024-03-10"},
		{"America/Sao_Paulo", nil, "2018-11-04", "2018-11-04"},
		{"Pacific/Apia", nil, "2011-12-30", "2011-12-30"},
		{"Atlantic/Azores", nil, "2024-03-31", "2024-03-31"},
		{"America/Scoresbysund", nil, "2023-03-26", "2023-03-26"},
		{"Atlantic/Azores", iso, "2024-03-31 00:30", "2024-03-31"},
		{"America/New_York", nil, "2024-01-16T03:00:00+01:00", "2024-01-15"},
		{"America/Santiago", nil, "2024-09-08T00:30:00-04:00", "2024-09-08"},
		{"America/New_York", iso, "2024-01-15 23:30", "2024-01-15"},
		{"America/New_York", nil, "2024-01-16T03:00:00Z", "2024-01-15"},
		{"Asia/Tokyo", nil, "2024-01-15T10:00:00+09:00", "2024-01-15"},
		{"Asia/Tokyo", nil, "2024-01-15T20:00:00Z", "2024-01-16"},
		{"UTC", nil, "2024-02-29", "2024-02-29"},
		{"UTC", nil, "2023-02-29", ""},
		{"UTC", []string{"02/01/2006"}, "03/04/2026", "2026-04-03"},
		{"UTC", []string{"02/01/2006"}, "31/04/2026", ""},
		{"UTC", nil, "03/04/2026", ""},
		// Dates outside years 1 to 9999 cannot be written back out.
		{"UTC", nil, "9999-12-31", "9999-12-31"},
		{"UTC", nil, "9999-12-31T23:00:00-05:00", ""},
		{"UTC", nil, "0000-01-01T00:30:00+01:00", ""},
		{"UTC", nil, "0000-06-01", ""},
	} {
		got, err := parseDate(tt.in, cfgIn(t, tt.zone, tt.layouts...))
		if tt.want == "" && err == nil || tt.want != "" && (err != nil || got.String() != tt.want) {
			t.Errorf("parseDate(%q) in %s = %s, %v; want %q", tt.in, tt.zone, got, err, tt.want)
		}
	}
}

func TestConfigValidation(t *testing.T) {
	for _, name := range []string{"", "Local", "localtime", "./localtime", ".//localtime", "Europe//Berlin", "Mars/Olympus"} {
		if _, err := LoadZone(name); err == nil {
			t.Errorf("LoadZone(%q) was accepted", name)
		}
	}
	loc := cfgIn(t, "Europe/Berlin").Location
	for _, c := range []Config{
		{Layouts: []string{"2006-01-02 15:04 MST"}, Location: loc},
		{Layouts: []string{time.UnixDate}, Location: loc}, // abbreviation mid-format
		{Layouts: nil, Location: loc},
		{Layouts: []string{" "}, Location: loc},
		{Layouts: DefaultLayouts(), Location: nil},
	} {
		if c.Validate() == nil {
			t.Errorf("Validate(%+v) was accepted", c)
		}
	}
	if err := (Config{Layouts: DefaultLayouts(), Location: loc}).Validate(); err != nil {
		t.Errorf("a valid config was rejected: %v", err)
	}
}

// ids renders each record's copy number and identity.
func ids(t *testing.T, src domain.Source, csv string) []string {
	t.Helper()
	txs, _ := parse(t, src, csv)
	var out []string
	for _, tx := range txs {
		out = append(out, fmt.Sprintf("%d %s", tx.Occurrence, tx.ID))
	}
	return out
}

func TestStableIDs(t *testing.T) {
	// Expected identities, computed independently from the published formula.
	const occ0, occ1 = "484f0bcf0feec40e6a2f98aa4a92e23c", "6b699cb23aa6da986266a2aa45baf5f5"
	row := "ACC-1,2026-09-01,-150.00,EUR,INV-1001\n"
	for _, tt := range []struct {
		src       domain.Source
		csv, want string
	}{
		{domain.Bank, bankHeader + row + row, "[0 " + occ0 + " 1 " + occ1 + "]"},
		// The same content written two ways, ordered by how it is written.
		{domain.Bank, bankHeader + row + "ACC-1,2026-09-01,-150.00,EUR,INV 1001\n", "[1 " + occ1 + " 0 " + occ0 + "]"},
		// A credit is money leaving.
		{domain.Ledger, ledgerHeader + "ACC-1,2026-09-01,,150.00,EUR,INV-1001\n", "[0 b0d47823f4ea5a6e7abee7cb11d238d4]"},
	} {
		if got := fmt.Sprint(ids(t, tt.src, tt.csv)); got != tt.want {
			t.Errorf("got %s, want %s", got, tt.want)
		}
	}

	// Rows differing in any part of their content are counted apart.
	got := ids(t, domain.Bank, bankHeader+"ACC-1,2026-09-01,1.00,EUR,A\nACC-1,2026-09-01,1.00,EUR,B\nACC-1,2026-09-02,1.00,EUR,A\n"+
		"ACC-2,2026-09-01,1.00,EUR,A\nACC-1,2026-09-01,1.00,USD,A\nACC-1,2026-09-01,2.00,EUR,A\n")
	for _, id := range got {
		if id[0] != '0' {
			t.Errorf("distinct rows share a copy count: %v", got)
		}
	}
	if got[1] != "0 0acf6c736fe0362d5f25cad542fd5e7c" {
		t.Errorf("identity of the B row = %s", got[1])
	}
}

func TestIDsSurviveReordering(t *testing.T) {
	rows := []string{
		"ACC-1,2026-09-01,-150.00,EUR,INV-1001",
		"ACC-1,2026-09-01,-150.00,EUR,INV 1001", // same content, written differently
		"ACC-1,2026-09-01,-150.00,EUR,INV-1001",
		"ACC-1,2026-09-02,75.50,EUR,INV-1002",
		"ACC-2,2026-09-03,10.00,USD,",
	}
	rawByID := func(lines []string) map[string][]string {
		txs, _ := parse(t, domain.Bank, bankHeader+strings.Join(lines, "\n")+"\n")
		m := map[string][]string{}
		for _, tx := range txs {
			m[tx.ID] = tx.Raw
		}
		return m
	}
	want, rng := rawByID(rows), rand.New(rand.NewPCG(1, 2))
	for range 20 {
		rng.Shuffle(len(rows), func(a, b int) { rows[a], rows[b] = rows[b], rows[a] })
		if diff := cmp.Diff(want, rawByID(rows)); diff != "" {
			t.Fatalf("reordering changed the identities (-want +got):\n%s", diff)
		}
	}
}

func TestSameRecordSameID(t *testing.T) {
	id := func(src domain.Source, zone, csv string) string {
		txs, _, err := Parse(strings.NewReader(csv), src, cfgIn(t, zone))
		if err != nil || len(txs) != 1 {
			t.Fatalf("%q: %v, %v", csv, txs, err)
		}
		return txs[0].ID
	}
	type side struct {
		src       domain.Source
		zone, csv string
	}
	bank := func(row string) side { return side{domain.Bank, "UTC", bankHeader + row + "\n"} }
	ledger := func(row string) side { return side{domain.Ledger, "UTC", ledgerHeader + row + "\n"} }
	stamped := bank("ACC-1,2024-01-15T23:30:00-05:00,1.00,EUR,A")
	inNewYork := stamped
	inNewYork.zone = "America/New_York"
	for _, tt := range []struct {
		a, b side
		same bool
	}{
		{bank("ACC-1,2026-09-01,1.00,EUR,A"), bank("ACC-1,2026-09-01T10:00:00Z,1.00,EUR,A"), true},
		{bank("ACC-1,2026-09-01,100,EUR,INV-1"), bank("ACC-1,2026-09-01,100.00,EUR,inv 1"), true},
		{bank("ACC-1,2026-09-01,1.00,EUR,A"), bank(" ACC-1 , 2026-09-01 , 1.00 , eur , A "), true},
		{ledger("ACC-1,2026-09-01,5.00,,EUR,A"), ledger(" ACC-1 , 2026-09-01 , 5.00 ,  , EUR , A "), true},
		{bank("ACC-1,2026-09-01,1.00,EUR,A"), bank("acc-1,2026-09-01,1.00,EUR,A"), false},
		{bank("ACC-1,2026-09-01,1.00,EUR,A"), ledger("ACC-1,2026-09-01,1.00,,EUR,A"), false},
		// The time zone moves a timestamp's date, which is why the store locks it.
		{stamped, inNewYork, false},
	} {
		if (id(tt.a.src, tt.a.zone, tt.a.csv) == id(tt.b.src, tt.b.zone, tt.b.csv)) != tt.same {
			t.Errorf("%q vs %q: same identity should be %v", tt.a.csv, tt.b.csv, tt.same)
		}
	}
}
