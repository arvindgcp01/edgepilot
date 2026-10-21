# EdgePilot

Self-hosted fleet management for Mac mini and Mac Studio devices deployed at
customer sites.

EdgePilot gives an operator one place to see every Mac, know its health, run a
vetted set of actions on it, move files to and from it, deploy software, and get
a real SSH session — **without any inbound network access to the customer's
network**.

> **Not an MDM.** No profiles, no APNs, no supervision.
> **Not a remote desktop.** GUI access is AnyDesk's job; EdgePilot never renders a screen.
> **Not a general remote-execution tool.** The command surface is an allowlist, and
> arbitrary shell requires a second approver, a signed payload, and a device that
> has not opted out.

---

## Status

**Design verified. Phase 0 foundations complete. Phase 1 next.**

| Artifact | State |
|---|---|
| [Architecture](docs/Architecture.md) | Complete — components, protocols, failure modes, decision log |
| [Database schema](docs/Database.md) · [migrations](apps/server/migrations) | **Verified** on PostgreSQL 16: 8 migrations apply, reverse, and re-apply clean; RLS, triggers, and constraints exercised |
| [Device protocol](packages/proto/edgepilot/v1/agent.proto) | **Verified** — compiles and lints clean under `buf` |
| [API specification](packages/shared/openapi/openapi.yaml) | **Verified** — 60 paths, 75 operations, 46 schemas; `redocly lint` and `bundle` clean |
| [Security design](docs/Security.md) | Complete, including stated limitations |
| [Implementation plan](docs/Roadmap.md) | Four phases with exit criteria and a risk register |
| Phase 0 foundations | **Complete** — Go workspace, config, logging, error taxonomy, `epctl migrate`, test harness, CI |

---

## How it works

```
Customer site (egress only)          Azure                        Operator
┌──────────────────────┐    ┌─────────────────────────┐    ┌──────────────┐
│ edgepilot-agent      │    │ REST API      :8080     │◄───│ Dashboard    │
│  LaunchDaemon, root  │───►│ Device plane  :9443     │    │ Next.js      │
│                      │mTLS│ SSH bastion   :2222     │◄───│ ssh -J       │
│ macOS sshd (loopback)│◄───│ Workers                 │    └──────────────┘
└──────────────────────┘    │ Postgres · Redis · Blob │
                            └─────────────────────────┘
```

Each agent holds **one** outbound mTLS gRPC stream. Heartbeats, telemetry,
inventory, commands, file transfers, log collection, and SSH tunnel bytes all
travel on it. The customer's firewall rule is a single egress destination — no
inbound port, no port forward, no VPN.

Four design choices carry most of the weight:

- **Device identity is a certificate, not a token.** Each Mac generates its own
  keypair, sends a CSR, and gets a 90-day mTLS client certificate that renews
  itself over the authenticated stream. The private key never leaves the device,
  and revocation takes effect within one heartbeat.

- **The server never sends an argv.** It dispatches a *slug* and validated
  parameters; the agent builds the command from a handler table it was compiled
  with. An attacker who owns the server can invoke only what the agent already
  implements.

- **`ssh -J` sessions are end-to-end encrypted.** The bastion relays an encrypted
  stream between the engineer's client and the Mac's `sshd`; metadata is fully
  audited, content is not readable by the control plane. The **browser terminal**
  deliberately trades that away — a browser cannot speak SSH, so the server
  terminates the session, and those sessions are recorded in exchange. Two paths,
  two trust models: see [Security](docs/Security.md) §5.1.

- **Multi-tenant with row-level security.** Tenancy is enforced by the repository
  layer *and* by PostgreSQL, deny-by-default. A forgotten `WHERE org_id = ?` is a
  query that returns nothing, not a data leak.

---

## Repository layout

```
apps/
  server/         Go — REST API, gRPC device plane, WebSocket hub, SSH bastion, workers
  dashboard/      Next.js + TypeScript + TailwindCSS + shadcn/ui
  agent/          Go — macOS LaunchDaemon
packages/
  proto/          Protobuf contract for the device plane (+ generated Go)
  shared/         OpenAPI spec and the generated TypeScript client
  ui/             shadcn-based component library shared by the dashboard
deployments/
  docker/         Compose stacks for development and single-node production
  k8s/            Manifests and Helm chart (Phase 4)
docs/             Architecture, Database, API, Deployment, Security, Roadmap, Contributing
```

---

## Getting started

**Prerequisites:** Go 1.25+, Node 20+, Docker with Compose v2+.

```bash
make dev        # Postgres, Redis, Azurite, MailHog
make migrate    # apply schema
make bootstrap  # create the first organization and admin user
make server     # :8080 REST, :9443 device plane, :2222 bastion
make dashboard  # :3000
```

Enroll a Mac:

```bash
# On the server — create a scoped, expiring enrollment token
epctl enrollment-token create --org acme --ttl 24h --max-uses 1

# On the Mac
sudo installer -pkg EdgePilotAgent.pkg -target /
sudo edgepilot-agent enroll --server edgepilot.example.com --token ep_enroll_...
```

The device appears in the dashboard within seconds, in `pending` until an admin
approves it.

See [Deployment.md](docs/Deployment.md) for production.

---

## Verifying the design artifacts

The design is checked by executing it, not by reading it:

```bash
make verify-schema   # apply, reverse, re-apply migrations on a throwaway Postgres 16
                     # and exercise RLS, triggers, and constraints
make verify-proto    # buf lint + buf build
make verify-api      # redocly lint + bundle
```

---

## Documentation

| Document | Contents |
|---|---|
| [Architecture.md](docs/Architecture.md) | Components, device plane, enrollment, SSH, commands, deployment, internals, failure modes, decision log |
| [Database.md](docs/Database.md) | Entity map, conventions, partitioning, RLS, indexes, retention, verification |
| [API.md](docs/API.md) | REST conventions, auth flows, realtime events, worked examples |
| [Security.md](docs/Security.md) | Threat model, identity, authorization, supply chain, incident response, limitations |
| [Deployment.md](docs/Deployment.md) | Compose and Azure deployment, configuration, backup, scaling |
| [Roadmap.md](docs/Roadmap.md) | Phased plan with exit criteria and risk register |
| [Contributing.md](docs/Contributing.md) | Code standards, testing, review, release |

---

© Visionify, Inc. Proprietary.
