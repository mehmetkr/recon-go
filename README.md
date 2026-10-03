# recon-go

A bank-to-ledger reconciliation engine. It reads a bank statement CSV and a ledger export CSV, pairs the records that refer to the same real payment, and writes a JSON report listing every match, every unmatched record with a reason, and every malformed row.

## Quick start

```bash
go build -o recon ./cmd/recon

./recon -bank exports/bank.csv -ledger exports/ledger.csv

cat results.json
```

The report lands in `results.json` by default. Pass `-out -` to write it to standard output. A `state.json` file remembers what has already been matched, so the next run only pairs new records.

## Usage

```
recon -bank FILE -ledger FILE [-state state.json] [-out results.json|-]
      [-tz UTC] [-bank-date-layout L]... [-ledger-date-layout L]...
      [-date-tolerance 3] [-fuzzy-window 7] [-workers GOMAXPROCS] [-force]
```

| Flag | Default | Purpose |
|---|---|---|
| `-bank` | (required) | Bank statement CSV |
| `-ledger` | (required) | Ledger export CSV |
| `-out` | `results.json` | Report path, or `-` for stdout |
| `-state` | `state.json` | Run memory: locks settings and tracks matches |
| `-tz` | `UTC` | IANA booking time zone |
| `-bank-date-layout` | `2006-01-02`, RFC 3339 | Go date layout for bank dates (repeatable) |
| `-ledger-date-layout` | `2006-01-02`, RFC 3339 | Go date layout for ledger dates (repeatable) |
| `-date-tolerance` | `3` | Days for the date_tolerance pass (N) |
| `-fuzzy-window` | `7` | Days for the fuzzy_reference pass (M, must be > N) |
| `-workers` | `GOMAXPROCS` | Concurrent work units |
| `-force` | `false` | Re-reconcile even if these inputs were already processed |

Exit codes: 0 (success or short-circuit), 1 (fatal error), 2 (usage error). Malformed rows are reported but do not fail the run.

## How matching works

Records are partitioned by `(account, currency, amount)`. Two records can only match if they share a partition key. Within each partition, three passes run in order:

| Pass | Rule | Eligible when |
|---|---|---|
| 1 | `exact` | Same date, both references at least 5 characters and identical after normalization |
| 2 | `date_tolerance` | Dates within N calendar days |
| 3 | `fuzzy_reference` | Dates within M calendar days and the references are similar (semi-global edit distance at most 1/5 of the shorter reference) |

Each pass sorts candidate edges by cost (date gap, then reference similarity) and walks them from cheapest to most expensive. When two records can be paired at the same cost and their group is unambiguous (all records on each side are content-identical), they are paired by occurrence order. Otherwise the entire group is marked `ambiguous` and removed from later passes.

After matching, every unmatched record gets exactly one reason:

1. `ambiguous`: multiple assignments at equal cost
2. `counterpart_taken`: a potential partner exists but is already matched
3. `no_rule_match`: records in the same partition exist, but no rule applies
4. `no_amount_match`: no record with the same partition key exists

## Run memory

`state.json` remembers every run and every match. On the next run, records already matched in state are set aside and counted as "excluded" rather than matched again. If the same inputs and settings have already been reconciled, the run short-circuits and writes nothing (exit 0). Use `-force` to re-reconcile anyway.

The first run locks the time zone and date layouts into the state file. Changing them requires starting a new state file, because different settings can give the same row a different identity.

## Input format

**Bank CSV:** `account,date,amount,currency,reference`. The amount is signed (negative for outflows, positive for inflows).

**Ledger CSV:** `account,date,debit,credit,currency,reference`. Amount = debit minus credit. Exactly one column must be present and positive.

Both files require a header row. Column names are matched case-insensitively. Extra columns are ignored. A UTF-8 BOM is stripped. Files must be valid UTF-8.

## Design decisions

### int64 cents over a decimal library

All amounts are stored as `int64` cents. There is no floating-point arithmetic anywhere in the pipeline. A library like `shopspring/decimal` was considered and would be preferred for allocation, FX conversion or interest calculations, but reconciliation only needs to compare amounts and compute differences, where integer arithmetic is exact and allocation is not involved.

### Ordered passes over per-record scoring

Each pass runs to completion before the next begins. An exact match always wins over a date-tolerance match, regardless of how the other dimensions look. The alternative (scoring every candidate pair and picking the global minimum) sounds elegant but makes the outcome harder to predict and explain: a small change in one reference can cascade through the entire assignment. Ordered passes keep the reasoning local.

### Partitioning by amount over date windows

Records are grouped by `(account, currency, amount)`. The alternative (grouping by date window and scanning for amount matches) creates large, overlapping windows where a single record appears in many groups. Amount partitioning keeps groups small and disjoint, which also makes concurrency straightforward: partitions are independent.

### Source identity by account, not by file hash

A record's identity is derived from its content (account, date, amount, currency, normalized reference) plus an occurrence index. It does not depend on the file it came from. The alternative (using the file hash as part of the identity) would make the same payment appear as two different records when re-exported, breaking incremental reconciliation.

### Reporting ambiguous matches instead of guessing

When multiple assignments are equally valid (same cost, same content structure), all involved records are marked `ambiguous` with their candidates listed. The alternative (picking one arbitrarily or by ID) would produce a match that looks confident but is not: the next export with one more row could flip the assignment. Reporting the ambiguity lets the operator resolve it with information the engine does not have.

### Debit/credit sign basis

Amounts are the company's cash, with inflow positive. The ledger's debit minus credit gives the same sign as the bank's signed amount for the same real payment. The alternative (keeping debit and credit as separate positive values) would require carrying two fields through the pipeline and matching them against a single signed bank amount, adding complexity without adding information.

### Date representation and time zone

Dates are calendar days stored as `int32` days since 1970-01-01. Timestamps with an explicit offset are converted to the run's time zone; bare dates are taken as written. The alternative (using `time.Time` directly) would carry unused sub-day precision through every comparison and make the date tolerance check depend on hour-of-day rather than calendar distance.

### Semi-global edit distance for reference similarity

Reference similarity uses semi-global Levenshtein alignment: the shorter reference may match anywhere within the longer one. The alternative (global edit distance) penalizes length differences, making `INV10023` and `PAYMENT INV10023 CONFIRMED` look distant even though the invoice number is clearly present.

### File store over a database

Persistence is a JSON file (`state.json`) written atomically. The alternative (PostgreSQL with row-level locking) would be needed for concurrent writers or a query interface, but the tool runs as a single CLI invocation. A file keeps the dependency footprint at zero and makes the state inspectable with any text editor.

## Known limitations

- **Many-to-one matching and fee-netted settlement.** A payout net of fees has no single ledger counterpart at the same amount. It appears as `no_amount_match`.
- **Calendar days, not business days.** Date tolerance counts calendar days. A Friday-to-Monday gap of 3 calendar days is 1 business day.
- **Occurrence index per file.** Identical rows are distinguished by a 0-based occurrence index counted within each file. Moving a row between files can change its identity.
- **IDs tied to settings.** Record IDs depend on the time zone, date layouts and normalization rules. These are locked per state file; changing them requires a new state file.
- **Incremental runs can differ from a single run.** Matches are never revisited across runs. Running over three weekly exports incrementally can produce different pairings than running once over the union of all three.
- **Boilerplate references count as strong evidence.** Containment of any 5+ character substring (even a common word like `TRANSFER`) is treated as the strongest similarity signal. Only the amount and date gates prevent false matches.
- **Short-circuit leaves the report file as-is.** After a short-circuit (exit 0), the `-out` file may belong to a different run. Use `-force` to regenerate.
- **Forced reruns share a run_id.** `-force` re-evaluates the inputs but produces the same `run_id`. Filtering matches by `run_id` returns both the original and forced results.
- **Ambiguity is not re-evaluated.** The fast path does not re-check records that were ambiguous in an earlier run. Use `-force` to re-evaluate them.
- **Absent counterparts are invisible.** A record committed in state but absent from the current input cannot explain a newcomer's `counterpart_taken` status.
- **Single-writer file store.** Concurrent runs against the same state file are not safe.
- **Unterminated CSV quotes consume following lines.** A missing closing quote swallows subsequent lines into one malformed record.
- **Non-ASCII references are dropped.** Reference normalization keeps only ASCII letters and digits. Characters like `ü` or `é` are removed rather than folded.
- **Sequential references count as strong.** `INV1001` vs `INV1002` has edit distance 1, which passes the threshold. Only amount and date prevent a false match.
- **Conservative ambiguity marking.** A group where only one side has content-identical records is marked ambiguous, even when every possible assignment would pair the same records.

## Towards a database store

For concurrent access or a query interface, the file store can be replaced with PostgreSQL:

```sql
CREATE TABLE runs (
    run_id      TEXT PRIMARY KEY,
    bank_sha    TEXT NOT NULL,
    ledger_sha  TEXT NOT NULL,
    config_hash TEXT NOT NULL,
    new_matches INTEGER NOT NULL,
    created_at  TIMESTAMPTZ DEFAULT now()
);

CREATE TABLE matches (
    match_id  TEXT PRIMARY KEY,
    bank_id   TEXT NOT NULL UNIQUE,
    ledger_id TEXT NOT NULL UNIQUE,
    rule      TEXT NOT NULL,
    run_id    TEXT NOT NULL REFERENCES runs(run_id)
);
```

One transaction per run. `ON CONFLICT DO NOTHING` on `matches` gives the same skip-if-exists semantics as the file store. The `UNIQUE` constraints on `bank_id` and `ledger_id` enforce per-side uniqueness at the database level.

## Next steps

- Many-to-one matching for fee-netted settlements
- Business-day calendars
- PostgreSQL store for concurrent access
- HTTP endpoint for integration
- CI pipeline
