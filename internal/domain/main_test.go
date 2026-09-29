package domain

import (
	"os"
	"testing"
	"time"
)

// TestMain runs the tests far west of UTC, so any reliance on the local clock shows.
func TestMain(m *testing.M) {
	time.Local = time.FixedZone("W-11", -11*3600)
	os.Exit(m.Run())
}
