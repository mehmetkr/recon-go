package ingest

import (
	"errors"
	"fmt"
	"path"
	"strings"
	"time"
	_ "time/tzdata" // built-in time zone data, for machines without their own
)

// Config describes how to read one export.
type Config struct {
	// Layouts are the accepted date formats, tried in order.
	Layouts []string
	// Location is the time zone the bookings are made in.
	Location *time.Location
}

// DefaultLayouts are the date formats used when none are given.
func DefaultLayouts() []string {
	return []string{"2006-01-02", time.RFC3339}
}

// Validate rejects settings that would lead to wrong dates.
func (c Config) Validate() error {
	if c.Location == nil {
		return errors.New("no time zone configured")
	}
	if len(c.Layouts) == 0 {
		return errors.New("no date layouts configured")
	}
	for _, l := range c.Layouts {
		if strings.TrimSpace(l) == "" {
			return errors.New("empty date layout")
		}
		// Zone abbreviations are ambiguous and would silently shift dates.
		if strings.Contains(l, "MST") {
			return fmt.Errorf("date layout %q uses a zone abbreviation; use a numeric offset (-0700 or Z07:00)", l)
		}
	}
	return nil
}

// LoadZone finds a named time zone, refusing names that depend on the machine.
func LoadZone(name string) (*time.Location, error) {
	switch name {
	case "":
		return nil, errors.New("time zone must not be empty")
	}
	// Other spellings of the machine's own zone are refused too.
	if name == "Local" || path.Clean(name) != name || path.Base(name) == "localtime" {
		return nil, fmt.Errorf("time zone %q is machine-dependent or not a canonical IANA name; use e.g. UTC or Europe/Berlin", name)
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("unknown time zone %q: %w", name, err)
	}
	return loc, nil
}
