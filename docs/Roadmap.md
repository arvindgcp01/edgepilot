# EdgePilot — Implementation Plan

Four phases. Each ends with something demonstrably working end-to-end on real
hardware, not a layer that only makes sense once the next one lands. The
sequencing rule throughout: **build the thing that removes the most uncertainty
first.**

Estimates assume two engineers (one backend/Go, one frontend/infra). They are
calendar estimates for focused work, not a commitment.

---

## Phase 0 — Foundations — **complete**

Not in the original phasing, but everything after it moves slower without it.

| # | Deliverable | State |
|---|---|---|
| 0.1 | Monorepo + toolchain | **Done.** Go 1.25 workspace over three modules; the agent physically cannot import server internals |
| 0.2 | `docker-compose.dev.yml` | **Done.** Postgres 16, Redis 7, Azurite, MailHog |
| 0.3 | Migration runner | **Done.** `epctl migrate up/down/to/status/force`, schema embedded via `//go:embed` |
| 0.4 | `config` package | **Done.** 69 variables, validated at startup, every problem reported at once, key material parsed not copied |
| 0.5 | Structured logging + request ID | **Done.** Redaction by field name *and* by value shape; context-carried request/trace/actor IDs |
| 0.6 | Error taxonomy + Problem Details | **Done.** 12 Kinds → RFC 9457; internal causes never rendered |
| 0.7 | `buf` codegen | **Done.** `make proto` is deterministic; generated code committed and checked for staleness in CI |
| 0.8 | CI | **Done.** `ci.yml` (lint/test/build/security) and `verify.yml` (schema/proto/API/docs) |
| 0.9 | Test harness | **Done.** Postgres: scratch database per test, migrations applied, RLS scoping helper. Redis added in Phase 1.4, its first caller: per-test key prefix, SCAN-based cleanup, never `FLUSHDB` |

**Exit criteria met:** `make lint && make test` green on a clean clone;
`golangci-lint` clean across both modules; `govulncheck` clean.

Redis in the harness was deliberately deferred until Phase 1.4, its first
caller — adding `go-redis` for an uncalled helper would have been speculative.

### What Phase 0 turned up

Four real defects, each found by executing something rather than reading it:

| Defect | Consequence had it shipped |
|---|---|
| RLS policies added without matching `GRANT`s on eight child tables | Every query against them fails with `permission denied` at runtime |
| `BYPASSRLS` assumed to confer table privileges | The cross-org workers (rollup, sweeper, retention) could not read anything |
| `0008` down-migration disabled RLS but never dropped the policies | Rolling one migration back and redeploying — the normal response to a bad release — fails |
| `epctl migrate down 1 -yes` silently ignored `-yes`, and `</dev/null` read as a terminal | A confirmation prompt that could be bypassed, guarding a destructive operation |

Plus two upstream advisories that `govulncheck` caught: a SQL-injection issue in
`pgx` (placeholder confusion with dollar-quoted literals — directly relevant,
since every query here uses `$1` placeholders) and a DoS in `golang.org/x/text`.
Both are fixed by upgrade.

---

## Phase 1 — Authentication, registration, heartbeat, dashboard (2.5–3 weeks)

**Goal: a real Mac appears in a real dashboard and stays green.** This is the
riskiest path in the system, so it goes first.

### 1.1 Database and domain — **complete**
- Migrations `0001`–`0008` — applied, exercised, and reversed against
  PostgreSQL 16; see `Database.md` §6.
- `domain` entities, repository interfaces, invariants. Stdlib only, plus
  `google/uuid` by explicit allowlist in `scripts/lint-arch.sh`.
- Query sets compiled by sqlc from the migration schema, and Postgres
  repositories for organizations, users, roles, sessions, devices, device
  certificates, and enrollment tokens.
- Tenant scoping via `SET LOCAL ROLE edgepilot_app` plus
  `edgepilot.org_id`, applied in one chokepoint every scoped query passes
  through.

**What it turned up**

| Defect | Consequence had it shipped |
|---|---|
| The store connected as a superuser, and superusers are exempt from every RLS policy | **Complete cross-tenant data exposure**, with the policies present and apparently working. Tenant isolation depended on the connection string rather than on code. |
| `Cursor.Encode` called `UnixNano()` on a zero `time.Time`, which overflows | A hostname-ordered cursor round-tripped into a garbage 1754 timestamp, corrupting pagination on that sort |

The first is why `TestDeviceIsInvisibleToAnotherTenant` exercises the repository
API rather than raw SQL: the schema tests already proved the policies were
correct, and they were — they simply were not being applied.

### 1.2 Authentication — **complete**
- Argon2id (RFC 9106 second recommendation), constant-time verification,
  parameter-upgrade-on-login, and a bounded concurrency limiter.
- Access tokens: EdDSA JWS with **no algorithm negotiation** — verification
  calls `ed25519.Verify` unconditionally rather than selecting from `alg`.
- Refresh tokens: opaque 256-bit values, SHA-256 at rest, single-use, with
  family revocation on reuse.
- TOTP (RFC 6238) with secrets sealed in AES-256-GCM bound to the user id.
- `epctl bootstrap` for the first organization and administrator.

**What it turned up**

| Defect | Consequence had it shipped |
|---|---|
| A locked account's `status` stayed `locked` after the timer expired, and `credentialsUsable` refused any non-active status | **A 15-minute lockout became permanent.** Every user who mistyped their password three times would need an administrator to recover them — a rate limit turned into a denial of service against your own users. |
| The rehash-on-login write is tenant-scoped, but the login context has no scope yet — authentication is what establishes it | Cost-parameter increases would silently never apply to existing passwords. The write failed, the warning was swallowed, and login succeeded. |
| Lockout expiry was written with the database's `now()` but evaluated against the service clock | Two clocks deciding one fact. It works in production only because both are wall time, and it cannot be tested deterministically at all. |

The password hasher is also concurrency-bounded: at 64 MiB per hash, a hundred
simultaneous login attempts would request 6.4 GB. Rate limiting does not close
that — the limit is per account and an attacker uses many — so hashing queues
rather than allocating, turning a memory exhaustion into added latency.

**Deliberately deferred:** per-source-IP lockout and the RBAC HTTP middleware
both belong with the transport layer in 1.6; the service-layer permission checks
they will call already exist. Session revocation is behind a `RevocationChecker`
interface that fails closed; the Redis-backed implementation lands with the HTTP
middleware that consults it in 1.6. Until then revocation takes effect at
access-token expiry — a documented 15-minute window rather than a silent gap.
(1.4's `revocation` service is the *certificate* set consulted during the mTLS
handshake, which is a different question with a different failure mode.)

### 1.3 Device CA and enrollment — **complete**
- `epctl ca init` — root plus a path-length-constrained intermediate, keys
  written 0600, with instructions to take the root offline.
- CSR validation and certificate issuance with a SPIFFE URI SAN carrying both
  the device and its tenant.
- Enrollment service: atomic token redemption, device registration, signing, and
  certificate persistence in one transaction.
- Certificate renewal over an already-authenticated identity — no token, no
  human.
- `epctl enrollment-token` for the first token, before anyone can sign in.

**The property this phase is built around**

A CSR is entirely attacker-controlled. It is signed by the key it contains,
which proves possession of that key and *nothing else* — not the subject, not
the SANs, not the extensions. A CA that copies identity out of a CSR issues
certificates for whatever the requester asked to be.

So the CA reads exactly two things from a CSR: the public key, and the signature
proving the requester holds the matching private key. `Sign()` does not accept a
subject parameter at all — identity comes from the device record and the
redeemed token. `TestCSRCannotChooseItsIdentity` submits a CSR naming another
tenant's device and asserts the issued certificate names neither.

**Containment of a leaked enrollment token.** A token holder can register a
device into that token's organization and obtain a certificate for it. They
cannot choose the tenant, impersonate an existing device, obtain anything that
authorises a command (a pending device receives none), or exceed the use count
(redemption is a single atomic statement — `TestSingleUseTokenAdmitsOneDevice`
runs eight concurrent enrollments and asserts exactly one succeeds).

**Verified against openssl, not only against Go.** The issued chain passes
`openssl verify -CAfile root.crt -untrusted intermediate.crt -purpose sslclient`,
so the certificates work with a standard TLS implementation rather than merely
round-tripping through the library that produced them.

**Deferred:** the Redis-backed revocation set belongs with the device plane in
1.4; `ListRevokedFingerprints` already supplies it, and the mTLS callback that
consults it lands with the gRPC listener.

### 1.4 Device plane — **complete** — *the highest-risk component*
- gRPC server on `:9443`, mTLS with `RequireAndVerifyClientCert`, device
  identity taken from the SPIFFE URI SAN and revocation checked in
  `VerifyPeerCertificate` — before the handshake completes, not after.
- Stream manager: one goroutine set per connection, bounded per-stream buffers,
  displacement of a stale connection by a newer one, graceful drain.
- `Hello`/`Welcome` with resume, `Heartbeat`, `Ack`.
- Redis presence with TTL, pub/sub as *notification not queue*, self-renewing
  leases for singleton workers.
- Batched `last_seen_at` flusher and an offline sweeper under a Redis lease.
- **Tests:** `bufconn` streams covering handshake, displacement, backpressure,
  drain, and teardown; 21 worker tests against real PostgreSQL and Redis.

**The backpressure policy.** A bounded outbound queue has to do *something* when
it fills, and the choice is: drop what is perishable, terminate the stream for
anything else. Pings are worthless once late. Everything else — command
dispatch, config, deployment tasks — has a durable record that reconciliation
re-delivers within seconds, so closing a saturated stream costs a reconnect
rather than a lost command. Blocking would stall an operator's HTTP request on
an unresponsive Mac; growing the queue would turn one such Mac into fleet-wide
memory growth.

**Three defects this phase turned up, all found by execution rather than
review:**

1. **A server-initiated close could not actually close the stream.** The handler
   ran its receive loop in the RPC goroutine, blocked in `stream.Recv()` — and
   nothing on the server can interrupt that call. Drain, displacement, and
   revocation all wait for the handler to *return*, so a stalled agent held its
   slot indefinitely. Caught by `TestDrainTellsAgentsToReconnectImmediately`
   timing out. The receive loop now runs in its own goroutine and the handler
   selects over it, `ep.Closed()`, and the RPC context.

2. **`Options.LockTimeout` on the migration runner did nothing.** It was passed
   as an `x-lock-timeout` DSN parameter, which no golang-migrate driver reads —
   silently ignored, leaving the library's 15-second default in place. Under a
   loaded database that is shorter than a real migration, so every replica but
   one fails to start and crash-loops through exactly the deploy that needed the
   migration.

3. **The migration lock was waited on forever at startup.** The driver takes the
   advisory lock while *constructing* the instance, to create the version table,
   outside golang-migrate's timeout machinery entirely — `pg_advisory_lock`
   blocks with no timeout. A replica starting during another's migration hung
   with nothing in its logs. Bounding it takes PostgreSQL's own `lock_timeout`
   on the connection *and* `m.LockTimeout`; neither covers the other. Both now
   surface as `ErrLockUnavailable`, which says plainly that nothing was applied
   and retrying is safe.

### 1.5 Agent skeleton — **complete**
- Identity: UUIDv7, a P-256 key generated on the device, atomic 0600 storage,
  enrollment on first boot, and the token file deleted once it has been used.
- Connection manager: mTLS with the server pinned to the CA bundle from
  enrollment, `Hello`/`Welcome`, resume from `last_seq_seen`, heartbeats,
  ping/pong on a priority queue, and reconnect with full jitter.
- Bounded on-disk spool with the **durable vs perishable** distinction (§3.2 of
  `Architecture.md`): segmented, CRC-checked, crash-safe, and capped.
- Collectors for hostname, OS, hardware facts, and uptime, with the parsing
  separated from the shelling-out so it is testable on Linux.
- LaunchDaemon plist with `KeepAlive` and a 60-second `ThrottleInterval`,
  newsyslog rotation, and SIGHUP reopening — see `deployments/macos/`.
- `simulator` build tag so the whole control-plane path runs in Linux CI.
- **Tests:** 110 across nine packages, including golden-file collector tests and
  the full first-boot path — no identity, enroll, persist, connect — driven
  against a real gRPC server and a real enrollment endpoint.

**The two properties the agent is built around**

*A bounded footprint under an unbounded outage.* A Mac at a customer site can be
offline for six hours, and what it did in that time has to survive without its
disk use becoming a function of how long the network stayed down. Durable
messages — command results, deployment progress, events — go to a segmented
on-disk ring with a 64 MiB cap. Perishable ones — metrics, heartbeats — never
touch it, because a five-minute-old CPU reading has no value and buffering an
hour of them would evict the command results that do. When the cap is reached the
oldest segment is dropped, which is real loss, so it is counted, logged, and
reported to the server rather than being silent.

*Nothing is discarded until the server has it.* Spooled records are sent but not
committed; the commit happens on the server's ack. A record removed on send and
never acknowledged is a lost command result, and no amount of retry logic
recovers what was already deleted.

**What it turned up**

| Defect | Consequence had it shipped |
|---|---|
| The send loop could not learn that the stream had ended | The agent waited on its own writer while the server had already gone. A device would look connected, send nothing, and only notice at shutdown — the same shape as the server-side drain bug in 1.4, from the same cause: waiting on a goroutine that has no way to know. |
| `CloseSend` raced with `Send` | Found by the race detector after the fix above. gRPC permits one writer; calling both at once corrupts the stream's internal state rather than failing cleanly. The send side is now drained before the half-close. |

**Verified across the fleet's hardware, without the hardware.** `system_profiler`
reports `number_processors` as the string `"proc 12:8:4"` on Apple silicon and as
a plain integer on Intel, and puts the chip name under different keys for each.
Code written against one shape reports zero cores for half the fleet. Both are
fixtures, both are tested, and `physical_memory` is parsed as the binary quantity
Apple's decimal-looking units actually mean — reading "16 GB" as 16×10⁹ would put
every device's memory 7% low.

**Deliberately deferred:** command execution, file transfer, tunnels, and metrics
collection are Phase 2. Messages for them are counted and logged rather than
silently dropped — an agent that quietly ignores a command the server believes it
dispatched leaves an operator watching a queue that never moves. Certificate
renewal over the stream is wired at the identity layer (`UpdateCertificate`,
`NeedsRenewal`) and needs the server's `CertificateRequest` handler, which lands
with the rest of the device-plane handlers.

**A gap worth naming:** the agent and the server are separate Go modules, and
Go's `internal` rule stops either from importing the other's packages — which is
the point, since a shared internal package is how a server dependency reaches
10,000 Macs. The consequence is that no test in this phase runs the real agent
against the real server. Each side is tested against the *contract*: the agent
against a gRPC server and an enrollment endpoint implementing the wire protocol,
the server against its own transport tests. They meet in an end-to-end test that
runs both built binaries, which needs `cmd/server` — Phase 1.6.

### 1.6 REST API and dashboard — **complete**

**Done — the Go half**
- `cmd/server`: one process running the API, the device plane, and the workers,
  with startup that fails before a listener opens and shutdown that drains agent
  streams *first* so a rolling deploy is invisible rather than a fleet-wide blink.
- Endpoints: auth (login, refresh, logout, logout-all, change-password, me),
  `/devices`, `/devices/summary`, `/devices/{id}`, approve, reject, decommission,
  enrollment tokens, and `POST /agent/enroll` — the one user-plane endpoint an
  agent calls.
- Cursor pagination, filtering by status, enrollment state, labels, and search,
  with a closed set of sort orders.
- Middleware: request id, real-IP resolution, panic recovery, access logging,
  security headers, CORS allowlist, body cap, per-handler timeout, bearer
  authentication, per-route permission checks.
- WebSocket hub: authenticated upgrade with an Origin allowlist, per-topic
  authorisation at subscribe time, bounded per-client queues.
- 33 tests against real PostgreSQL and a real WebSocket listener.

**Done — the dashboard**
- Next.js 15 app router: login (with MFA and tenant resolution), app shell,
  fleet table with summary cards and live WebSocket status, device detail with
  approve/reject/decommission, enrollment-token management.
- The TypeScript client is **generated** from `openapi.yaml` — 4,487 lines of
  types, never hand-written.
- 12 tests covering the parts that break: binary byte formatting, relative time,
  Problem Details rendering, and the token handling below.

**The property the client is built around.** The access token lives in a module
variable and dies with the page; the refresh token is an `HttpOnly` cookie this
code can never read. An XSS can *use* a session while the page is open, which is
bad, and cannot steal a credential that outlives it, which is much worse. A test
asserts the token appears in neither storage nor `document.cookie`.

Concurrent refreshes share one in-flight request. Refresh tokens are single-use
and presenting a spent one revokes the family — so five requests that all 401
must not spend five tokens and log the operator out for doing nothing wrong.

**Outstanding**
- Redis fanout for the hub. Today it delivers events raised on the same replica,
  which is correct for a single-replica deployment and incomplete for several —
  the bus already carries what it needs, and the subscription is the missing part.
- A contract test pinning the router against `openapi.yaml`, which is what would
  have caught the two divergences below mechanically.

**What it turned up**

| Defect | Consequence had it shipped |
|---|---|
| `trustedProxies` was a package-level variable set by every `New` | Two servers in one process shared one setting and the last one constructed won. Found by the race detector. It decides which address every audit entry and every per-IP rate limit is attributed to, so getting it from another server's configuration is not a cosmetic bug. |
| The API diverged from `openapi.yaml` in two places | The list envelope used `items` where the contract says `data`, and token creation took a `ttl` duration where the contract says `expires_at`. Both were caught by reading the contract rather than by a test — which is exactly why the contract test above is listed as outstanding rather than optional. |
| The test harness opened unbounded connection pools | Twenty-five new parallel tests exhausted PostgreSQL's `max_connections`, and every test in the run failed with "sorry, too many clients already" — a harness problem that reads exactly like a product failure. Capped, but see "a weakness worth naming" below. |
| A connected, approved device showed `status: pending` forever | The liveness statement transitioned only `offline → online`. A freshly enrolled device starts `pending`, and nothing else ever moved it — so **every working device in the fleet table read "pending"**. Found by deploying and looking at it, not by a test. |
| Every access-log line recorded `route=unmatched` | `ServeMux` stamps `Request.Pattern` on the request it dispatches, but middleware that adds context values returns a *new* request, so the outer access log never saw it. Grouping requests by route in a dashboard would have returned exactly one bucket. |
| Nothing could generate the device-plane certificate | `EP_GRPC_CERT_FILE` was documented as required in production with no way to produce it — and it cannot come from a public CA, because agents verify against the CA they were enrolled with. `epctl ca server-cert` now issues it. It must be signed by the **root**: the intermediate is restricted to `clientAuth` so a stolen intermediate cannot impersonate the server, which also means it cannot issue a server certificate. `openssl verify` rejected the first attempt at depth 1. |
| The dashboard fetched before the session existed | Data loading ran from the page body, which React executes before the shell has resolved the session. Requests fired with no token, the server correctly answered 401, and the console filled with failures that looked like a broken API. |
| `new URL()` threw on a same-origin path | `API_BASE` is empty when the API shares an origin, and `new URL("/api/v1/ws")` has no base to resolve against. It took the whole fleet page down rather than degrading to "no live updates". |

**A weakness worth naming.** The Go suite fails under machine load. Each package
creates a scratch database per test and runs under `-race`; with anything else
competing for CPU — a local server, a container build — fixtures start timing out
and a dozen unrelated tests fail at once. Every failure so far has been this, and
every one passed on a quiet machine. It has not been fixed, only reduced, and on
a busy CI runner it will read as flakiness.

**The cookie trade the dashboard forced into the open.** The refresh cookie is
`HttpOnly; Secure; SameSite=Strict`. Those three attributes are why an XSS cannot
steal a long-lived credential — and they also mean a browser will not send the
cookie cross-site, and will not store it at all over plain HTTP. A dashboard
served from a different origin than the API therefore appears to log in and loses
the session on the first reload.

The wrong fix is `SameSite=Lax`. The right one is a single origin: a rewrite in
development, one TLS terminator in production. `deployments/docker/Caddyfile`
enforces it, and says why — including why `:9443` is deliberately *not* behind
that proxy, since terminating mTLS would discard the device's identity.

**Two decisions worth recording**

*How a sign-in resolves its organization.* Credentials are scoped to a tenant, so
a login has to name one — but the request cannot simply be trusted to, or a
caller could aim attempts at any tenant. Resolution is: an explicit slug, then
the request's subdomain, then the only organization when there is exactly one.
An unresolvable tenant fails as `invalid_credentials`, never as "no such
organization", because the second answer tells an unauthenticated caller which
tenants exist. The contract gained an optional `organization` field for the
multi-tenant, single-hostname case.

*Enrollment tokens needed their own permissions* (migration `0009`). Approving a
device is a decision about one machine that has already asked to join; minting an
enrollment token creates a credential that admits machines nobody has seen yet —
and with `auto_approve`, admits them without a second decision. Operator can read
them, only Admin can mint.

**Exit criteria**
- A Mac mini enrolls with a token, appears within 10 s, and goes green.
- Unplug it → `offline` within 90 s → alert row.
- Restart the server → the agent reconnects and resumes.
- Two orgs cannot see each other's devices (verified by test, not inspection).
- Every action so far appears in the audit log with a valid hash chain.

---

## Phase 2 — Monitoring, logs, SSH, commands (3–3.5 weeks)

### 2.1 Metrics (4 days)
- Agent collectors: CPU, memory + pressure, disk, network, load, temperature
  (absent on hardware without a readable sensor — must be `null`, never `0`),
  uptime, top processes.
- Ingest with per-replica batching and `COPY` into partitioned storage.
- Partition maintenance worker (three months ahead) + retention.
- Hourly rollup worker.
- `GET /devices/{id}/metrics` with auto resolution selection.
- Dashboard charts (Recharts) against pre-bucketed series.
- **Tests:** partition rollover, rollup correctness, counter-to-rate derivation
  across a device reboot (counters reset — the naive delta goes negative).

### 2.2 Inventory (3 days)
- Full collectors: system, hardware, network, storage, applications, services,
  security, peripherals.
- Checksum-based skip of unchanged sections.
- Device detail tabs; processes and services views.

### 2.3 Commands (5 days)
- Catalogue, JSON Schema validation server-side **and** agent-side.
- Queue, dispatch, reconciliation, expiry sweeper, cancellation.
- Agent handler table with per-slug implementations; argv built agent-side.
- Custom script path: upload, two-person approval, Ed25519 signing, pinned
  verification, per-device kill switch.
- Result ingest with 256 KiB inline cap and overflow to blob storage.
- Live output streaming to the dashboard.
- **Tests:** the full approval state machine; a signature for device A rejected on
  device B; expired signature rejected; unknown slug rejected; kill switch honoured.

### 2.4 SSH (5 days) — *the second-highest-risk component*
- Bastion on `:2222` (`golang.org/x/crypto/ssh`), public-key auth against
  `ssh_keys`.
- `direct-tcpip` interception, RBAC, presence lookup, replica relay.
- Agent tunnel handler with a loopback-only target allowlist.
- Authorized-key reconciliation with `expiry-time=` and restriction options.
- `sshd` hardening drop-in.
- `ssh_sessions` lifecycle and audit.
- Dashboard: key management, **Copy SSH command**, **Open Terminal**, session
  history.
- **Tests:** end-to-end tunnel through a container running `sshd`; revocation
  removes the key; a non-loopback tunnel target is refused.

### 2.5 Logs (3 days)
- Bundle collection with a mandatory bounded window, compression, SAS upload.
- Live tail streaming (not persisted).
- `device_events` ingest, search, and filtering.
- Dashboard log viewer and download.

**Exit criteria**
- 30-day CPU chart renders in under a second on a device with 30 days of history.
- `ssh -J` gets a real shell; revoking the key kills access on the next push.
- `flush_dns` completes end-to-end and is auditable.
- A custom script requires a second approver and is refused on a kill-switched
  device.

---

## Phase 3 — Deployment, file manager, alerts (2.5–3 weeks)

### 3.1 Object storage (2 days)
- `ObjectStore` interface; Azure Blob driver with SAS; filesystem driver for dev.
- Reservation → direct upload → commit → checksum verification.
- Retention reaper.

### 3.2 File manager (4 days)
- Agent handlers: list, stat, download, upload, delete, rename, mkdir.
- Path allowlist and traversal defence (resolve symlinks, then re-check).
- Two-phase transfer via SAS so bytes never traverse the API server.
- Recursive upload with per-file progress over the WebSocket.
- Dashboard: browser, drag-and-drop, progress bars.

### 3.3 Software deployment (6 days)
- Package and version CRUD; upload, checksum, release-key signing.
- Deployment planner: selector resolution, target freeze, batching.
- Scheduler advancing batches, honouring pauses, evaluating the auto-rollback
  threshold.
- Agent installer: resumable download, signature verification, `.pkg` / `.zip` /
  `.tar.gz` handling, `install_spec` hooks, verification step.
- Rollback using `deployment_status.previous_version_id`.
- Dashboard: package library, rollout wizard, live per-device progress.
- **Tests:** batch advance, failure threshold triggers rollback, resumed download
  after interruption, tampered artifact rejected.

### 3.4 Alerts (3 days)
- Evaluator worker with `duration_seconds` hysteresis and fingerprint dedup.
- All six required rule types plus deployment failure and certificate expiry.
- Webhook and email notification with `renotify_seconds` suppression.
- Dashboard: alert list, acknowledge, resolve, rule editor.

**Exit criteria**
- A `.pkg` deploys to 50 devices in batches of 10 and rolls back on a forced 30%
  failure rate.
- A 2 GB upload does not raise API server memory.
- CPU pinned for 5 minutes fires exactly one alert; it resolves on recovery.

---

## Phase 4 — Auto-update, HA, performance (2–3 weeks)

### 4.1 Agent auto-update (4 days)
- Signed releases, staged rollout by stable UUID hash, `is_yanked` halt.
- Staging path, atomic swap, launchd restart.
- **Watchdog rollback**: if the new binary fails to check in within the window,
  restore the previous one. An agent that cannot phone home cannot be fixed
  remotely, so this must be autonomous.
- **Tests:** rollout cohort stability across polls; watchdog restores on a
  deliberately broken build.

### 4.2 High availability (5 days)
- Multi-replica validation: stream ownership, cross-replica command routing,
  bastion relay.
- Graceful drain: stop accepting new streams, `Disconnect{REASON_SHUTTING_DOWN}`,
  wait for reconnection elsewhere.
- Leader election for singleton workers.
- Postgres read replica for dashboard reads; connection pooling (PgBouncer,
  transaction mode — note this constrains `SET LOCAL`, so RLS scoping must be
  set per transaction, which it already is).
- Kubernetes manifests + Helm chart.
- Chaos tests: kill a replica mid-command, mid-deployment, mid-SSH-session.

### 4.3 Performance (4 days)
- Load test at 10,000 simulated agents (the simulator build makes this cheap).
- Profile the metric ingest path; tune batch sizes.
- Query plan review of every index in `Database.md` §4 against realistic volumes.
- Agent footprint: target < 1% CPU idle, < 50 MB RSS. Measure, do not assume.
- Dashboard: virtualised device table, code splitting, bundle budget.

### 4.4 Operational maturity (3 days)
- Prometheus metrics, Grafana dashboards, alert rules for EdgePilot itself.
- OpenTelemetry traces spanning HTTP → service → agent.
- Backup and restore runbook, tested by actually restoring.
- Disaster recovery drill; documented RTO/RPO.

---

## Ordering rationale

**Why the device plane precedes the dashboard.** The gRPC stream, reconnect,
resume, presence, and replica routing carry nearly all the technical risk. A
dashboard on top of a shaky stream is rework; a stream proven with `curl` and a
CLI is not.

**Why SSH precedes deployment.** SSH is the operator's escape hatch. Once it
works, every subsequent bug is diagnosable on real hardware. Building deployment
first means debugging installer failures with no way in.

**Why alerts come after monitoring and deployment.** Alert thresholds chosen
before seeing real metric distributions are guesses. Two weeks of real data makes
them evidence.

**Why auto-update is last.** It is the one feature that can brick the fleet.
It ships when the watchdog can be tested against everything else already working.

---

## Risk register

| Risk | Impact | Mitigation |
|---|---|---|
| macOS API changes break collectors | Medium | Collectors behind an interface, golden-file tests, no parsing of human-readable output where a `-json`/plist form exists |
| Agent bug bricks the fleet | **Critical** | Staged rollout, watchdog rollback, canary ring, `is_yanked` |
| Temperature unreadable on Apple silicon | Low | Modelled as nullable end to end; the UI shows "unavailable", never `0 °C` |
| Metric volume outruns Postgres | Medium | Partitioning + rollups from day one; TimescaleDB is a drop-in escalation if needed |
| Customer firewall blocks egress | Medium | Single outbound destination; documented; fall back to 443 with ALPN |
| SSH tunnel latency poor over long links | Medium | Measure early in Phase 2; tune window sizes; the tunnel is a byte pipe, so TCP tuning applies |
| PgBouncer transaction pooling vs `SET LOCAL` | Medium | Already scoped per transaction; add an integration test through PgBouncer in Phase 4 |
| Scope creep toward MDM | Medium | The non-goals in `Architecture.md` §1 are the contract |

---

## What is already done

| Artifact | State |
|---|---|
| `docs/Architecture.md` | Complete, with the decision log |
| `docs/Database.md` + `apps/server/migrations/*` | **Verified** — 8 migrations apply, reverse, and re-apply cleanly; 34 invariants exercised |
| `packages/proto/edgepilot/v1/agent.proto` + generated Go | **Verified** — lints, compiles, round-trips; forward compatibility pinned by test |
| `packages/shared/openapi/openapi.yaml` | **Verified** — 60 paths, 75 operations, 46 schemas; `redocly lint` and `bundle` clean |
| `docs/Security.md` | Complete, including stated limitations |
| Phase 0 foundations | **Complete** — see above |
| Phase 1.1–1.5 | **Complete** — domain and repositories, authentication, device CA and enrollment, device plane, macOS agent |
| Phase 1.6 | **Complete** — `cmd/server`, the REST API, the WebSocket hub, and the Next.js dashboard. Deployed and exercised end to end on Azure. |

**Phase 1 is complete.** A real agent enrols across the internet into a live
Azure deployment, appears in the dashboard, and goes offline within 106 seconds
of being stopped — every exit criterion below, verified on a running system
rather than in a test harness.

Next: **Phase 2** — metrics collection, log shipping, SSH access, and command
execution.
