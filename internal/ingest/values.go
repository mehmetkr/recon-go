package ingest

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/mehmetkr/recon-go/internal/domain"
)

var amountPattern = regexp.MustCompile(`^([+-]?)([0-9]+)(?:\.([0-9]{1,2}))?$`)

// parseAmount reads an amount into whole cents, without rounding.
func parseAmount(s string) (int64, error) {
	m := amountPattern.FindStringSubmatch(s)
	if m == nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	sign, whole, frac := m[1], m[2], m[3]
	for len(frac) < 2 {
		frac += "0"
	}
	cents, err := strconv.ParseInt(sign+whole+frac, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("amount %q out of range", s)
	}
	return cents, nil
}

// ledgerAmount turns the debit and credit columns into cash in or out.
func ledgerAmount(debit, credit string) (int64, error) {
	d, err := optionalAmount(debit)
	if err != nil {
		return 0, fmt.Errorf("debit: %w", err)
	}
	c, err := optionalAmount(credit)
	if err != nil {
		return 0, fmt.Errorf("credit: %w", err)
	}
	switch {
	case d > 0 && c > 0:
		return 0, errors.New("both debit and credit are set")
	case d > 0:
		return d, nil
	case c > 0:
		return -c, nil
	default:
		return 0, errors.New("neither debit nor credit is set")
	}
}

func optionalAmount(s string) (int64, error) {
	if s == "" {
		return 0, nil
	}
	v, err := parseAmount(s)
	if err != nil {
		return 0, err
	}
	if v < 0 {
		return 0, fmt.Errorf("negative value %q", s)
	}
	return v, nil
}

// offsetProbe helps tell a plain date from a timestamp with its own time zone.
var offsetProbe = time.FixedZone("probe", 3600)

// parseDate keeps plain dates as written and moves timestamps into the booking zone.
func parseDate(s string, cfg Config) (domain.Date, error) {
	for _, layout := range cfg.Layouts {
		utc, err := time.ParseInLocation(layout, s, time.UTC)
		if err != nil {
			continue
		}
		t := utc
		probe, err := time.ParseInLocation(layout, s, offsetProbe)
		if err == nil && utc.Equal(probe) {
			// The input named its own time zone.
			t = utc.In(cfg.Location)
		}
		// Dates outside years 1 to 9999 cannot be written back out.
		if y := t.Year(); y < 1 || y > 9999 {
			return 0, fmt.Errorf("date %q is outside years 0001-9999", s)
		}
		return domain.DateOf(t), nil
	}
	return 0, fmt.Errorf("date %q matches none of the layouts %q", s, cfg.Layouts)
}

var currencyPattern = regexp.MustCompile(`^[A-Za-z]{3}$`)

// allowedCurrencies are the supported currencies, all counted in cents.
var allowedCurrencies = map[string]bool{"EUR": true, "GBP": true, "USD": true}

func parseCurrency(s string) (string, error) {
	// Check the letters before capitalising, so look-alike letters never pass.
	if !currencyPattern.MatchString(s) {
		return "", fmt.Errorf("unsupported currency %q", s)
	}
	c := strings.ToUpper(s)
	if !allowedCurrencies[c] {
		return "", fmt.Errorf("unsupported currency %q", s)
	}
	return c, nil
}

// normalizeRef keeps only plain letters and digits, in capitals.
func normalizeRef(s string) string {
	b := make([]byte, 0, len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z':
			b = append(b, c-'a'+'A')
		case c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			b = append(b, c)
		}
	}
	return string(b)
}
