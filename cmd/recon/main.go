// Command recon reconciles a bank statement against a ledger and writes results.json.
package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"unicode/utf8"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/ingest"
	"github.com/mehmetkr/recon-go/internal/match"
	"github.com/mehmetkr/recon-go/internal/report"
	"github.com/mehmetkr/recon-go/internal/store"
)

// runMatch is the matcher, replaceable in tests.
var runMatch = match.Run

// Exit codes.
const (
	exitOK    = 0
	exitFatal = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(interruptContext(), os.Args[1:], os.Stdout, os.Stderr))
}

// interruptContext stops the run on the first Ctrl-C and quits at once on the second.
func interruptContext() context.Context {
	if signal.Ignored(os.Interrupt) {
		return context.Background()
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	go func() {
		<-ctx.Done()
		stop()
		fmt.Fprintln(os.Stderr, "recon: interrupted; stopping (press Ctrl-C again to abort)")
	}()
	return ctx
}

// layouts collects date formats given one flag at a time.
type layouts []string

func (l *layouts) String() string { return strings.Join(*l, ",") }
func (l *layouts) Set(v string) error {
	*l = append(*l, v)
	return nil
}

type options struct {
	bankPath, ledgerPath, outPath, statePath string
	force                                    bool
	tz                                       string
	bankLayouts, ledgerLayouts               layouts
	params                                   match.Params
	workers                                  int
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("recon", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.bankPath, "bank", "", "bank statement CSV (required)")
	fs.StringVar(&o.ledgerPath, "ledger", "", "ledger export CSV (required)")
	fs.StringVar(&o.outPath, "out", "results.json", `report file, or "-" for stdout`)
	fs.StringVar(&o.statePath, "state", "state.json", "file remembering earlier runs and their matches")
	fs.BoolVar(&o.force, "force", false, "reconcile again even if these inputs and settings were already reconciled")
	fs.StringVar(&o.tz, "tz", "UTC", "IANA booking time zone")
	fs.Var(&o.bankLayouts, "bank-date-layout", "Go date layout for the bank file (repeatable; replaces the defaults)")
	fs.Var(&o.ledgerLayouts, "ledger-date-layout", "Go date layout for the ledger file (repeatable; replaces the defaults)")
	fs.IntVar(&o.params.DateTolerance, "date-tolerance", 3, "N: days for the date_tolerance pass")
	fs.IntVar(&o.params.FuzzyWindow, "fuzzy-window", 7, "M: days for the fuzzy_reference pass (> N)")
	fs.IntVar(&o.workers, "workers", runtime.GOMAXPROCS(0), "maximum concurrent work units")
	if err := fs.Parse(args); err != nil {
		return o, err
	}
	if fs.NArg() > 0 {
		return o, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	if o.bankPath == "" || o.ledgerPath == "" {
		return o, errors.New("-bank and -ledger are required")
	}
	if o.outPath == "" {
		return o, errors.New(`-out must be a file path or "-"`)
	}
	if o.statePath == "" {
		return o, errors.New("-state must be a file path")
	}
	if err := checkPaths(o); err != nil {
		return o, err
	}
	if len(o.bankLayouts) == 0 {
		o.bankLayouts = ingest.DefaultLayouts()
	}
	if len(o.ledgerLayouts) == 0 {
		o.ledgerLayouts = ingest.DefaultLayouts()
	}
	if err := o.params.Validate(); err != nil {
		return o, err
	}
	if o.workers < 1 {
		return o, errors.New("-workers must be at least 1")
	}
	return o, nil
}

// checkPaths refuses to write the report or the state over an input, or over each other.
func checkPaths(o options) error {
	for _, in := range []string{o.bankPath, o.ledgerPath} {
		if o.outPath != "-" && sameFile(o.outPath, in) {
			return fmt.Errorf("-out %s is also an input file", o.outPath)
		}
		if sameFile(o.statePath, in) {
			return fmt.Errorf("-state %s is also an input file", o.statePath)
		}
	}
	if o.outPath != "-" && sameFile(o.outPath, o.statePath) {
		return errors.New("-out and -state must be different files")
	}
	return nil
}

// sameFile reports whether two paths name the same file, existing or not.
func sameFile(a, b string) bool {
	if store.Resolve(a) == store.Resolve(b) {
		return true
	}
	fa, errA := os.Stat(a)
	fb, errB := os.Stat(b)
	return errA == nil && errB == nil && os.SameFile(fa, fb)
}

// setAside splits records into those still free and those paired in an earlier run.
func setAside(records []domain.Transaction, matched map[string]bool) (free, taken []domain.Transaction) {
	for _, t := range records {
		if matched[t.ID] {
			taken = append(taken, t)
		} else {
			free = append(free, t)
		}
	}
	return free, taken
}

func run(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))

	o, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, "recon:", err)
		return exitUsage
	}
	loc, err := ingest.LoadZone(o.tz)
	if err != nil {
		fmt.Fprintln(stderr, "recon:", err)
		return exitUsage
	}
	bankCfg := ingest.Config{Layouts: o.bankLayouts, Location: loc}
	ledgerCfg := ingest.Config{Layouts: o.ledgerLayouts, Location: loc}
	for _, c := range []ingest.Config{bankCfg, ledgerCfg} {
		if err := c.Validate(); err != nil {
			fmt.Fprintln(stderr, "recon:", err)
			return exitUsage
		}
	}

	if err := reconcile(ctx, o, bankCfg, ledgerCfg, stdout, log); err != nil {
		log.Error("run failed", "err", err)
		return exitFatal
	}
	return exitOK
}

// writeReport saves the report, unless the run was interrupted first.
func writeReport(ctx context.Context, outPath string, out []byte, stdout io.Writer) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if outPath != "-" {
		if err := store.WriteAtomic(ctx, outPath, out); err != nil {
			return fmt.Errorf("writing report %s: %w", outPath, err)
		}
		return nil
	}
	if _, err := stdout.Write(out); err != nil {
		return err
	}
	return ctx.Err()
}

// readUTF8 reads an input file, insisting on UTF-8 so rows can be echoed faithfully.
func readUTF8(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if !utf8.Valid(b) {
		return nil, fmt.Errorf("%s: not valid UTF-8; convert the export to UTF-8 first", path)
	}
	return b, nil
}

func reconcile(ctx context.Context, o options, bankCfg, ledgerCfg ingest.Config, stdout io.Writer, log *slog.Logger) error {
	state, err := store.Load(o.statePath)
	if err != nil {
		return err
	}
	// Settings that shape record identities must match the ones the state was built with.
	if err := state.Lock(o.tz, bankCfg.Layouts, ledgerCfg.Layouts); err != nil {
		return fmt.Errorf("%s: %w", o.statePath, err)
	}
	bankRaw, err := readUTF8(o.bankPath)
	if err != nil {
		return err
	}
	ledgerRaw, err := readUTF8(o.ledgerPath)
	if err != nil {
		return err
	}
	cfg := store.Config{
		AlgoVersion:   store.AlgoVersion,
		TZ:            o.tz,
		BankLayouts:   bankCfg.Layouts,
		LedgerLayouts: ledgerCfg.Layouts,
		DateTolerance: o.params.DateTolerance,
		FuzzyWindow:   o.params.FuzzyWindow,
		MinRefLen:     match.MinRefLen,
		ThresholdNum:  match.ThresholdNum,
		ThresholdDen:  match.ThresholdDen,
	}
	entry := store.Run{BankSHA: store.FileHash(bankRaw), LedgerSHA: store.FileHash(ledgerRaw), ConfigHash: store.ConfigHash(cfg)}
	entry.RunID = store.RunID(entry.BankSHA, entry.LedgerSHA, entry.ConfigHash)
	if state.HasRun(entry.RunID) && !o.force {
		log.Info("already reconciled; nothing written (use -force to run again)", "run_id", entry.RunID)
		return nil
	}

	bank, bankErrs, err := ingest.Parse(bytes.NewReader(bankRaw), domain.Bank, bankCfg)
	if err != nil {
		return fmt.Errorf("%s: %w", o.bankPath, err)
	}
	ledger, ledgerErrs, err := ingest.Parse(bytes.NewReader(ledgerRaw), domain.Ledger, ledgerCfg)
	if err != nil {
		return fmt.Errorf("%s: %w", o.ledgerPath, err)
	}

	// Records paired in an earlier run are set aside: they never match again.
	matchedBank, matchedLedger := state.MatchedIDs()
	freeBank, takenBank := setAside(bank, matchedBank)
	freeLedger, takenLedger := setAside(ledger, matchedLedger)

	res, err := runMatch(ctx, freeBank, freeLedger, append(takenBank, takenLedger...), o.params, o.workers)
	if err != nil {
		return err
	}
	rep := report.Build(report.Input{
		RunID:     entry.RunID,
		Config:    cfg,
		Read:      map[domain.Source]int{domain.Bank: len(bank), domain.Ledger: len(ledger)},
		Excluded:  map[domain.Source]int{domain.Bank: len(takenBank), domain.Ledger: len(takenLedger)},
		Result:    res,
		Malformed: append(bankErrs, ledgerErrs...),
	})
	out, err := report.Encode(rep)
	if err != nil {
		return err
	}
	if err := state.AddRun(entry, res.Matches); err != nil {
		return err
	}
	// The report is saved first: if it cannot be written, the state file stays as it was.
	if err := writeReport(ctx, o.outPath, out, stdout); err != nil {
		return err
	}
	if err := state.Save(ctx, o.statePath); err != nil {
		return err
	}

	s := rep.Summary
	log.Info("reconciled",
		"run_id", entry.RunID,
		"bank", s.Read[domain.Bank], "ledger", s.Read[domain.Ledger],
		"matched", len(rep.Matches),
		"exceptions", len(rep.Exceptions),
		"malformed", len(rep.Malformed),
		"excluded_bank", s.Excluded[domain.Bank], "excluded_ledger", s.Excluded[domain.Ledger],
		"out", o.outPath)
	return nil
}
