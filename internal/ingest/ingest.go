// Package ingest turns bank and ledger exports into clean, identified records.
package ingest

import (
	"bufio"
	"bytes"
	"encoding/csv"
	"errors"
	"fmt"
	"io"
	"slices"
	"strconv"
	"strings"

	"github.com/mehmetkr/recon-go/internal/domain"
)

var requiredColumns = map[domain.Source][]string{
	domain.Bank:   {"account", "date", "amount", "currency", "reference"},
	domain.Ledger: {"account", "date", "debit", "credit", "currency", "reference"},
}

var utf8BOM = []byte{0xEF, 0xBB, 0xBF}

// Parse reads one export, reporting bad rows and carrying on.
func Parse(r io.Reader, src domain.Source, cfg Config) ([]domain.Transaction, []domain.RowError, error) {
	required, ok := requiredColumns[src]
	if !ok {
		return nil, nil, fmt.Errorf("unknown source %q", src)
	}
	if err := cfg.Validate(); err != nil {
		return nil, nil, err
	}

	// Remove a leading byte-order mark so the header reads cleanly.
	br := bufio.NewReader(r)
	if b, _ := br.Peek(len(utf8BOM)); bytes.Equal(b, utf8BOM) {
		if _, err := br.Discard(len(utf8BOM)); err != nil {
			return nil, nil, err
		}
	}
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1

	header, err := cr.Read()
	if errors.Is(err, io.EOF) {
		return nil, nil, errors.New("empty file: missing header")
	}
	if err != nil {
		return nil, nil, fmt.Errorf("reading header: %w", err)
	}
	cols, err := mapHeader(header, required)
	if err != nil {
		return nil, nil, err
	}

	var (
		txs     []domain.Transaction
		rowErrs []domain.RowError
	)
	for {
		rec, err := cr.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			var pe *csv.ParseError
			if !errors.As(err, &pe) {
				return nil, nil, fmt.Errorf("reading %s file: %w", src, err)
			}
			// Take the line number from the error itself.
			rowErrs = append(rowErrs, domain.RowError{
				Source: src, Line: pe.StartLine, Reason: domain.RowCSVParse,
				Detail: pe.Err.Error(), Raw: rec,
			})
			continue
		}
		line, _ := cr.FieldPos(0)
		if len(rec) != len(header) {
			rowErrs = append(rowErrs, domain.RowError{
				Source: src, Line: line, Reason: domain.RowFieldCount,
				Detail: fmt.Sprintf("expected %d fields, got %d", len(header), len(rec)),
				Raw:    rec,
			})
			continue
		}
		tx, reason, detail := parseRow(rec, cols, src, cfg)
		if reason != "" {
			rowErrs = append(rowErrs, domain.RowError{
				Source: src, Line: line, Reason: reason, Detail: detail, Raw: rec,
			})
			continue
		}
		tx.Line = line
		tx.Raw = rec
		txs = append(txs, tx)
	}

	assignIDs(txs)
	return txs, rowErrs, nil
}

// mapHeader finds each required column by name.
func mapHeader(header, required []string) (map[string]int, error) {
	cols := make(map[string]int, len(header))
	for i, h := range header {
		name := asciiLower(strings.TrimSpace(h))
		if name == "" {
			continue
		}
		if _, dup := cols[name]; dup {
			return nil, fmt.Errorf("duplicate header column %q", name)
		}
		cols[name] = i
	}
	var missing []string
	for _, name := range required {
		if _, ok := cols[name]; !ok {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required columns: %s", strings.Join(missing, ", "))
	}
	return cols, nil
}

// asciiLower lowercases plain letters only, so look-alike letters never pass.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// parseRow turns one row into a transaction, or explains why it cannot.
func parseRow(rec []string, cols map[string]int, src domain.Source, cfg Config) (domain.Transaction, domain.RowErrorReason, string) {
	get := func(name string) string { return strings.TrimSpace(rec[cols[name]]) }

	account := get("account")
	if account == "" {
		return domain.Transaction{}, domain.RowBadAccount, "account is empty"
	}
	currency, err := parseCurrency(get("currency"))
	if err != nil {
		return domain.Transaction{}, domain.RowBadCurrency, err.Error()
	}
	date, err := parseDate(get("date"), cfg)
	if err != nil {
		return domain.Transaction{}, domain.RowBadDate, err.Error()
	}
	var amount int64
	if src == domain.Bank {
		amount, err = parseAmount(get("amount"))
	} else {
		amount, err = ledgerAmount(get("debit"), get("credit"))
	}
	if err != nil {
		return domain.Transaction{}, domain.RowBadAmount, err.Error()
	}
	if amount == 0 {
		return domain.Transaction{}, domain.RowBadAmount, "amount is zero"
	}
	ref := get("reference")
	return domain.Transaction{
		Source:    src,
		Account:   account,
		Currency:  currency,
		Date:      date,
		Amount:    amount,
		Reference: ref,
		RefNorm:   normalizeRef(ref),
	}, "", ""
}

// assignIDs gives every row an identity that survives reordering the file.
func assignIDs(txs []domain.Transaction) {
	groups := make(map[domain.ContentKey][]int)
	for i, tx := range txs {
		k := tx.ContentKey()
		groups[k] = append(groups[k], i)
	}
	for _, idx := range groups {
		slices.SortFunc(idx, func(a, b int) int {
			if c := slices.Compare(txs[a].Raw, txs[b].Raw); c != 0 {
				return c
			}
			return a - b // identical rows are interchangeable
		})
		for occ, i := range idx {
			tx := &txs[i]
			tx.Occurrence = occ
			tx.ID = domain.Digest(32,
				string(tx.Source), tx.Account, tx.Currency, tx.Date.String(),
				strconv.FormatInt(tx.Amount, 10), tx.RefNorm, strconv.Itoa(occ))
		}
	}
}
