# Skills review (BE + FE)

Sources reviewed (16 Oct 2026):

1. [samber/cc-skills-golang](https://github.com/samber/cc-skills-golang) — real Golang agent skills (MIT)
2. [Ebazhanov/linkedin-skill-assessments-quizzes](https://github.com/Ebazhanov/linkedin-skill-assessments-quizzes) — a dump of LinkedIn quiz questions (AGPL-3.0)

## Short verdict

| Source | Use as an agent skill? | Reason |
|---|---|---|
| cc-skills-golang | **Yes, selectively** | Built for agents; idiomatic Go content; public evals |
| LinkedIn quizzes | **No** | Not an agent skill; many outdated questions and class components; some wrong answers; AGPL; its purpose ("pass the LinkedIn quiz") does not help build a scheduler |

## 1. samber/cc-skills-golang

### Added to `.claude/skills/` (15 skills)

Pinned commit: see `.claude/skills/cc-skills-golang.SOURCE`. Claude Code and Cursor both read `.claude/skills/`, so no copy is kept in `.cursor/`.

| Skill | Needed for this task? | Review notes |
|---|---|---|
| `golang-how-to` | Yes | Orchestrator that picks the supporting skills |
| `golang-code-style` | Yes | Clear > clever; matches lead review |
| `golang-naming` | Yes | Package/API naming |
| `golang-error-handling` | Yes | `errors.As`, domain error codes |
| `golang-context` | Yes | HTTP → service → DB |
| `golang-concurrency` | Yes | Concurrent booking test, no goroutine leaks |
| `golang-database` | Yes | pgx, TX, parameterized SQL — **see the override below** |
| `golang-testing` | Yes | Table-driven + integration |
| `golang-observability` | Yes | slog + Prometheus in the plan |
| `golang-security` | Yes | SQL injection, HTTP input |
| `golang-project-layout` | Yes | `cmd/` + `internal/` |
| `golang-safety` | Yes | nil, slice aliasing, map races |
| `golang-structs-interfaces` | Yes | Domain structs, JSON tags |
| `golang-lint` | Yes (added 17 Oct) | The repo runs a fairly strict golangci setup (`backend/.golangci.yml`, gosec/modernize); the skill other skills reference most |
| `golang-troubleshooting` | Yes (added 17 Oct) | Debugging flaky concurrency tests / races; root-cause process before fixing |

### Rejected (not copied)

| Skill | Why it was dropped |
|---|---|
| `golang-grpc`, `golang-graphql` | Outside the V1 stack |
| OpenAPI / Swagger UI | Hand-written OpenAPI 3 + embed (`/api/v1/docs`) — no swag codegen |
| `golang-cli`, `golang-spf13-cobra`, `golang-spf13-viper` | No CLI work |
| `golang-google-wire`, `golang-uber-dig`, `golang-uber-fx`, `golang-samber-do` | No DI framework used |
| `golang-samber-lo/mo/ro/oops/hot/slog` | Vendor bias; the project does not depend on samber libs |
| `golang-benchmark`, `golang-performance` | Not at the optimisation stage yet (at review time). Capacity measurement was added later as `scripts/bench.sh` against this repo's own SLOs; these skills were still not adopted. |
| `golang-continuous-integration`, `golang-gopls`, `golang-modernize`, `golang-refactoring`, `golang-documentation`, `golang-design-patterns`, `golang-dependency-*`, `golang-data-structures`, `golang-popular-libraries`, `golang-pkg-go-dev`, `golang-stay-updated`, `golang-stretchr-testify` | Useful in general but not required for V1; avoids skill bloat |

### Mandatory override for `golang-database`

The original skill stresses `SELECT … FOR UPDATE` to prevent races. **Correct for row updates**, **not enough** for interval booking (bay/tech + `tstzrange`).

In this repo, the hard guarantee is a PostgreSQL **exclusion constraint** (`EXCLUDE USING gist`). FOR UPDATE does not replace it. This override lives in `backend/AGENTS.md` (always loaded when working in `backend/`), not only in the skill.

## 2. LinkedIn skill assessments quizzes

### Why the repo / quizzes were not added to `.claude/skills`

1. **Wrong format** — Markdown Q&A, with no agent workflow (when to load, what to do).
2. **Wrong purpose** — practice for LinkedIn badges, not a standard for production code.
3. **Untrustworthy quality** — crowd-sourced; for example, the Go quiz explains `append` with the sentence "slices in GO are immutable" (**wrong**: the slice header and backing array can be mutated; `append` may reallocate).
4. **React quiz does not match the stack** — many questions on class components / `super(props)`; the project uses React 19 function components + hooks.
5. **AGPL-3.0 licence** — copying the quizzes verbatim into an MIT/challenge repo needs legal consideration; not worth it, since the content was already rejected.

### LinkedIn topics that *are relevant* but are covered by other sources

| Quiz topic | Handling |
|---|---|
| `go/` | Use cc-skills-golang (added) |
| `reactjs/`, `javascript/`, `html/`, `css/`, `front-end-development/` | `web/AGENTS.md` (this task's FE rules for React 19 + Vite + TS) |
| `rest-api/`, `json/` | Covered in the HTTP handlers + `scheduler-orchestration` |
| `git/`, `mysql/`, `mongodb/`, … | Out of scope |

## 3. Project agent configuration (17 Oct 2026)

One shared source for Claude Code and Cursor; the `.cursor/` directory has been removed.

| Component | Location | Who reads it |
|---|---|---|
| Shared rules | `AGENTS.md`, `backend/AGENTS.md`, `web/AGENTS.md` | Cursor reads them directly (including the copies in subdirectories); Claude reads them via `CLAUDE.md` → `@AGENTS.md` |
| Claude-only notes | `CLAUDE.md` | Claude |
| Skills | `.claude/skills/` | Claude + Cursor |
| Hooks | `.claude/settings.json` + `.claude/hooks/` | Claude: gofmt after editing a file, sync the two OpenAPI copies, report files >1000 lines, ask before editing an existing migration |
| Subagent | `.claude/agents/invariant-reviewer.md` | Claude: reviews backend diffs against the booking invariants |

The old `file-size.mdc` and the `web-react-typescript` skill have been merged into `AGENTS.md` / `web/AGENTS.md`.

### Project-written skills

| Skill | Role |
|---|---|
| `scheduler-orchestration` | Booking flow, lifecycle, outbox/WS; invariants in `backend/AGENTS.md` |
| `verify` | Picks the check level: fmt → lint → unit → integration → full cluster |
| `add-migration` | Next-numbered migration, SMALLINT, GiST, matching code updates |
| `add-api-endpoint` | Checklist: route → handler → domain errors → OpenAPI → TS → tests |
| `run-stack` | Runs the compose stack, ports, demo accounts, smoke |

## 4. How agents use them

When writing/editing Go in `backend/`: follow `backend/AGENTS.md`, load `golang-how-to` to pick the relevant Go skills, and load `scheduler-orchestration` when touching booking/store/worker.

When writing/editing `web/`: follow `web/AGENTS.md`.

Do not load the LinkedIn quizzes. Do not pull in more samber vendor skills unless the user explicitly asks.
