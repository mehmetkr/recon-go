package domain

import (
	"encoding/json"
	"fmt"
	"time"
)

const isoDate = "2006-01-02"

// Date is a calendar day, counted from 1970 so days compare simply.
type Date int32

// DateOf returns the calendar day of a moment, in its own time zone.
func DateOf(t time.Time) Date {
	y, m, d := t.Date()
	return Date(time.Date(y, m, d, 0, 0, 0, 0, time.UTC).Unix() / 86400)
}

// ParseDate reads a YYYY-MM-DD date.
func ParseDate(s string) (Date, error) {
	t, err := time.Parse(isoDate, s)
	if err != nil {
		return 0, err
	}
	return DateOf(t), nil
}

// Time returns the start of the day in UTC.
func (d Date) Time() time.Time {
	return time.Unix(int64(d)*86400, 0).UTC()
}

func (d Date) String() string {
	return d.Time().Format(isoDate)
}

func (d Date) MarshalJSON() ([]byte, error) {
	return json.Marshal(d.String())
}

func (d *Date) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	parsed, err := ParseDate(s)
	if err != nil {
		return fmt.Errorf("invalid date %q: %w", s, err)
	}
	*d = parsed
	return nil
}
