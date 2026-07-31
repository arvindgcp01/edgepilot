# The East US deployment

A live single-node EdgePilot, provisioned 2026-07-31.

| | |
|---|---|
| Subscription | Microsoft Azure Sponsorship (`8914bde9-…`) |
| Resource group | `edgepilot-eastus` |
| Region | East US |
| VM | `edgepilot-vm`, Standard_B2s (2 vCPU, 4 GB), Ubuntu 24.04 |
| Public IP | `20.106.158.37` (Standard SKU, static) |
| Private IP | `10.0.0.4` |
| Disk | 64 GB StandardSSD |
| Deployment | Docker Compose at `/opt/edgepilot/deployments/docker` |
| `EP_ENV` | **staging** — see below |

Cost is roughly **$35–40/month**: the B2s is about $30, the disk about $5, and the
static IP about $3.

## Why this says staging and not production

The server refuses to start with `EP_ENV=production` here, and it is right to.
Production requires `sslmode=verify-full` to PostgreSQL, `rediss://` to Redis,
object storage rather than the filesystem, an SSH host key, and pinned signing
keys. This stack has PostgreSQL and Redis on a private Docker network with no
TLS between them, and no object storage at all.

That refusal is the configuration validator doing its job. The honest response
was to label the deployment for what it is rather than to weaken the check.

**What it would take to call this production**, in the order that matters:

1. Azure Database for PostgreSQL Flexible Server, private endpoint,
   `sslmode=verify-full`. This is also what gives the deployment backups —
   today a lost VM is a lost fleet.
2. Azure Cache for Redis with TLS, `rediss://`.
3. A storage account for `EP_STORAGE_DRIVER=azblob`.
4. An SSH host key for the bastion, and Ed25519 signing keys for agent releases.
5. TLS in front of the API. It is plain HTTP on `:8080` today.

## Access

The network security group allows **only one source address** — the operator's,
`124.123.34.96/32` — on ports 22, 8080/443, and 9443.

Real Macs at customer sites cannot reach the device plane until that is widened:

```bash
az network nsg rule update -g edgepilot-eastus --nsg-name edgepilot-vmNSG \
  -n allow-device-plane --source-address-prefixes Internet
```

That is defensible when you do it — `:9443` is mutual TLS and admits only
certificates issued by this deployment's own CA — but it is a public surface and
should be a deliberate act, not a default.

```bash
ssh azureuser@20.106.158.37
cd /opt/edgepilot/deployments/docker
docker compose ps
docker compose logs server -f
```

## The certificate authority

Generated on the VM with the same binary that serves, so there is no separately
installed toolchain to keep in step.

```
/opt/edgepilot/deployments/docker/secrets/ca/
  chain.crt          intermediate + root — EP_CA_CERT
  intermediate.key   signs device certificates — EP_CA_KEY
  server.crt/.key    the device plane's TLS identity, for 20.106.158.37
  root.crt           public
```

**`root.key` is not in that directory.** It was moved to
`/opt/edgepilot/root.key.OFFLINE` immediately after the device-plane certificate
was issued.

> That file is still on the VM, which is not "offline" in any meaningful sense.
> Move it to storage that is not attached to this machine. If the intermediate is
> stolen you reissue it and devices re-enrol; if the root is stolen there is
> nothing to do but rebuild the fleet.

The device-plane certificate names `20.106.158.37`, `edgepilot-vm`, and
`10.0.0.4`. Agents verify those names, so **putting a DNS name in front of this
deployment requires reissuing the certificate**:

```bash
docker run --rm --entrypoint /usr/local/bin/epctl \
  -v $PWD/secrets:/secrets edgepilot-server:dev \
  ca server-cert -dir /secrets/ca -force \
  -hosts edgepilot.visionify.ai,20.106.158.37
docker compose restart server
```

That needs `root.key` back temporarily: the intermediate is restricted to client
authentication on purpose, so it cannot issue a server certificate. Signing this
one is a root operation, about once a year.

## Verified on this deployment

- The API answers `/healthz` and `/readyz` over the internet.
- An administrator signs in and receives a token pair; the refresh token is an
  `HttpOnly` cookie and is absent from the response body.
- An enrollment token is minted, shown once, and stored only as a SHA-256.
- **A real agent binary enrolled from outside Azure**, obtained a certificate,
  deleted its enrollment token, and opened an mTLS stream to `:9443`.
- The device appears in the fleet listing as `online`, and the fleet summary
  counts it.
- Restarting the agent reuses its stored identity rather than enrolling again.
- Stopping the agent moves it to `offline` in 106 seconds — a 90-second
  threshold plus one 30-second sweep.

## Teardown

```bash
az group delete -n edgepilot-eastus --yes --no-wait
```

Deletes the VM, the disk, the public IP, the network, and the NSG. Nothing in
this deployment is stored anywhere else, so this is complete — and irreversible.
