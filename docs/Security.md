# EdgePilot — Security Design

EdgePilot holds root on every Mac in the fleet. That is the honest framing, and
every decision below follows from it: the platform must be safe not only against
outside attackers, but against a compromise of EdgePilot itself.

The recurring question in this document is **"what does an attacker get if they
own the server?"** A design where that answer is "the entire fleet" is not
acceptable, and several controls here exist for no other reason.

---

## 1. Threat model

### In scope

| # | Threat | Primary control |
|---|---|---|
| T1 | Internet attacker reaches the API | TLS 1.3, no unauthenticated mutating endpoints, rate limits, no inbound path to devices at all |
| T2 | Stolen user credentials | Argon2id, MFA, lockout, short access tokens, refresh reuse detection, RBAC, full audit |
| T3 | Stolen device credentials | Per-device certificates, 90-day lifetime, instant revocation, private key never leaves the Mac |
| T4 | **Compromised EdgePilot server** | Ed25519 signing key held offline; agent pins release keys; agent builds its own argv; per-device kill switch; `ssh -J` content is end-to-end encrypted. **The browser terminal is the exception** — it holds a fleet-wide shell credential and reads sessions in clear. See §5.1 |
| T5 | Malicious or careless insider | Two-person rule for custom scripts, permission separation, append-only hash-chained audit log |
| T6 | Cross-tenant data access | RLS enforced by PostgreSQL, deny-by-default, indistinguishable 404s |
| T7 | Supply chain — backdoored agent or package | Signature verified against a compiled-in public key before anything executes |
| T8 | Tampering with evidence | Audit log immutable at the database level and hash-chained |
| T9 | Lateral movement from one Mac to another | Tunnels restricted to loopback sshd; no device-to-device path exists |

### Out of scope

- Physical access to a Mac. Someone at the keyboard with the firmware password
  wins; FileVault and a boot policy are the mitigations, and they are outside
  EdgePilot.
- macOS kernel vulnerabilities.
- The customer's own network security.
- GUI session security — AnyDesk owns that.

---

## 2. Identity

### 2.1 Users

| Control | Choice | Reason |
|---|---|---|
| Password hashing | Argon2id, 64 MiB / t=3 / p=4 | Memory-hard; bcrypt's 72-byte truncation and low memory cost make GPU cracking cheap |
| Minimum length | 12 characters, checked against a breach corpus | Length beats composition rules, which mostly produce `Password1!` |
| MFA | TOTP, secret sealed with the application data key, recovery codes stored as SHA-256 | A database dump must not yield working second factors |
| Lockout | Exponential, capped, per account **and** per source IP | Per-account alone lets an attacker lock out every user as a denial of service |
| Login response | Constant-time, identical for unknown email / wrong password / disabled account | Otherwise the endpoint is a user enumeration oracle |

### 2.2 Tokens

**Access token**: JWT, EdDSA (Ed25519), 15-minute lifetime. Claims: `sub`, `org`,
`roles`, `perms`, `sid`, `jti`, `exp`, `iat`. Verified on every request; `sid`
checked against a Redis revocation set so a logout takes effect immediately
rather than at expiry.

**Refresh token**: opaque 256-bit random value, **not** a JWT. Only its SHA-256 is
stored. Single-use with rotation:

```
login → token A (family F)
refresh(A) → token B, A revoked
refresh(A) again → A was already rotated → the token leaked
                 → revoke the ENTIRE family F
```

Reuse detection is why refresh tokens are stateful. A stolen refresh token gets
the attacker at most one rotation before the legitimate client's next refresh
trips the detector and ends every session from that login.

**Storage**: access token in memory only; refresh token in an `HttpOnly; Secure;
SameSite=Strict` cookie scoped to `/api/v1/auth`. Neither goes in `localStorage`
— that turns any XSS into permanent account takeover, and no CSP is airtight
enough to bet the fleet on.

### 2.3 Devices

Devices authenticate with **mTLS client certificates** from EdgePilot's private
CA. The certificate SAN is `spiffe://edgepilot/<org-id>/<device-id>`, so device
identity is established at the transport layer and there is no application-level
device token to leak, log, or replay.

```
first boot → generate P-256 key in the Secure Enclave-backed keychain
             (file-backed, 0600 root:wheel, where unavailable)
          → build CSR, CN = device UUID
          → POST /agent/enroll with a one-time enrollment token
          → receive a 90-day certificate
renewal   → at 60 days, over the ALREADY AUTHENTICATED stream
```

- The private key never leaves the Mac. The server only ever sees a CSR.
- Renewal needs no enrollment token and no human, so certificates can be short
  without creating operational burden.
- Revocation is a database state consulted by the mTLS verify callback against a
  Redis-cached set. A revoked device's stream dies within one heartbeat.
- Enrollment tokens are stored hashed, scoped to one org, TTL-bounded, and
  use-counted. `auto_approve = false` is the production default: the device
  connects and reports inventory but receives **no commands** until an admin
  approves it.

---

## 3. Authorization

28 permissions, three built-in roles. Permissions are checked at the **service**
layer, not in handlers, so every caller of an operation is covered rather than
every HTTP route.

The separations that matter:

| Withheld from Operator | Why |
|---|---|
| `command:custom_script` | Requesting arbitrary code must be a distinct grant from routine operations |
| `command:approve` | If the same role can request and approve, the two-person rule is theatre |
| `ssh:manage_keys` | Otherwise an operator grants themselves access to any device |
| `device:approve` | Enrolling a rogue device should not be self-service |
| `audit:read` | Deliberate: reading the audit log is an administrative function |

`agent:release` is held only by the internal Visionify operations org — customer
admins cannot publish agent binaries to their own fleet.

Tenancy is enforced twice: the repository layer filters by org, and PostgreSQL
RLS denies by default when the scope GUC is unset (see `Database.md` §3.4).
Cross-tenant reads return **404, not 403** — a 403 confirms the resource exists.

---

## 4. Command execution — the core of the design

### 4.1 The server never sends an argv

There is no "run this string" endpoint. The server dispatches a **slug** and
**validated parameters**; the agent looks the slug up in a handler table it was
compiled with and builds the argv itself.

```
server: CommandRequest{slug: "restart_service", params: {"label": "com.example.svc"}}
agent:  → handler table lookup
        → re-validate params against ITS OWN copy of the schema
        → exec launchctl kickstart -k system/com.example.svc
```

An attacker who fully owns the API server can invoke only the operations the
agent already implements, with parameters that pass the agent's own schema check.
They cannot turn the fleet into a botnet by sending `/bin/sh -c`.

Both sides validate. Neither trusts the other's validation.

### 4.2 Custom scripts: the deliberate exception, made expensive

Real operations occasionally need something not in the catalogue. Five independent
controls stand in the way:

1. **Permission** — `command:custom_script`, not granted to Operator.
2. **Two-person rule** — a *different* user must approve. Enforced by a database
   trigger, not just the service layer.
3. **Signature** — on approval, the server signs `(device_id, command_id,
   script_sha256, expires_at)` with the Ed25519 release key.
4. **Pinned verification** — the agent verifies against a **compiled-in** public
   key, checks the SHA-256, checks the expiry, and checks that `device_id` is its
   own. A signature for device A is useless against device B.
5. **Per-device kill switch** — `allow_custom_scripts = false` makes the agent
   refuse regardless of any valid signature. This is the default.

**What T4 gets:** a compromised server cannot run a script. It lacks the signing
key. A compromised server *plus* the signing key still cannot run one on a device
with the kill switch set, and still leaves an audit trail it cannot alter.

The signing key lives in Azure Key Vault (HSM-backed) and is used by a signing
service, not by the API process. Ideally it is offline entirely and signing is a
release-time operation.

### 4.3 Key rotation

`ConfigUpdate.signing_public_keys` is a **list**. Rotation is additive: distribute
the new key, wait for fleet convergence, start signing with it, then withdraw the
old one. The compiled-in key is the root of trust for the initial bootstrap and
changes only with an agent release.

---

## 5. SSH

### 5.1 Two SSH paths, two trust models

EdgePilot offers two ways to reach a Mac, and they make **opposite** trades. The
difference is not cosmetic and neither is a strict improvement on the other.

| | `ssh -J` (bastion) | Browser terminal |
|---|---|---|
| Who terminates SSH | the engineer's own client | **the server** |
| Can EdgePilot read the session | **no** — it relays ciphertext | **yes** |
| Credential the server holds | none | a key that opens a shell fleet-wide |
| Session content recorded | no | **yes**, asciicast |
| What a server compromise yields | tunnels, not sessions | live shells and transcripts |

**The bastion path.** The bastion relays an *encrypted* SSH stream between the
engineer's client and the Mac's `sshd`. The inner handshake is end-to-end; the
bastion sees ciphertext and byte counts. Metadata — who, which device, which key,
from where, how long, how many bytes — is captured in `ssh_sessions` and
`audit_logs`. Nothing else is available to capture, by construction.

**The browser terminal.** A browser cannot speak SSH, so a terminal rendered in a
page requires the server to be the SSH client: it authenticates to the Mac with
its own key, decrypts the session, and streams the terminal to the page. That
gives up the property above completely. Every keystroke and every credential
typed during such a session passes through the server in clear text, and
`EP_SSH_CLIENT_KEY_FILE` is a credential that opens a shell on every Mac that has
installed its public half.

This is the trade Teleport, AWS Session Manager and Azure Arc all make, and it is
defensible — but it must be a decision, not a default that arrives with a
convenient feature.

**Because confidentiality is gone, accountability replaces it.** Browser-terminal
sessions are recorded to `EP_SESSION_RECORDING_DIR` in asciicast v2 format.
*Output only* — keystrokes are not captured, because a password prompt does not
echo and recording input would turn every recording into a file of the customer's
passwords. Recording is not optional in any deployment that enables the terminal;
if the server can read a session, there should be an account of what was done in
it.

**How to keep the stronger property.** Leave `EP_SSH_CLIENT_KEY_FILE` unset. The
browser terminal is then disabled, the endpoint answers `terminal_disabled`, and
`ssh -J` is unaffected. Revoking the terminal key's `ssh_keys` row has the same
effect fleet-wide, on the next agent reconnect.

Where full session capture is a hard compliance requirement and the control plane
must *not* be able to read sessions, the answer is still recording on the device
(`sudo` session logging, shipped in the log bundle) rather than either path here.

### 5.2 Private keys

EdgePilot stores **public keys only**. The bastion authenticates the outer hop by
verifying a signature. There is no private key on the server to steal.

### 5.3 Device hardening

The agent maintains `/etc/ssh/sshd_config.d/edgepilot.conf`:

```
PasswordAuthentication no
KbdInteractiveAuthentication no
ChallengeResponseAuthentication no
PermitRootLogin no
ListenAddress 127.0.0.1
AllowTcpForwarding no
X11Forwarding no
AuthorizedKeysFile /var/db/edgepilot/authorized_keys.%u
```

`ListenAddress 127.0.0.1` means the Mac is unreachable over SSH even if the
customer's firewall is misconfigured. The only path is the tunnel.

`AllowTcpForwarding no` on the device prevents a session from being pivoted into
the customer's network.

### 5.4 Authorized-key reconciliation

The agent owns `authorized_keys` and rewrites it in full on every config push, so
revoking a key in the dashboard removes it fleet-wide. Each line carries
`expiry-time=` and restriction options, so **`sshd` enforces expiry independently**
— a key expires correctly even on a device that has been offline for a month and
never received the revocation.

### 5.5 Tunnels cannot be repurposed

`TunnelOpen` carries a target host and port, but the agent enforces an allowlist
(loopback `sshd` only). Without that check the tunnel is a general-purpose SOCKS
proxy into the customer's network, granted by whoever controls the server.

---

## 6. Software supply chain

### 6.1 Agent updates

```
release → build reproducibly in CI
        → SHA-256
        → Ed25519 signature by the release key (Key Vault, HSM)
        → agent_versions row, rollout_percent = 0
        → staged: 1% → 10% → 50% → 100%
```

The running agent verifies the signature against a **compiled-in** public key
before replacing its binary. A compromised object store, a compromised CDN, or a
compromised API server is not sufficient to push a backdoored agent.

Staged rollout uses `hash(device_uuid) % 100 < rollout_percent`, so the 10% cohort
is stable across polls rather than re-rolled each time. `is_yanked` halts further
upgrades immediately.

The updater writes to a staging path, verifies, swaps atomically, and restarts via
launchd. If the new agent fails to check in within a watchdog window, the previous
binary is restored — an agent that cannot phone home cannot be fixed remotely, so
rollback must be autonomous.

### 6.2 Deployed packages

Identical chain: SHA-256 + Ed25519 verified before `installer -pkg` or extraction
runs. `install_spec` hooks reference **allowlisted command slugs**, never shell
strings, so a package cannot smuggle arbitrary execution through its install
recipe.

### 6.3 Build integrity

- Pinned dependencies (`go.sum`, lockfiles), `go mod verify` in CI.
- `govulncheck`, `gosec`, `trivy` on images, `npm audit` — all failing the build.
- SBOM (CycloneDX) generated and attached per release.
- Container images built from `scratch`/`distroless`, non-root, read-only root
  filesystem.

---

## 7. Data protection

| Data | At rest | In transit |
|---|---|---|
| Database | Azure Database for PostgreSQL, service-managed encryption | TLS, `sslmode=verify-full` |
| Blob storage | Azure Storage encryption | HTTPS, SAS URLs ≤ 15 min, single-permission |
| Secrets | Azure Key Vault; never in env files committed anywhere | — |
| TOTP secrets | Sealed with the application data key (AES-256-GCM), Key Vault-wrapped | — |
| Passwords / refresh tokens / enrollment tokens | Argon2id or SHA-256; plaintext never persisted | — |
| Agent identity key | Keychain, or file 0600 root:wheel | Never transmitted |

**What is never stored anywhere**: user passwords, refresh tokens, enrollment
tokens, operator SSH private keys, SSH session content.

**Logging redaction** is enforced by a `slog` `ReplaceAttr` handler keyed on field
name (`password`, `token`, `secret`, `authorization`, `csr`, `private_key`,
`cookie`, …). Redaction by policy fails the first time someone logs a struct;
redaction in the handler cannot be bypassed by forgetting.

---

## 8. Network posture

```
Internet ──► Azure LB (TLS 1.3 only, HSTS)
              ├─ :443  REST + dashboard WS   → JWT
              ├─ :9443 device plane          → mTLS, client cert required
              └─ :2222 SSH bastion           → publickey only

  :9090 metrics/health  — private VNet only, never public
  :7443 replica relay   — private VNet, mTLS with the replica CA

Customer site: egress only. No inbound rule. No port forward. No VPN.
```

TLS 1.3 (1.2 permitted with AEAD suites only for legacy middleboxes). HSTS with
preload. Certificate transparency monitoring on the public hostname.

The device plane requires a client certificate at the TLS layer — an unenrolled
or revoked device never reaches application code.

---

## 9. Audit

Every state-changing action produces an `audit_logs` row: actor, action, resource,
device, outcome, IP, request ID, and redacted metadata. **Denied attempts are
recorded too** — a rejected privilege escalation is precisely the event worth
keeping, and `audit_logs_denied_idx` exists to query them.

The log is:

- **Append-only at the database level.** `BEFORE UPDATE` and `BEFORE DELETE`
  triggers raise unconditionally. Not a convention.
- **Hash-chained.** `hash = SHA-256(prev_hash || canonical_row)`.
  `POST /audit-logs/verify` re-walks a window and reports the first break.
- **Retained 7 years**, exported to blob storage before partitions are dropped.

---

## 10. Incident response

| Situation | Action |
|---|---|
| Device compromised | `DELETE /devices/{id}` — revokes the certificate, kills the stream, removes authorized keys on any surviving path |
| User compromised | Disable user → all sessions revoked, refresh families killed, SSH keys revoked and reconciled off every device |
| Enrollment token leaked | Revoke it; already-enrolled devices are unaffected, so this is safe to do immediately |
| Signing key compromised | Rotate: publish a new key via `ConfigUpdate`, wait for convergence, withdraw the old one; quarantine every version signed with the old key |
| Server compromised | Revoke the device CA and re-enroll; audit chain verification establishes what was tampered with; the signing key is a separate control and may well be intact |
| Bad agent release | `is_yanked = true` stops further upgrades; devices already upgraded roll back via the watchdog |

---

## 11. Deliberate limitations

Stated plainly, because a security document that claims completeness is not
trustworthy:

1. **Session content is not recorded.** See §5.1. This is a trade, not an
   oversight, and on-device recording is the answer where it is required.
2. **An admin can do a great deal.** Two-person control covers custom scripts; it
   does not cover every destructive built-in. Adding `requires_approval` to
   `shutdown` (already the default) is the lever available.
3. **The agent runs as root.** It installs packages and manages `sshd`, which
   require it. Individual command handlers drop privileges where they can
   (`run_as: edgepilot` is the custom-script default), but the daemon itself is
   root.
4. **Compromise of the signing key plus the server is fleet-wide compromise.**
   That is why the key is HSM-backed, separate from the API process, and ideally
   offline.
5. **No hardware attestation.** The agent's identity key is protected by the
   Secure Enclave where available, but EdgePilot does not attest platform
   integrity. A device that is already rooted can lie about its inventory.
