# EdgePilot — Contributing

## Setup

```bash
git clone <repo> && cd edgepilot
make setup      # tool versions, git hooks, pnpm install, go mod download
make dev        # Postgres, Redis, Azurite, MailHog
export EP_TEST_DATABASE_URL='postgres://edgepilot:edgepilot@localhost:5432/edgepilot?sslmode=disable'
make test       # everything
```

`internal/testsupport` creates a **scratch database per test** from that DSN and
drops it afterwards, so tests are safe under `t.Parallel()` and cannot corrupt
the data in your dev stack. With the variable unset, integration tests skip with
an explanatory message — except in CI, where a silently skipped integration
suite is indistinguishable from a passing one, so they fail instead.

Required: Go 1.25+, Node 20+, Docker with Compose v2+. `make setup` verifies
versions and fails loudly rather than producing confusing errors later.

---

## Architecture rules

These are enforced in CI (`make lint-arch`), not merely encouraged.

1. **`internal/domain` imports nothing but the standard library.** No Gin, no
   sqlc, no gRPC, no Redis. If a domain type needs a database tag, the design is
   wrong — map in the repository.
2. **Dependencies point inward.** `transport → service → domain ← repository`.
3. **Services take interfaces, never concrete types.** That is what makes them
   testable without a database.
4. **Handlers contain no business logic.** Decode, call a service, encode. A
   handler with an `if` about domain state belongs in a service.
5. **Repositories return domain types**, never sqlc-generated structs or `pgx.Rows`.
   The generated types are an implementation detail of one package; leaking them
   puts `pgtype.UUID` and `pgtype.Timestamptz` into every service signature.
6. **Wiring is explicit.** `cmd/server/main.go` constructs the graph by hand. No
   reflection container — the compiler should check the graph, and the wiring
   should be greppable.

---

## Code standards

### Go

- `gofumpt`, `goimports`, `golangci-lint` with the repo config. CI fails on any
  finding.
- **Errors wrap with context**: `fmt.Errorf("load device %s: %w", id, err)`.
  Never discard an error, never `_ =` without a comment saying why.
- **Domain errors, not HTTP codes**, below the transport layer. One middleware
  maps them.
- **`context.Context` is the first parameter** on anything that does I/O, and it
  is honoured — no ignoring cancellation.
- **No naked goroutines.** Every goroutine has an owner, a shutdown path, and a
  panic recover if it services a connection.
- **Structured logging only.** `slog` with key-value pairs. No `fmt.Printf`, no
  string concatenation into a message.
- **No secrets in logs, ever.** The redactor catches known field names; do not
  rely on it as the only line of defence.

### TypeScript

- Strict mode. `any` requires a comment justifying it.
- **The API client is generated.** Never hand-write a fetch against the API — add
  it to the OpenAPI spec and regenerate.
- Server components by default; `"use client"` only where interactivity requires it.
- TanStack Query for server state. WebSocket events **invalidate** query keys
  rather than mutating caches, so a reconnect self-heals.
- Components from `packages/ui`. Add there before adding to an app.

### SQL

- Every schema change is a migration pair. Schema is never derived from Go structs.
- Queries live in `queries/*.sql` and are compiled by `make sqlc`. Generated code
  is committed and never hand-edited; CI fails if it is stale.
- Migrations are backward-compatible with the previous release — expand/contract.
  Never drop a column in the same deploy as the code that stops writing it.
- Every new index arrives with the query that justifies it, in the migration
  comment. An index without a query is write overhead.
- New `org_id` table → add its RLS policy **and its `GRANT`** in the same
  migration. Enabling RLS grants nothing; the policy only narrows access a grant
  already permits.

---

## Testing

| Layer | Approach | Requirement |
|---|---|---|
| `domain` | Pure unit, table-driven | Every invariant |
| `service` | Fakes for repository interfaces | Every branch of every use case |
| `repository` | Real PostgreSQL, scratch database per test | Every query, plus RLS isolation |
| `transport/http` | `httptest` + real router + fake services | Every status code the spec declares |
| `transport/grpc` | `bufconn` | Reconnect, resume, replay idempotency, backpressure |
| Agent collectors | Golden files in `testdata` | Real captured output, never synthetic |
| End-to-end | Compose + simulator agent | The critical paths |

**Rules**

- A bug fix comes with a test that fails before it and passes after. No exceptions.
- Tests do not use `time.Sleep`. Inject a clock.
- Tests are independent and parallel-safe. No shared fixture mutation.
- Security-relevant behaviour gets an explicit test: cross-tenant access, the
  two-person rule, signature verification, path traversal, token reuse. These are
  the tests that must exist even when the feature is obviously correct today.

**Coverage** is a smell detector, not a target. 80% overall, but the number is
never the argument — a service at 95% with no test for its authorisation branch
is worse than one at 70% that covers it.

---

## Commits and PRs

Conventional Commits:

```
feat(agent): spool durable messages to disk while disconnected
fix(server): revoke the whole refresh family on token reuse
docs(security): state the SSH session-recording limitation
```

Scopes: `server`, `agent`, `dashboard`, `proto`, `api`, `db`, `ci`, `docs`.

A PR states **what changed, why, and how it was verified**. "Ran the tests" is not
a verification note; "enrolled a Mac mini on 15.3, confirmed reconnect after a
90-second network drop" is.

PRs touching authentication, authorization, the command allowlist, signing, or
tenancy require review from a second engineer and an explicit note on the threat
model impact.

---

## CI

Every PR runs:

```
lint          gofumpt · golangci-lint · eslint · prettier
lint-arch     import-boundary enforcement
proto         buf lint · buf breaking (against main)
api           redocly lint · bundle
test          go test ./... -race · vitest
schema        migrations up → down → up on a clean Postgres 16, invariants exercised
security      govulncheck · gosec · trivy · npm audit
build         server, agent (darwin/arm64 + darwin/amd64), dashboard
```

`-race` is not optional. The device plane is heavily concurrent and a data race
there is a fleet-wide bug.

---

## Releases

Semantic versioning, tagged from `main`.

**Server**: build, scan, sign the image, push, roll out with expand/contract
migrations.

**Agent**: build reproducibly, SHA-256, sign with the release key (Key Vault,
HSM-backed), publish to `agent_versions` at `rollout_percent = 0`, then stage
1% → 10% → 50% → 100%, watching `edgepilot_agent_stream_errors_total` and the
canary ring at each step.

An agent release is the highest-risk change in the system — it can brick the
fleet. It never ships on a Friday, and it never ships without the watchdog
rollback path having been tested against the specific build.

---

## Getting help

- Design questions → `docs/Architecture.md` and its decision log (§12).
- "Why is it built this way?" is usually answered there or in
  `docs/Security.md` §11. If it is not, that is a documentation bug worth filing.
