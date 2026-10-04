// Command recon-cli reconciles a bank statement against a ledger and writes results.json.
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

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/ingest"
	"github.com/mehmetkr/recon-go/internal/match"
	"github.com/mehmetkr/recon-go/internal/recon"
	"github.com/mehmetkr/recon-go/internal/store"
	"github.com/mehmetkr/recon-go/internal/store/postgres"
)

// matchFunc is the CLI alias for the matcher signature.
type matchFunc = recon.MatchFunc

// Exit codes.
const (
	exitOK    = 0
	exitFatal = 1
	exitUsage = 2
)

func main() {
	os.Exit(run(interruptContext(), os.Args[1:], match.Run, os.Stdout, os.Stderr))
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
		fmt.Fprintln(os.Stderr, "recon-cli: interrupted; stopping (press Ctrl-C again to abort)")
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
	storeType, databaseURL                   string
	force                                    bool
	tz                                       string
	bankLayouts, ledgerLayouts               layouts
	params                                   match.Params
	workers                                  int
}

func parseFlags(args []string, stderr io.Writer) (options, error) {
	var o options
	fs := flag.NewFlagSet("recon-cli", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&o.bankPath, "bank", "", "bank statement CSV (required)")
	fs.StringVar(&o.ledgerPath, "ledger", "", "ledger export CSV (required)")
	fs.StringVar(&o.outPath, "out", "results.json", `report file, or "-" for stdout`)
	fs.StringVar(&o.statePath, "state", "state.json", "file remembering earlier runs and their matches")
	fs.StringVar(&o.storeType, "store", "file", `store backend: "file" or "postgres"`)
	fs.StringVar(&o.databaseURL, "database-url", "", "PostgreSQL connection string (required when -store=postgres)")
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
	switch o.storeType {
	case "file":
		if o.statePath == "" {
			return o, errors.New("-state must be a file path")
		}
		if err := checkPaths(o); err != nil {
			return o, err
		}
	case "postgres":
		if o.databaseURL == "" {
			if v := os.Getenv("DATABASE_URL"); v != "" {
				o.databaseURL = v
			} else {
				return o, errors.New("-database-url is required when -store=postgres")
			}
		}
	default:
		return o, fmt.Errorf("unknown -store %q (use file or postgres)", o.storeType)
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

func run(ctx context.Context, args []string, matcher matchFunc, stdout, stderr io.Writer) int {
	log := slog.New(slog.NewTextHandler(stderr, nil))

	o, err := parseFlags(args, stderr)
	if errors.Is(err, flag.ErrHelp) {
		return exitOK
	}
	if err != nil {
		fmt.Fprintln(stderr, "recon-cli:", err)
		return exitUsage
	}
	loc, err := ingest.LoadZone(o.tz)
	if err != nil {
		fmt.Fprintln(stderr, "recon-cli:", err)
		return exitUsage
	}
	bankCfg := ingest.Config{Layouts: o.bankLayouts, Location: loc}
	ledgerCfg := ingest.Config{Layouts: o.ledgerLayouts, Location: loc}
	for _, c := range []ingest.Config{bankCfg, ledgerCfg} {
		if err := c.Validate(); err != nil {
			fmt.Fprintln(stderr, "recon-cli:", err)
			return exitUsage
		}
	}

	if err := reconcile(ctx, o, bankCfg, ledgerCfg, matcher, stdout, log); err != nil {
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

func reconcile(ctx context.Context, o options, bankCfg, ledgerCfg ingest.Config, matcher matchFunc, stdout io.Writer, log *slog.Logger) error {
	st, cleanup, err := buildCLIStore(ctx, o, log)
	if err != nil {
		return err
	}
	defer cleanup()

	state, err := st.LoadState(ctx)
	if err != nil {
		return err
	}
	if err := state.Lock(o.tz, bankCfg.Layouts, ledgerCfg.Layouts); err != nil {
		if o.storeType == "file" {
			return fmt.Errorf("%s: %w", o.statePath, err)
		}
		return err
	}
	bankRaw, err := readUTF8(o.bankPath)
	if err != nil {
		return err
	}
	ledgerRaw, err := readUTF8(o.ledgerPath)
	if err != nil {
		return err
	}
	bank, bankErrs, err := ingest.Parse(bytes.NewReader(bankRaw), domain.Bank, bankCfg)
	if err != nil {
		return fmt.Errorf("%s: %w", o.bankPath, err)
	}
	ledger, ledgerErrs, err := ingest.Parse(bytes.NewReader(ledgerRaw), domain.Ledger, ledgerCfg)
	if err != nil {
		return fmt.Errorf("%s: %w", o.ledgerPath, err)
	}

	svc := recon.Service{Matcher: matcher}
	out, err := svc.Reconcile(ctx, state, recon.Input{
		BankRaw:       bankRaw,
		LedgerRaw:     ledgerRaw,
		Bank:          bank,
		Ledger:        ledger,
		BankErrs:      bankErrs,
		LedgerErrs:    ledgerErrs,
		TZ:            o.tz,
		BankLayouts:   bankCfg.Layouts,
		LedgerLayouts: ledgerCfg.Layouts,
		Params:        o.params,
		Workers:       o.workers,
		Force:         o.force,
	})
	if err != nil {
		return err
	}
	if out.Skipped {
		log.Info("already reconciled; nothing written (use -force to run again)", "run_id", out.Report.RunID)
		return nil
	}
	// The report is saved first: if it cannot be written, the state stays as it was.
	if err := writeReport(ctx, o.outPath, out.Encoded, stdout); err != nil {
		return err
	}
	if err := st.SaveState(ctx, state); err != nil {
		return err
	}
	rep := out.Report
	log.Info("reconciled",
		"run_id", rep.RunID,
		"bank", rep.Summary.Read[domain.Bank], "ledger", rep.Summary.Read[domain.Ledger],
		"matched", len(rep.Matches),
		"exceptions", len(rep.Exceptions),
		"malformed", len(rep.Malformed),
		"excluded_bank", rep.Summary.Excluded[domain.Bank], "excluded_ledger", rep.Summary.Excluded[domain.Ledger],
		"out", o.outPath)
	return nil
}

func buildCLIStore(ctx context.Context, o options, log *slog.Logger) (store.Store, func(), error) {
	switch o.storeType {
	case "postgres":
		if err := postgres.Migrate(o.databaseURL); err != nil {
			return nil, nil, fmt.Errorf("migrations: %w", err)
		}
		log.Info("migrations applied")
		pool, err := pgxpool.New(ctx, o.databaseURL)
		if err != nil {
			return nil, nil, fmt.Errorf("database connection: %w", err)
		}
		return postgres.New(pool), pool.Close, nil
	case "file":
		return store.FileStore{Path: o.statePath}, func() {}, nil
	default:
		return nil, nil, fmt.Errorf("unknown store type %q", o.storeType)
	}
}
