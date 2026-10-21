# EdgePilot — API Guide

The machine-readable contract is
[`packages/shared/openapi/openapi.yaml`](../packages/shared/openapi/openapi.yaml)
(OpenAPI 3.1). It is the source of truth: the dashboard's TypeScript client is
generated from it, and CI contract-tests the Go router against it, so an endpoint
that drifts fails the build.

This document covers the parts a spec cannot express well — why the API is shaped
the way it is, and how the flows fit together.

**Base URL:** `https://<host>/api/v1`

---

## 1. Conventions

### Authentication

All endpoints require `Authorization: Bearer <access_token>` except `POST
/auth/login`, `POST /auth/refresh`, `POST /agent/enroll` (enrollment token), and
the health endpoints.

Access tokens live 15 minutes. Refresh tokens are opaque, single-use, and
delivered as an `HttpOnly; Secure; SameSite=Strict` cookie. The refresh token is
never in a response body, and neither token belongs in `localStorage` — see
[Security.md](Security.md) §2.2.

### Tenancy is implicit

Every request is scoped to the caller's organization by their token. There is no
`org_id` parameter anywhere in the API, and one cannot be supplied. A resource in
another organization returns **404, not 403** — a 403 would confirm it exists.

### Errors — RFC 9457

```json
{
  "type": "https://edgepilot.visionify.ai/problems/insufficient-permission",
  "title": "Insufficient permission",
  "status": 403,
  "detail": "This action requires command:destructive.",
  "instance": "/api/v1/devices/3f9a.../commands",
  "request_id": "018f2a...",
  "errors": [
    { "pointer": "/params/delay_seconds", "message": "must be <= 3600" }
  ]
}
```

`Content-Type: application/problem+json`. `request_id` appears in the response
headers, the server logs, and the audit log — quote it in a bug report and the
whole path is recoverable.

| Status | Meaning here |
|---|---|
| 400 | Malformed request |
| 401 | Missing, expired, or invalid credentials |
| 403 | Authenticated, but lacking the permission |
| 404 | Absent, **or** in another organization |
| 409 | State conflict — duplicate slug, invalid transition, checksum mismatch |
| 422 | Well-formed but semantically invalid (usually a `params_schema` failure) |
| 429 | Rate limited; see `Retry-After` |
| 503 | Device offline, or a dependency is down |

### Pagination is cursor-based

```
GET /devices?limit=50
→ { "data": [...], "next_cursor": "eyJpZCI6...", "has_more": true }
GET /devices?limit=50&cursor=eyJpZCI6...
```

Offset pagination is deliberately absent. Every list in this API is backed by a
table receiving concurrent inserts, and `OFFSET` on such a table silently skips
and duplicates rows between pages.

The cursor encodes the sort key plus the ID as a tiebreaker. Changing `sort`
invalidates a cursor; the server returns 400 rather than silently reordering.

### Idempotency

Mutating endpoints accept `Idempotency-Key: <uuid>`. Replaying a key within 24 h
returns the original response without acting again. Use it for anything that
fans out to devices.

### Time and IDs

RFC 3339, UTC, `Z` suffix. IDs are UUIDs; devices and commands use UUIDv7, so
`ORDER BY id` is chronological.

---

## 2. Authentication flow

```
POST /auth/login {email, password[, mfa_code]}
  → 200 {access_token, expires_in: 900}
    Set-Cookie: edgepilot_refresh=<opaque>; HttpOnly; Secure; SameSite=Strict
  → 428 when MFA is enrolled and no code was supplied

… 15 minutes later …

POST /auth/refresh          (cookie sent automatically)
  → 200 {access_token}      old refresh token revoked, new one set
```

Refresh tokens rotate on every use. Presenting an already-rotated token means it
leaked, so the **entire family** is revoked and every session from that login
ends immediately. Clients must therefore serialise refreshes — two concurrent
refreshes with the same token will log the user out. The generated client does
this with a single-flight promise.

`GET /auth/me` returns the principal with **fully resolved permissions**, so the
UI gates controls on `perms.includes('command:destructive')` rather than
re-deriving from role names.

---

## 3. Device enrollment

```
Admin                       Server                      Agent (first boot)
  │ POST /enrollment-tokens    │                              │
  │───────────────────────────►│                              │
  │◄─── ep_enroll_… (once) ────│                              │
  │                            │                              │
  │      token delivered in the .pkg or at /etc/edgepilot/enroll
  │                            │◄──── POST /agent/enroll ─────│
  │                            │      {device_id, csr, facts} │
  │                            │───── 201 {certificate} ─────►│
  │                            │◄════ mTLS gRPC :9443 ═══════►│
```

The plaintext enrollment token is returned exactly once, at creation. Only its
SHA-256 is stored.

With `auto_approve: false` (the production default) the device connects and
reports inventory but receives **no commands** until
`POST /devices/{id}/approve`.

---

## 4. Commands

### The catalogue drives the UI

`GET /command-definitions` returns each command with a JSON Schema for its
parameters. The dashboard renders the parameter form directly from that schema,
so adding a command needs no frontend change.

```json
{
  "slug": "restart_service",
  "name": "Restart Service",
  "category": "maintenance",
  "required_permission": "command:execute",
  "requires_approval": false,
  "is_destructive": false,
  "params_schema": {
    "type": "object",
    "additionalProperties": false,
    "required": ["label"],
    "properties": {
      "label": { "type": "string", "pattern": "^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$" },
      "domain": { "enum": ["system", "gui"], "default": "system" }
    }
  }
}
```

`additionalProperties: false` everywhere: an unrecognised parameter is a rejected
command, not an ignored field.

### Enqueue

```http
POST /commands
Idempotency-Key: 018f2a3b-...

{
  "slug": "flush_dns",
  "device_ids": ["3f9a...", "7c2b..."]
}
```

```json
202 { "correlation_id": "…", "commands": [ {...}, {...} ] }
```

One command per device, sharing a `correlation_id`. Alternatively supply
`device_selector` with a label expression instead of an explicit list.

Destructive commands require `reason`, which is recorded in the audit log.

### Lifecycle

```
pending_approval ──approve──► queued ──► dispatched ──► running ──► succeeded
       │                         │            │            │      └─► failed
       └──reject──► rejected     │            │            └─────────► timed_out
                                 └── cancel ──┴──► cancelled
                                 └── expires_at reached ──► expired
```

Poll `GET /commands/{id}`, or subscribe to `command:<uuid>` on the WebSocket.

### Custom scripts

Deliberately a separate endpoint with a separate permission
(`POST /commands/custom-script`), because it always requires a second approver
and uploads a payload. The full control chain is in [Security.md](Security.md)
§4.2. The relevant API consequence: the command sits in `pending_approval` and a
**different** user must call `POST /commands/{id}/approve`. Self-approval is
rejected by a database trigger, not merely by the service layer.

---

## 5. Metrics

```http
GET /devices/{id}/metrics?from=2026-07-01T00:00:00Z&metrics=cpu,memory&resolution=auto
```

Bucketing happens server-side. Windows up to 48 h read raw 30-second samples;
anything wider reads the hourly rollup. The response reports which was used:

```json
{
  "resolution": "1h",
  "source": "hourly",
  "series": [
    { "name": "cpu_percent", "unit": "percent",
      "points": [[1751328000000, 12.4], [1751331600000, null], ...] }
  ]
}
```

**A `null` value is a genuine gap** — the device was offline or not reporting.
Render it as a break in the line. Interpolating across it invents data that
contradicts the offline record.

`temperature_c` is `null` on hardware with no readable sensor. It is never `0`,
and the UI must show "unavailable" rather than a plausible-looking zero.

---

## 6. Files

Uploads and downloads are **two-phase**, so multi-gigabyte transfers never occupy
API server memory or connections:

```
POST /devices/{id}/files          → 201 { id, upload_url }
PUT  <upload_url>                 (client → blob storage, directly)
POST /files/{id}/commit           → 202  server verifies checksum, dispatches to agent
                                   … progress over the WebSocket …
GET  /files/{id}                  → { status: "available" }
```

Downloads are asynchronous in the same shape: the agent uploads to blob storage,
and `GET /files/{id}/content` returns a short-lived signed URL.

`GET /files/{id}/content` returns the URL in a **200 body** rather than issuing a
302. A cross-origin redirect to blob storage is opaque to `fetch` and fails CORS
preflight on credentialed requests; handing the client the URL avoids both.

Paths are checked against an allowlist and re-checked after symlink resolution.

---

## 7. Deployments

```
POST /packages                          → package
POST /packages/{id}/versions            → 201 { upload_url }
PUT  <upload_url>                       (artifact → blob storage)
POST /package-versions/{id}/commit      → verified, signed, status: ready
POST /deployments                       → 201 draft
POST /deployments/{id}/actions {start}  → running
```

A deployment resolves its target set **when it starts** and freezes it into
`deployment_status`, so a device enrolled mid-rollout is not silently swept in.

`GET /deployments/{id}/status` returns per-device rows plus a `summary` map of
counts by state, which is what the progress UI renders.

`{"action": "rollback"}` reverses using each device's recorded
`previous_version_id` — rollback is per-device, because devices in different
batches were on different versions.

---

## 8. SSH

```http
POST /devices/{id}/ssh/connection
```

```json
{
  "command": "ssh -J ep-bastion.visionify.ai:2222 -i ~/.ssh/edgepilot_ed25519 edgepilot@3f9a1c2e-…",
  "bastion_host": "ep-bastion.visionify.ai",
  "bastion_port": 2222,
  "target": "3f9a1c2e-…",
  "username": "edgepilot",
  "key_fingerprint": "SHA256:tLd7l9…",
  "bastion_host_key": "ssh-ed25519 AAAAC3…"
}
```

**No secret is returned.** The operator authenticates with a key they already
hold; this call authorises them, records the intent, and hands back a command
string. `bastion_host_key` lets the client pin the bastion instead of trusting on
first use.

The `ssh_sessions` row is opened by the bastion when the connection actually
arrives, not by this call — otherwise the history would show sessions that were
never established.

---

## 9. Realtime

```
GET /ws  (Upgrade: websocket, Authorization: Bearer …)

→ {"type":"subscribe","topics":["fleet","device:3f9a…","deployment:7c2b…"]}
← {"type":"device.status","device_id":"3f9a…","data":{"status":"offline"}}
```

**Topics:** `fleet`, `device:<uuid>`, `command:<uuid>`, `deployment:<uuid>`,
`alerts`, `file:<uuid>`.

**Event types:** `device.status`, `device.metrics`, `command.updated`,
`command.output`, `deployment.progress`, `file.progress`, `alert.fired`,
`alert.resolved`.

Two properties worth knowing when building against it:

- **Events carry no data the caller could not fetch over REST.** A stale or
  replayed frame therefore leaks nothing, and authorisation is re-checked at
  subscribe time and again on reconnect.
- **Events are a cache-invalidation signal, not a state feed.** The dashboard
  invalidates the relevant TanStack Query key rather than mutating its cache from
  the payload, so a missed frame or a reconnect self-heals on the next fetch.
  Building a UI that treats the socket as the source of truth will drift.

---

## 10. Rate limits

| Scope | Limit |
|---|---|
| `POST /auth/login` | 5 / 15 min per account **and** per source IP |
| Authenticated, general | 600 / min per user |
| `POST /commands` | 60 / min per user |
| Uploads | 20 concurrent per org |

Responses carry `X-RateLimit-Limit`, `X-RateLimit-Remaining`,
`X-RateLimit-Reset`, and `Retry-After` on 429.

---

## 11. Versioning

The version is in the path (`/api/v1`). Within a version, only additive changes
ship: new endpoints, new optional request fields, new response fields. Removing a
field, tightening validation, or changing a default is a new version.

`buf breaking` guards the device-plane protobuf, and `redocly` guards this spec,
both in CI. Agent compatibility is a separate axis: `command_definitions.min_agent_version`
lets a new command reject dispatch to an old agent at enqueue time, with a clear
error, instead of producing an unknown-slug failure on the device.
