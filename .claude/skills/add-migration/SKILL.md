---
name: add-migration
description: >-
  Add a PostgreSQL schema change to this repo — new numbered SQL file under
  backend/internal/migrate/migrations, SMALLINT enum conventions, GiST
  exclusion constraints for appointments, and the Go/TS/seed/OpenAPI updates
  that must ship with it. Use when adding or changing tables, columns,
  constraints, indexes, or enum values.
paths:
  - "backend/internal/migrate/**"
  - "backend/internal/store/**"
  - "backend/internal/domain/**"
---

# Add a migration

Migrations are embedded (`//go:embed migrations/*.sql`), sorted by filename, and each runs once in its own TX, recorded in `schema_migrations` by filename. Server start runs `migrate.Up`.

## Steps

1. `ls backend/internal/migrate/migrations` and create the **next** number: `NNN_short_name.sql` (3 digits). Never edit an applied file; the PreToolUse hook will ask if you try, and `scripts/check-migrations.sh` (run by `lint.sh` and CI) fails if an existing migration was modified, deleted or renamed.
2. Write forward-only SQL. Use `IF NOT EXISTS` / `IF EXISTS` where cheap so local re-runs after a partial failure stay sane.
3. Conventions:
   - Enum-like columns are `SMALLINT` + `CHECK (col BETWEEN 1 AND n)`, codes from 1, never Postgres `ENUM` or text codes (see `001_schema.sql`). The codes are defined once, in `internal/domain`; Go SQL writes them by name (`store/codes.go`), never as bare numbers.
   - Appointment bounds `timestamptz`; shop hours `time`; IDs `uuid`.
   - Anything that can make two appointments share a bay, technician or vehicle over time **must** stay covered by the `EXCLUDE USING gist (… WITH =, tstzrange(start_at, end_at, '[)') WITH &&) WHERE (status IN (1, 2, 3))` constraints. If you recreate the table or change status codes, recreate all three (`appointments_bay_no_overlap`, `appointments_technician_no_overlap`, `appointments_vehicle_no_overlap`) with the same predicate.
   - Add indexes for new FK / filter columns used by availability or schedule queries.
4. Ship the matching code in the same change:
   - `internal/domain`: new `int16` types/constants + `Name()`/`Valid()`.
   - `internal/store`: SQL + scan functions (parameterized only).
   - `internal/seed`: demo data if the UI needs it.
   - OpenAPI (both copies — the hook syncs them) and `web/src/types.ts` if the field is exposed.
5. Verify: tier **test** from the `verify` skill (fresh `scheduler_test` DB runs all migrations from scratch). If constraints changed, run the concurrent booking test with `-race` and the `invariant-reviewer` agent.
