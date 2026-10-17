@AGENTS.md

## Claude Code specifics

Shared rules live in `AGENTS.md` files (root, `backend/`, `web/`) so Cursor reads the same text; `backend/CLAUDE.md` and `web/CLAUDE.md` just import them. Edit the `AGENTS.md` files, not these stubs.

- **Hooks** (`.claude/settings.json`): after every Edit/Write, Go files under `backend/` are `gofmt`-ed, the two OpenAPI copies are synced, and any `.go`/`.ts`/`.tsx` file over 1000 lines is reported back. Editing an existing migration asks for confirmation. The hooks need `jq`; without it they silently do nothing.
- **Project skills**: `/verify [fast|test|full]`, `/run-stack`, `add-migration`, `add-api-endpoint`, `scheduler-orchestration`, plus the vendored `golang-*` set.
- **Subagent** `invariant-reviewer`: use it to review any diff touching `backend/internal/{booking,store,migrate,worker,events,realtime}` against the booking invariants before calling the work done.
