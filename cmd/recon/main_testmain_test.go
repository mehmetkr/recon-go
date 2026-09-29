package main

import (
	"os"
	"path/filepath"
	"testing"
)

// testBinary is this test program's own path, so tests can run it as the real command.
var testBinary string

// testdataDir is the folder holding the reviewed reports.
var testdataDir string

// TestMain runs the tests from a scratch folder, so nothing lands in the source tree.
func TestMain(m *testing.M) {
	// When started by a test, act as the real command.
	if os.Getenv("RECON_TEST_RUN_MAIN") == "1" {
		main()
		return
	}
	exe, err := os.Executable()
	if err != nil {
		panic(err)
	}
	testBinary = exe
	if testdataDir, err = filepath.Abs("testdata"); err != nil {
		panic(err)
	}
	dir, err := os.MkdirTemp("", "recon-cli-test-")
	if err != nil {
		panic(err)
	}
	if err := os.Chdir(dir); err != nil {
		panic(err)
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}
