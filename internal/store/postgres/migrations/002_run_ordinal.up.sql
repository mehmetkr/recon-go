ALTER TABLE runs ADD COLUMN ordinal INTEGER;

UPDATE runs SET ordinal = sub.rn
FROM (SELECT run_id, ROW_NUMBER() OVER (ORDER BY created_at, run_id) - 1 AS rn FROM runs) sub
WHERE runs.run_id = sub.run_id;

ALTER TABLE runs ALTER COLUMN ordinal SET NOT NULL;

ALTER TABLE runs ADD CONSTRAINT runs_ordinal_unique UNIQUE (ordinal);
