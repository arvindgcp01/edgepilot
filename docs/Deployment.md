# EdgePilot — Deployment

Docker Compose for development and single-node production; Kubernetes-ready by
construction (stateless replicas, external state, no local disk dependency) and
delivered as manifests in Phase 4.

---

## 1. Topology

```
                    ┌──────────────────────────────────┐
   Internet ───────►│ Azure Application Gateway / LB   │
                    │  TLS 1.3 termination, WAF        │
                    └───┬──────────┬─────────┬─────────┘
                    :443│      :9443│    :2222│
                    ┌───▼──────────▼─────────▼─────────┐
                    │ edgepilot-server × N (stateless) │
                    │  REST · gRPC · WS · bastion      │
                    │  workers (leader-elected)        │
                    └───┬──────────┬─────────┬─────────┘
                        │          │         │
              ┌─────────▼──┐  ┌────▼────┐  ┌─▼──────────────┐
              │ PostgreSQL │  │  Redis  │  │ Azure Blob     │
              │ Flexible   │  │ Cache   │  │ packages/logs  │
              │ Server 16  │  │ (HA)    │  │                │
              └────────────┘  └─────────┘  └────────────────┘
```

`:9443` needs **TCP or TLS passthrough**, not HTTP termination — the device plane
requires the client certificate to reach the application for identity extraction.
On Application Gateway that means an L4 listener; terminating TLS there would
discard the client certificate and break device authentication.

`:2222` is likewise plain TCP.

---

## 2. Sizing

| Fleet | Replicas | Postgres | Redis | Notes |
|---|---|---|---|---|
| ≤ 100 | 1 | 2 vCPU / 8 GB | 1 GB | Single-node Compose is legitimate here |
| ≤ 1,000 | 2 | 4 vCPU / 16 GB | 4 GB | Add a read replica for dashboard queries |
| ≤ 10,000 | 3–4 | 8 vCPU / 32 GB, read replica | 8 GB HA | PgBouncer in transaction mode |

Storage: raw metrics are ~2.6 GB/day at 10,000 devices before compression, with
90-day retention. Provision ≥ 300 GB and monitor partition growth.

---

## 3. Development

```bash
make dev        # Postgres 16, Redis 7, Azurite, MailHog
make migrate
make bootstrap  # first organization + admin
make server
make dashboard
```

`deployments/docker/docker-compose.dev.yml` runs Azurite as the Blob emulator, so
the object-storage path is exercised locally rather than stubbed. The filesystem
`ObjectStore` driver exists for tests that should not need a container at all.

---

## 4. Single-node production

`deployments/docker/docker-compose.yml` brings up the server, dashboard, Postgres,
Redis, and Caddy for TLS. Suitable up to a few hundred devices.

```bash
cp deployments/docker/.env.example deployments/docker/.env
# fill in every value — the server refuses to start on a missing or invalid one
docker compose -f deployments/docker/docker-compose.yml up -d
docker compose exec server epctl migrate up
docker compose exec server epctl ca init
docker compose exec server epctl bootstrap --org "Visionify" --email ops@visionify.ai
```

Backups are a real step, not an afterthought — see §7.

### The device-plane certificate

`:9443` presents a certificate that agents verify against the CA bundle they
received at enrollment — not against the system trust store. A certificate from a
public CA does not work there, and this one does not work for the dashboard on
:443. They are two different trust decisions.

```bash
epctl ca server-cert -dir ./secrets/ca \
  -hosts edgepilot.example.com,203.0.113.10
```

It must be signed by the **root**, not the intermediate. The intermediate is
restricted to client authentication on purpose — that is what stops a stolen
intermediate key being used to impersonate the server to the fleet — so it cannot
issue a server certificate at all. Bring `root.key` back from storage, run this,
and put it away again; it is needed about once a year.

Every name and address an agent will dial must be in `-hosts`. A certificate for
the wrong name fails the handshake, which looks from the dashboard exactly like
the fleet going offline.

### Migrations under concurrency

`epctl migrate up` is safe to run from every replica at once. One takes a
PostgreSQL advisory lock and migrates; the rest wait on it and then find nothing
to do.

The wait is bounded at five minutes (`migrate.DefaultLockTimeout`). Past that,
the command exits non-zero with **"the migration lock is held by another
process"** — nothing was applied, the schema is untouched, and retrying once the
other migration finishes is safe. A deploy that hits this on a long migration
should retry rather than roll back.

The same five minutes also bounds ordinary lock waits *within* a migration, so
an `ALTER TABLE` stuck behind a long-running transaction fails the deploy instead
of hanging while every query queues behind it. If a migration legitimately needs
longer, raise the timeout for that run rather than removing it.

---

## 5. Configuration

Environment only. Parsed once into a typed struct and **validated at startup**:
the process refuses to boot on a missing or malformed required value rather than
failing at first use, so a misconfiguration is a failed deploy instead of a 3 a.m.
page.

Loading either yields a fully valid configuration or an error listing **every**
problem found, not just the first. Fixing a misconfigured deployment should be
one pass, not one redeploy per mistake.

Key material is parsed here too, not merely copied: a malformed Ed25519 key
fails at boot with a clear message rather than at the first login attempt.

`apps/server/internal/config` is the source of truth for this table, and its
tests assert the defaults below.

### General

| Variable | Required | Default | Notes |
|---|---|---|---|
| `EP_ENV` | | `development` | `development` \| `staging` \| `production` |
| `EP_PUBLIC_HOST` | in prod | `localhost` | Hostname agents and operators reach. Rejected as `localhost` in production |
| `EP_REPLICA_ID` | | hostname | Must be unique per replica: it keys the presence map that routes commands |

### Listeners

| Variable | Default | Notes |
|---|---|---|
| `EP_HTTP_ADDR` | `:8080` | REST API and dashboard WebSocket |
| `EP_GRPC_ADDR` | `:9443` | Device plane, mTLS |
| `EP_SSH_ADDR` | `:2222` | Bastion |
| `EP_ADMIN_ADDR` | `:9090` | Health and metrics — **never expose publicly** |
| `EP_HTTP_READ_TIMEOUT` | `15s` | |
| `EP_HTTP_WRITE_TIMEOUT` | `30s` | |
| `EP_HTTP_IDLE_TIMEOUT` | `120s` | |
| `EP_HTTP_SHUTDOWN_TIMEOUT` | `30s` | Drain window on SIGTERM |
| `EP_HTTP_ALLOWED_ORIGINS` | same-origin | Comma-separated. `*` is rejected in production |
| `EP_HTTP_MAX_REQUEST_BYTES` | `1 MiB` | Artifacts bypass the API entirely, so this stays small |
| `EP_GRPC_KEEPALIVE_INTERVAL` | `30s` | Detects a Mac that vanished without closing its socket |
| `EP_GRPC_KEEPALIVE_TIMEOUT` | `10s` | |
| `EP_GRPC_MAX_RECV_BYTES` | `16 MiB` | |
| `EP_GRPC_CERT_FILE` / `EP_GRPC_KEY_FILE` | — | Required in production |
| `EP_SSH_HOST_KEY_FILE` | — | Required in production. Its fingerprint is given to operators for pinning |
| `EP_SSH_PUBLIC_HOST` | `EP_PUBLIC_HOST` | What appears in the generated `ssh -J` command |
| `EP_SSH_PUBLIC_PORT` | `2222` | |
| `EP_SSH_IDLE_TIMEOUT` | `30m` | |
| `EP_SSH_MAX_SESSIONS` | `200` | |

All four listener addresses must differ; a collision is rejected at startup
rather than surfacing as `address already in use`.

### Data stores

> **The login role in `EP_DATABASE_URL` must be a member of both
> `edgepilot_app` and `edgepilot_admin`.** The server assumes those roles per
> transaction — `edgepilot_app` for tenant-scoped work, `edgepilot_admin` for
> the few operations that legitimately cross organizations — and refuses to
> start if it cannot.
>
> This is not ceremony. PostgreSQL exempts superusers and `BYPASSRLS` roles from
> every row-level security policy, so a pool connecting as a privileged role
> sees every tenant's data with all the policies in place and apparently
> working. Assuming the restricted role inside the transaction makes isolation a
> property of the code rather than of the connection string.
>
> ```sql
> CREATE ROLE edgepilot_login LOGIN PASSWORD '…';
> GRANT edgepilot_app, edgepilot_admin TO edgepilot_login;
> ```
>
> Do **not** point `EP_DATABASE_URL` at a superuser in production.

| Variable | Required | Default | Notes |
|---|---|---|---|
| `EP_DATABASE_URL` | yes | — | Must set `sslmode=verify-full` in production. See the role requirement above |
| `EP_DATABASE_MAX_CONNS` | | `25` | Per replica |
| `EP_DATABASE_MAX_IDLE_CONNS` | | `5` | May not exceed `EP_DATABASE_MAX_CONNS` |
| `EP_DATABASE_CONN_MAX_LIFETIME` | | `30m` | |
| `EP_DATABASE_CONN_MAX_IDLE_TIME` | | `5m` | |
| `EP_DATABASE_STATEMENT_TIMEOUT` | | `30s` | Bounds any single query, so one bad plan cannot drain the pool |
| `EP_REDIS_URL` | yes | — | Must use `rediss://` in production |
| `EP_REDIS_POOL_SIZE` | | `20` | |
| `EP_REDIS_DIAL_TIMEOUT` | | `5s` | |
| `EP_REDIS_READ_TIMEOUT` | | `3s` | |
| `EP_REDIS_WRITE_TIMEOUT` | | `3s` | |

### Authentication

| Variable | Required | Default | Notes |
|---|---|---|---|
| `EP_JWT_PRIVATE_KEY` | yes | — | Ed25519 PKCS#8 PEM. Accepts raw PEM, base64-wrapped, or escaped newlines |
| `EP_DATA_KEY` | yes | — | 32 bytes, hex or base64. Seals TOTP secrets at rest |
| `EP_ACCESS_TOKEN_TTL` | | `15m` | Must be shorter than the refresh TTL |
| `EP_REFRESH_TOKEN_TTL` | | `720h` | |
| `EP_ARGON_MEMORY_KIB` | | `65536` | RFC 9106 second recommendation |
| `EP_ARGON_ITERATIONS` | | `3` | |
| `EP_ARGON_PARALLELISM` | | `4` | |
| `EP_MAX_FAILED_LOGINS` | | `5` | |
| `EP_LOCKOUT_DURATION` | | `15m` | |

### Certificates and signing

| Variable | Required | Default | Notes |
|---|---|---|---|
| `EP_CA_CERT` / `EP_CA_KEY` | in prod | — | Device CA. Key Vault reference in production |
| `EP_DEVICE_CERT_TTL` | | `2160h` (90d) | |
| `EP_DEVICE_CERT_RENEW_BEFORE` | | `720h` (30d) | Must be less than the certificate TTL |
| `EP_SIGNING_PUBLIC_KEYS` | in prod | — | Comma-separated Ed25519 public keys agents pin. A list, because rotation is additive |
| `EP_SIGNING_PRIVATE_KEY` | | absent | Normally **unset**: signing belongs to a separate HSM-backed service, so owning the API server is not sufficient to sign a payload |

### Object storage

| Variable | Required | Default | Notes |
|---|---|---|---|
| `EP_STORAGE_DRIVER` | | `fs` | `azblob` \| `fs` |
| `EP_AZURE_STORAGE_ACCOUNT` | if `azblob` | — | |
| `EP_AZURE_STORAGE_CONTAINER` | if `azblob` | — | |
| `EP_AZURE_STORAGE_KEY` | | — | Omit when using workload identity, which is preferred |
| `EP_STORAGE_FS_ROOT` | if `fs` | `/var/lib/edgepilot/objects` | |
| `EP_STORAGE_SIGNED_URL_TTL` | | `15m` | |
| `EP_ALLOW_FILESYSTEM_STORAGE_IN_PRODUCTION` | | `false` | `fs` in production pins the server to one host, so it must be an explicit choice |

### Agent policy

Pushed to agents in `ConfigUpdate`.

| Variable | Default | Notes |
|---|---|---|
| `EP_AGENT_HEARTBEAT_INTERVAL` | `15s` | |
| `EP_AGENT_METRICS_INTERVAL` | `30s` | |
| `EP_AGENT_INVENTORY_INTERVAL` | `6h` | Unchanged sections are skipped by checksum |
| `EP_AGENT_OFFLINE_AFTER` | `90s` | Must exceed twice the heartbeat interval, so one dropped packet does not page anyone |
| `EP_AGENT_MAX_CONCURRENT_COMMANDS` | `4` | |
| `EP_AGENT_SPOOL_MAX_BYTES` | `64 MiB` | Bounded on-disk spool for durable messages while disconnected |
| `EP_DEVICE_PLANE_ENDPOINT` | derived | Defaults to `EP_PUBLIC_HOST` plus the gRPC port |

### Observability

| Variable | Default | Notes |
|---|---|---|
| `EP_LOG_LEVEL` | `info` (`debug` in dev) | `debug` \| `info` \| `warn` \| `error` |
| `EP_LOG_FORMAT` | `json` (`text` in dev) | `json` \| `text` |
| `EP_OTLP_ENDPOINT` | — | Enables tracing when set |
| `EP_SERVICE_NAME` | `edgepilot-server` | |
| `EP_TRACE_SAMPLE_PERCENT` | `10` | 0–100 |
| `EP_METRICS_ENABLED` | `true` | |

Secrets come from Key Vault via workload identity in Azure. No secret has a
default, and none is ever logged — the `slog` redactor drops them by field name
(see [Security.md](Security.md) §7).

---

## 6. Azure

1. **Resource group** with a VNet, one subnet for the app and one delegated to
   Postgres.
2. **Azure Database for PostgreSQL Flexible Server 16**, private access only,
   zone-redundant HA, PITR enabled. Run `epctl migrate up` from a jump host or an
   init container.
3. **Azure Cache for Redis**, Standard or better (HA), private endpoint, TLS.
4. **Storage account**, private endpoint, versioning + soft delete on the
   container. Lifecycle rules matching the retention table in
   [Database.md](Database.md) §5.
5. **Key Vault** holding `EP_JWT_PRIVATE_KEY`, `EP_DATA_KEY`, `EP_CA_KEY`, and the
   release signing key. HSM-backed for the signing key.
6. **Container Apps** or **AKS** running `edgepilot-server`, with a workload
   identity granting Key Vault and Storage access. No connection strings in
   environment variables.
7. **Application Gateway**: HTTP listener on 443 for REST and the dashboard, and
   **L4 listeners** for 9443 and 2222 per §1.
8. **DNS**: `edgepilot.<domain>` and `ep-bastion.<domain>`.
9. **Monitoring**: scrape `:9090/metrics`, ship logs to Log Analytics, alert on
   the EdgePilot-level rules in §8.

---

## 7. Backup and restore

| Asset | Method | Target |
|---|---|---|
| Postgres | Flexible Server PITR | RPO 5 min, RTO 1 h |
| Blob storage | Versioning + soft delete + GRS | RPO ~0 |
| Key Vault | Soft delete + purge protection | — |
| Redis | **Not backed up, by design** | Rebuilt from Postgres on start |

Redis holds presence, caches, rate-limit counters, and pub/sub. All of it is
derivable or expendable — losing Redis costs live dashboard updates until agents
re-heartbeat, and the command queue keeps working through Postgres reconciliation.

**A backup that has never been restored is a hypothesis.** The restore drill is a
quarterly runbook item: restore to a scratch server, run `epctl migrate status`,
verify the audit hash chain over the restored window, and record the elapsed time
against the stated RTO.

---

## 8. Operating it

**Health**

- `/healthz` — liveness. If the process answers, it is alive.
- `/readyz` — readiness. Fails when Postgres or Redis is unreachable or
  migrations are behind, which pulls the replica from the load balancer without
  killing it.

**Metrics that matter**

| Metric | Watch for |
|---|---|
| `edgepilot_devices_connected` | A step change means a replica dropped its streams |
| `edgepilot_agent_stream_errors_total` | Rising = certificate, network, or protocol trouble |
| `edgepilot_command_queue_depth` | Sustained growth = dispatch is stuck |
| `edgepilot_metric_ingest_lag_seconds` | The leading indicator that Postgres is the bottleneck |
| `edgepilot_partition_headroom_days` | Alert below 30. A missing partition is a hard write failure |
| `edgepilot_cert_expiring_devices` | Renewal is automatic; a non-zero value means it is not working |

**Rolling upgrades**

Replicas drain gracefully: stop accepting new streams, send
`Disconnect{REASON_SHUTTING_DOWN}`, let agents reconnect elsewhere, then exit.
Agents reconnect within their backoff, so a rolling restart is not a fleet outage.

Migrations must be backward-compatible with the previous release for the duration
of the roll — expand/contract, never a destructive change in the same deploy as
the code that stops using the column.

**Scaling**

The server is stateless; add replicas. The two things that do not scale by adding
replicas are singleton workers (leader-elected, so only one runs regardless) and
Postgres write throughput on metric ingest (batched `COPY`; if it saturates, the
escalation is TimescaleDB or a dedicated ingest path).

---

## 9. Firewall requirements

**Customer site — egress only:**

| Destination | Port | Purpose |
|---|---|---|
| `edgepilot.<domain>` | 9443/tcp | Device plane |
| `edgepilot.<domain>` | 443/tcp | Enrollment |
| `<storage>.blob.core.windows.net` | 443/tcp | Package and log transfer |

**No inbound rule. No port forward. No VPN.** If a customer's egress policy
requires a single port, the device plane also serves on 443 with ALPN.

**Server side:** 443, 9443, and 2222 inbound from the internet; 9090 and 7443
inside the VNet only.
