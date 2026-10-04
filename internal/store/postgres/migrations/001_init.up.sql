CREATE TABLE settings (
    id         BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (id),
    tz         TEXT NOT NULL,
    bank_layouts   TEXT[] NOT NULL,
    ledger_layouts TEXT[] NOT NULL
);

CREATE TABLE runs (
    run_id      TEXT PRIMARY KEY,
    bank_sha    TEXT NOT NULL,
    ledger_sha  TEXT NOT NULL,
    config_hash TEXT NOT NULL,
    new_matches INTEGER NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE matches (
    match_id  TEXT PRIMARY KEY,
    bank_id   TEXT NOT NULL UNIQUE,
    ledger_id TEXT NOT NULL UNIQUE,
    rule      TEXT NOT NULL,
    run_id    TEXT NOT NULL REFERENCES runs(run_id)
);
