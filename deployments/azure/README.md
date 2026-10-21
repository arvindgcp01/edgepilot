# The East US deployment

A live single-node EdgePilot. The original deployment of 2026-07-31 was torn
down and rebuilt the same day; everything below describes the rebuild.

| | |
|---|---|
| Subscription | Microsoft Azure Sponsorship (`8914bde9-…`) |
| Resource group | `edgepilot-eastus` |
| Region | East US |
| VM | `edgepilot-vm`, Standard_B2s (2 vCPU, 4 GB), Ubuntu 24.04 |
| Hostname | `edgepilot-visionify.eastus.cloudapp.azure.com` |
| Public IP | `20.85.220.6` (Standard SKU, static) |
| Private IP | `10.0.0.4` |
| Disk | 64 GB StandardSSD, plus a 4 GB swapfile |
| Deployment | Docker Compose at `/opt/edgepilot/deployments/docker` |
| `EP_ENV` | **staging** — see below |

Cost is roughly **$35–40/month**: the B2s is about $30, the disk about $5, and the
static IP about $3.

**Address the deployment by its hostname, not its address.** The rebuild changed
the public IP from `20.106.158.37` to `20.85.220.6`; the Azure DNS label survived
it because it was reclaimed after the delete. Anything pinned to the address —
an agent's endpoint, a firewall rule at a customer site — breaks on a rebuild.
The hostname does not.

The 4 GB swapfile is not optional on a B2s. The dashboard image is a Next.js
production build, and 4 GB of RAM alone gets the compiler OOM-killed part way
through `pnpm build`.

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
5. Ed25519 signing keys pinned in `EP_SIGNING_PUBLIC_KEYS`, which is empty here.

TLS in front of the API is no longer on that list — Caddy terminates it, see
below.

## Access

| Port | Source | What |
|---|---|---|
| 22 | `122.169.131.207/32` | SSH |
| 80, 443 | Internet | Caddy: dashboard and API |
| 8080 | `122.169.131.207/32` | the API directly, for debugging |
| 9443 | Internet | the device plane |

`:9443` is open to the internet deliberately — it is mutual TLS and admits only
certificates issued by this deployment's own CA, and a Mac at a customer site
cannot reach it otherwise. `:22` and `:8080` are pinned to one operator address.

That address changes. It was `124.123.34.96/32` at first provisioning and
`110.226.146.158/32` by the time of the rebuild; both were stale. When SSH times
out, this is why:

```bash
az network nsg rule update -g edgepilot-eastus --nsg-name edgepilot-vmNSG \
  -n allow-ssh --source-address-prefixes "$(curl -s -4 ifconfig.me)/32"
```

```bash
ssh azureuser@edgepilot-visionify.eastus.cloudapp.azure.com
cd /opt/edgepilot/deployments/docker
docker compose ps
docker compose logs server -f
```

## The two certificates

They are different trust decisions and neither substitutes for the other.

**The dashboard and API, on `:443`** — a Let's Encrypt certificate that Caddy
obtains and renews on its own, because `EP_SITE_ADDRESS` is a real hostname and
`EP_TLS_DIRECTIVE` is empty. This is what makes the `Secure` refresh cookie
usable: over plain HTTP a browser discards it and sessions do not survive a
reload.

**The device plane, on `:9443`** — signed by this deployment's own root, and
verified by agents against the CA bundle they received at enrollment rather than
against the system trust store. A public certificate does not work here.

```
/opt/edgepilot/deployments/docker/secrets/ca/
  chain.crt          intermediate + root — EP_CA_CERT
  intermediate.key   signs device certificates — EP_CA_KEY
  server.crt/.key    the device plane's TLS identity
  root.crt           public
```

Owned by UID 65532 and mounted read-only: the server image runs as that user and
cannot read root-owned `0600` keys. `epctl` runs as the same user, so the
directory has to be chowned to it *before* `ca init`, not after.

**`root.key` is not in that directory.** It was moved to
`/opt/edgepilot/root.key.OFFLINE` immediately after the device-plane certificate
was issued.

> That file is still on the VM, which is not "offline" in any meaningful sense.
> Move it to storage that is not attached to this machine. If the intermediate is
> stolen you reissue it and devices re-enrol; if the root is stolen there is
> nothing to do but rebuild the fleet.

The device-plane certificate names
`edgepilot-visionify.eastus.cloudapp.azure.com`, `edgepilot-vm`, `20.85.220.6`,
and `10.0.0.4`. Agents verify those names, so a new name in front of this
deployment means reissuing it:

```bash
docker run --rm --entrypoint /usr/local/bin/epctl \
  -v $PWD/secrets:/secrets edgepilot-server:dev \
  ca server-cert -dir /secrets/ca -force \
  -hosts edgepilot.visionify.ai,edgepilot-visionify.eastus.cloudapp.azure.com
docker compose restart server
```

That needs `root.key` back temporarily: the intermediate is restricted to client
authentication on purpose, so it cannot issue a server certificate. Signing this
one is a root operation, about once a year.

## Verified on the rebuild

- `/healthz` returns 200 and `/readyz` returns `{"status":"ready"}` over the
  internet.
- The `:443` certificate is issued by Let's Encrypt for the Azure hostname and
  is valid to 2026-10-29.
- The dashboard serves at `https://edgepilot-visionify.eastus.cloudapp.azure.com/`.
- An administrator signs in over HTTPS and receives an access token; the refresh
  token arrives as an `HttpOnly; Secure; SameSite=Strict` cookie scoped to
  `/api/v1/auth` and is absent from the response body.
- That access token authenticates `GET /api/v1/devices/summary`.
- The schema is at version 9 and all five containers report healthy.
- `:9443` accepts TCP connections from outside Azure.

**Not yet re-verified on this deployment:** a real agent enrolling, obtaining a
certificate, and holding an mTLS stream. That was verified against the previous
VM and the code has not changed, but no Mac has enrolled against this one — the
fleet is empty.

## Teardown

```bash
az group delete -n edgepilot-eastus --yes
```

Deletes the VM, the disk, the public IP, the network, and the NSG. Nothing in
this deployment is stored anywhere else, so this is complete — and irreversible.
It also releases the `edgepilot-visionify` DNS label; reclaim it on the next
`az vm create` with `--public-ip-address-dns-name edgepilot-visionify`, which
only works because the label was freed. Drop `--no-wait` if you intend to
recreate immediately, or the label is still held when you try to take it.
