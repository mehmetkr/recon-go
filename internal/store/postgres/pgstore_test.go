//go:build integration

package postgres_test

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	tcpg "github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/store"
	"github.com/mehmetkr/recon-go/internal/store/postgres"
)

func setupPg(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	ctr, err := tcpg.Run(ctx, "postgres:17",
		tcpg.WithDatabase("recon_test"),
		tcpg.WithUsername("recon"),
		tcpg.WithPassword("recon"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(60*time.Second),
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ctr.Terminate(context.Background()) })

	connStr, err := ctr.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatal(err)
	}

	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)

	if err := postgres.Migrate(connStr); err != nil {
		t.Fatal(err)
	}
	return pool
}

func TestPgStoreRoundTrip(t *testing.T) {
	pool := setupPg(t)
	ctx := context.Background()
	st := postgres.New(pool)

	state, err := st.LoadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(state.Runs) != 0 || len(state.Matches) != 0 {
		t.Fatal("fresh database should return empty state")
	}

	if err := state.Lock("UTC", []string{"2006-01-02"}, []string{"2006-01-02"}); err != nil {
		t.Fatal(err)
	}

	run := store.Run{
		RunID:      "run-001",
		BankSHA:    "aaa",
		LedgerSHA:  "bbb",
		ConfigHash: "ccc",
	}
	matches := []domain.Match{
		{MatchID: "m1", BankID: "b1", LedgerID: "l1", Rule: domain.RuleExact},
		{MatchID: "m2", BankID: "b2", LedgerID: "l2", Rule: domain.RuleDateTolerance},
	}
	if err := state.AddRun(run, matches); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveState(ctx, state); err != nil {
		t.Fatal(err)
	}

	loaded, err := st.LoadState(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if loaded.TZ != "UTC" {
		t.Errorf("TZ = %q, want UTC", loaded.TZ)
	}
	if len(loaded.Runs) != 1 || loaded.Runs[0].RunID != "run-001" {
		t.Errorf("want 1 run with ID run-001, got %+v", loaded.Runs)
	}
	if loaded.Runs[0].NewMatches != 2 {
		t.Errorf("NewMatches = %d, want 2", loaded.Runs[0].NewMatches)
	}
	if len(loaded.Matches) != 2 {
		t.Fatalf("want 2 matches, got %d", len(loaded.Matches))
	}
	m1 := loaded.Matches["m1"]
	if m1.BankID != "b1" || m1.LedgerID != "l1" || m1.Rule != domain.RuleExact || m1.RunID != "run-001" {
		t.Errorf("match m1 = %+v", m1)
	}

	if !loaded.HasRun("run-001") {
		t.Error("HasRun should return true for run-001")
	}
}

func TestPgStoreIdempotentSave(t *testing.T) {
	pool := setupPg(t)
	ctx := context.Background()
	st := postgres.New(pool)

	state, _ := st.LoadState(ctx)
	state.Lock("UTC", []string{"2006-01-02"}, []string{"2006-01-02"})
	state.AddRun(store.Run{RunID: "run-001", BankSHA: "a", LedgerSHA: "b", ConfigHash: "c"},
		[]domain.Match{{MatchID: "m1", BankID: "b1", LedgerID: "l1", Rule: domain.RuleExact}})

	if err := st.SaveState(ctx, state); err != nil {
		t.Fatal(err)
	}
	if err := st.SaveState(ctx, state); err != nil {
		t.Fatal("second save should be idempotent:", err)
	}

	loaded, _ := st.LoadState(ctx)
	if len(loaded.Runs) != 1 || len(loaded.Matches) != 1 {
		t.Errorf("duplicate save created extra rows: runs=%d matches=%d", len(loaded.Runs), len(loaded.Matches))
	}
}

func TestPgStoreMultipleRuns(t *testing.T) {
	pool := setupPg(t)
	ctx := context.Background()
	st := postgres.New(pool)

	state, _ := st.LoadState(ctx)
	state.Lock("Europe/Berlin", []string{"02/01/2006"}, []string{"2006-01-02"})
	state.AddRun(store.Run{RunID: "r1", BankSHA: "a1", LedgerSHA: "b1", ConfigHash: "c1"},
		[]domain.Match{{MatchID: "m1", BankID: "b1", LedgerID: "l1", Rule: domain.RuleExact}})
	st.SaveState(ctx, state)

	state2, _ := st.LoadState(ctx)
	state2.AddRun(store.Run{RunID: "r2", BankSHA: "a2", LedgerSHA: "b2", ConfigHash: "c2"},
		[]domain.Match{{MatchID: "m2", BankID: "b2", LedgerID: "l2", Rule: domain.RuleFuzzyReference}})
	st.SaveState(ctx, state2)

	final, _ := st.LoadState(ctx)
	if len(final.Runs) != 2 || len(final.Matches) != 2 {
		t.Fatalf("want 2 runs and 2 matches, got runs=%d matches=%d", len(final.Runs), len(final.Matches))
	}
	if final.Runs[0].RunID != "r1" || final.Runs[1].RunID != "r2" {
		t.Errorf("run order = [%s, %s], want [r1, r2]", final.Runs[0].RunID, final.Runs[1].RunID)
	}
	if final.TZ != "Europe/Berlin" {
		t.Errorf("TZ = %q, want Europe/Berlin", final.TZ)
	}
	bank, ledger := final.MatchedIDs()
	if !bank["b1"] || !bank["b2"] || !ledger["l1"] || !ledger["l2"] {
		t.Errorf("MatchedIDs missing entries: bank=%v ledger=%v", bank, ledger)
	}
}
