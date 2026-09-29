package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// bigInputs writes enough rows to keep a run busy for a moment.
func bigInputs(t *testing.T, n int) (bank, ledger string) {
	t.Helper()
	dir := t.TempDir()
	var b, l strings.Builder
	b.WriteString(bankHeader)
	l.WriteString(ledgerHeader)
	for i := range n {
		date := fmt.Sprintf("2026-%02d-%02d", 1+i%12, 1+i%28)
		fmt.Fprintf(&b, "ACC-1,%s,%d.00,EUR,PAYMENT REFERENCE INV%08d\n", date, 1+i%500, i)
		fmt.Fprintf(&l, "ACC-1,%s,%d.00,,EUR,INV%08d\n", date, 1+i%500, (i*7)%n)
	}
	bank, ledger = filepath.Join(dir, "bank.csv"), filepath.Join(dir, "ledger.csv")
	if err := os.WriteFile(bank, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(ledger, []byte(l.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	return bank, ledger
}

// child is the real command running in its own process.
type child struct {
	cmd         *exec.Cmd
	interrupted chan struct{} // closed once the child confirms the first Ctrl-C
}

func startChild(t *testing.T, wrap []string, args ...string) *child {
	t.Helper()
	argv := append(append(wrap, testBinary), withState(t, args)...)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Env = append(os.Environ(), "RECON_TEST_RUN_MAIN=1")
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	c := &child{cmd: cmd, interrupted: make(chan struct{})}
	go func() {
		sc := bufio.NewScanner(stderr)
		seen := false
		for sc.Scan() {
			if !seen && strings.Contains(sc.Text(), "press Ctrl-C again") {
				seen = true
				close(c.interrupted)
			}
		}
		io.Copy(io.Discard, stderr)
	}()
	return c
}

func (c *child) wait(t *testing.T) *os.ProcessState {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- c.cmd.Wait() }()
	select {
	case err := <-done:
		var exitErr *exec.ExitError
		if err != nil && !errors.As(err, &exitErr) {
			t.Fatal(err)
		}
		return c.cmd.ProcessState
	case <-time.After(2 * time.Minute):
		c.cmd.Process.Kill()
		t.Fatal("child still running after 2 minutes")
		return nil
	}
}

func killedBySIGINT(st *os.ProcessState) bool {
	ws, ok := st.Sys().(syscall.WaitStatus)
	return ok && ws.Signaled() && ws.Signal() == syscall.SIGINT
}

// interruptOnce starts a run and presses Ctrl-C once, retrying if it came too early.
func interruptOnce(t *testing.T, bank, ledger, out string) *child {
	t.Helper()
	for delay := 200 * time.Millisecond; delay < 5*time.Second; delay *= 2 {
		c := startChild(t, nil, "-bank", bank, "-ledger", ledger, "-out", out)
		time.Sleep(delay)
		c.cmd.Process.Signal(os.Interrupt)
		select {
		case <-c.interrupted:
			return c
		case <-time.After(30 * time.Second):
		}
		if st := c.wait(t); killedBySIGINT(st) {
			continue // too early; try again
		}
		t.Fatal("child did not acknowledge the interrupt")
	}
	t.Fatal("could not deliver an interrupt after main() started")
	return nil
}

func TestSignals(t *testing.T) {
	if testing.Short() {
		t.Skip("spawns the CLI on large inputs")
	}
	if signal.Ignored(os.Interrupt) {
		// Ctrl-C cannot be delivered while this process ignores it.
		t.Skip("SIGINT is ignored for this process")
	}
	bank, ledger := bigInputs(t, 150000)

	t.Run("one interrupt cancels the run", func(t *testing.T) {
		out := filepath.Join(t.TempDir(), "results.json")
		c := interruptOnce(t, bank, ledger, out)
		if st := c.wait(t); st.ExitCode() != exitFatal {
			t.Errorf("exit status %v, want exit %d", st, exitFatal)
		}
		if _, err := os.Stat(out); !os.IsNotExist(err) {
			t.Error("an interrupted run must not write a report")
		}
	})

	t.Run("a second interrupt terminates immediately", func(t *testing.T) {
		c := interruptOnce(t, bank, ledger, filepath.Join(t.TempDir(), "r.json"))
		// The child has confirmed it is ready for the second Ctrl-C.
		c.cmd.Process.Signal(os.Interrupt)
		if st := c.wait(t); !killedBySIGINT(st) {
			t.Errorf("exit status %v, want termination by SIGINT", st)
		}
	})

	t.Run("an ignored SIGINT stays ignored", func(t *testing.T) {
		// This run finishes, so it gets a smaller input.
		bank, ledger := bigInputs(t, 40000)
		out := filepath.Join(t.TempDir(), "results.json")
		// The shell starts the command with Ctrl-C ignored.
		c := startChild(t, []string{"/bin/sh", "-c", `trap "" INT; exec "$0" "$@"`},
			"-bank", bank, "-ledger", ledger, "-out", out)
		time.Sleep(300 * time.Millisecond)
		c.cmd.Process.Signal(os.Interrupt)
		if st := c.wait(t); st.ExitCode() != exitOK {
			t.Errorf("exit status %v, want %d", st, exitOK)
		}
		if _, err := os.Stat(out); err != nil {
			t.Errorf("report missing: %v", err)
		}
	})
}
