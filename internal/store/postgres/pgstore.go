// Package postgres implements store.Store backed by PostgreSQL.
package postgres

import (
	"context"
	"embed"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/mehmetkr/recon-go/internal/domain"
	"github.com/mehmetkr/recon-go/internal/store"
)

//go:embed migrations/*.sql
var migrations embed.FS

// Migrations returns the embedded migration files.
func Migrations() embed.FS { return migrations }

// PgStore implements store.Store with PostgreSQL.
type PgStore struct {
	pool *pgxpool.Pool
}

var _ store.Store = (*PgStore)(nil)

// New returns a PgStore backed by the given connection pool.
func New(pool *pgxpool.Pool) *PgStore {
	return &PgStore{pool: pool}
}

// LoadState reconstructs state from the settings, runs and matches tables.
func (s *PgStore) LoadState(ctx context.Context) (*store.State, error) {
	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{AccessMode: pgx.ReadOnly})
	if err != nil {
		return nil, fmt.Errorf("begin read tx: %w", err)
	}
	defer tx.Rollback(ctx)

	st := &store.State{
		Version:  1,
		IDScheme: 1,
		Matches:  map[string]store.Committed{},
	}

	hasSettings := true
	err = tx.QueryRow(ctx,
		`SELECT tz, bank_layouts, ledger_layouts FROM settings WHERE id = TRUE`,
	).Scan(&st.TZ, &st.BankLayouts, &st.LedgerLayouts)
	if err == pgx.ErrNoRows {
		hasSettings = false
	} else if err != nil {
		return nil, fmt.Errorf("loading settings: %w", err)
	}

	rows, err := tx.Query(ctx,
		`SELECT run_id, bank_sha, ledger_sha, config_hash, new_matches FROM runs ORDER BY ordinal`)
	if err != nil {
		return nil, fmt.Errorf("loading runs: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r store.Run
		if err := rows.Scan(&r.RunID, &r.BankSHA, &r.LedgerSHA, &r.ConfigHash, &r.NewMatches); err != nil {
			return nil, fmt.Errorf("scanning run: %w", err)
		}
		st.Runs = append(st.Runs, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating runs: %w", err)
	}

	mrows, err := tx.Query(ctx,
		`SELECT match_id, bank_id, ledger_id, rule, run_id FROM matches`)
	if err != nil {
		return nil, fmt.Errorf("loading matches: %w", err)
	}
	defer mrows.Close()
	for mrows.Next() {
		var matchID string
		var c store.Committed
		if err := mrows.Scan(&matchID, &c.BankID, &c.LedgerID, &c.Rule, &c.RunID); err != nil {
			return nil, fmt.Errorf("scanning match: %w", err)
		}
		st.Matches[matchID] = c
	}
	if err := mrows.Err(); err != nil {
		return nil, fmt.Errorf("iterating matches: %w", err)
	}

	if !hasSettings && (len(st.Runs) > 0 || len(st.Matches) > 0) {
		return nil, fmt.Errorf("database has runs or matches but no settings row")
	}
	if bank, ledger := st.MatchedIDs(); len(bank) != len(st.Matches) || len(ledger) != len(st.Matches) {
		return nil, fmt.Errorf("database pairs a record more than once")
	}

	return st, nil
}

// SaveState persists the state by upserting settings and inserting new runs
// and matches. Everything is written in a single transaction.
func (s *PgStore) SaveState(ctx context.Context, state *store.State) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin: %w", err)
	}
	defer tx.Rollback(ctx)

	_, err = tx.Exec(ctx,
		`INSERT INTO settings (tz, bank_layouts, ledger_layouts)
		 VALUES ($1, $2, $3)
		 ON CONFLICT (id) DO UPDATE SET tz = $1, bank_layouts = $2, ledger_layouts = $3`,
		state.TZ, state.BankLayouts, state.LedgerLayouts)
	if err != nil {
		return fmt.Errorf("upserting settings: %w", err)
	}

	for i, r := range state.Runs {
		_, err = tx.Exec(ctx,
			`INSERT INTO runs (run_id, bank_sha, ledger_sha, config_hash, new_matches, ordinal)
			 VALUES ($1, $2, $3, $4, $5, $6)
			 ON CONFLICT (run_id) DO NOTHING`,
			r.RunID, r.BankSHA, r.LedgerSHA, r.ConfigHash, r.NewMatches, i)
		if err != nil {
			return fmt.Errorf("inserting run %s: %w", r.RunID, err)
		}
	}

	for matchID, c := range state.Matches {
		_, err = tx.Exec(ctx,
			`INSERT INTO matches (match_id, bank_id, ledger_id, rule, run_id)
			 VALUES ($1, $2, $3, $4, $5)
			 ON CONFLICT (match_id) DO NOTHING`,
			matchID, c.BankID, c.LedgerID, c.Rule, c.RunID)
		if err != nil {
			return fmt.Errorf("inserting match %s: %w", matchID, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// SaveReport stores report JSON alongside the run that produced it.
func (s *PgStore) SaveReport(ctx context.Context, runID string, data []byte) error {
	tag, err := s.pool.Exec(ctx,
		`UPDATE runs SET report_data = $1 WHERE run_id = $2`,
		data, runID)
	if err != nil {
		return fmt.Errorf("saving report %s: %w", runID, err)
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("%w: run %s", domain.ErrNotFound, runID)
	}
	return nil
}

// GetReport retrieves a previously stored report by run ID.
func (s *PgStore) GetReport(ctx context.Context, runID string) ([]byte, error) {
	var data []byte
	err := s.pool.QueryRow(ctx,
		`SELECT report_data FROM runs WHERE run_id = $1`, runID,
	).Scan(&data)
	if err == pgx.ErrNoRows {
		return nil, fmt.Errorf("%w: run %s", domain.ErrNotFound, runID)
	}
	if err != nil {
		return nil, fmt.Errorf("reading report %s: %w", runID, err)
	}
	if data == nil {
		return nil, fmt.Errorf("%w: report %s", domain.ErrNotFound, runID)
	}
	return data, nil
}
