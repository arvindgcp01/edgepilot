# Installing the agent on Linux

Ubuntu 20.04+ and Debian 11+ are tested. RHEL-family distributions use the
same files. The agent is a static binary run by systemd as root, as on a Mac.

## Install

One line, the same command as on a Mac:

```bash
curl -fsSL https://edgepilot.visionify.ai/install.sh | sudo bash -s -- <enrollment-token>
```

`/install.sh` checks `uname -s`, downloads `/dl/edgepilot-agent-linux.tgz`,
verifies it against `/dl/SHA256SUMS`, and runs the `install.sh` inside, which:

1. **Makes sure an SSH server is running.** If needed it installs
   `openssh-server` through apt, dnf or yum and enables it. The agent's tunnel
   reaches sshd on `127.0.0.1:22` only, so the installer warns if sshd listens
   elsewhere. Set `EP_SKIP_SSH_SERVER=1` to leave sshd untouched.
2. Installs the agent, the systemd unit and log rotation.
3. Enrolls with the token and waits up to 30 s to confirm it.
4. Prints the `ssh -J` command and the machine's login accounts.

Mint the token with **auto-approve** (`epctl enrollment-token -auto-approve`, or
the dashboard option) and the curl command is the only step needed on the
machine. Otherwise approve the device under Devices.

Re-running the same command without a token upgrades an enrolled machine in
place. It keeps its identity.

## SSH in

Register your public key once, for an account that exists on the Linux
machines (for example `ubuntu`):

```bash
epctl ssh-key add -org <slug> -email you@company.com -name laptop \
  -user ubuntu -key-file ~/.ssh/id_ed25519.pub
```

Then, from your own machine:

```bash
ssh -J edgepilot.visionify.ai:2222 ubuntu@<hostname-or-device-id>
```

The agent writes the key to `/etc/edgepilot/authorized_keys.d/<user>` and adds
`/etc/ssh/sshd_config.d/100-edgepilot.conf` so sshd reads it. Your own
`~/.ssh/authorized_keys` is never touched. A key that names an account the
machine doesn't have is skipped. EdgePilot never creates accounts.

## Layout

| Path | Mode | What it is |
|---|---|---|
| `/usr/local/libexec/edgepilot-agent` | `0755` | The binary |
| `/etc/systemd/system/edgepilot-agent.service` | `0644` | The unit |
| `/etc/logrotate.d/edgepilot` | `0644` | Log rotation |
| `/etc/edgepilot/` | `0755` | Must be traversable: sshd reads the key files as the target user |
| `/etc/edgepilot/enroll` | `0600` | Enrollment token, **deleted by the agent once used** |
| `/etc/edgepilot/authorized_keys.d/` | `0755` | Operator keys, one file per account |
| `/etc/edgepilot/agent.env` | `0600` | Optional overrides (`EP_LOG_LEVEL=debug`, …) |
| `/var/lib/edgepilot/` | `0700` | Device identity: key, certificate, CA bundle |
| `/var/log/edgepilot/agent.log` | `0644` | The log |

## Checking on it

```bash
systemctl status edgepilot-agent
sudo tail -f /var/log/edgepilot/agent.log
```

Exit codes are the same as on a Mac: `78` configuration, `77` revoked or
decommissioned (re-enroll), `1` anything else. systemd restarts the agent every
60 s regardless.

## Uninstall

```bash
sudo systemctl disable --now edgepilot-agent
sudo rm -f /etc/systemd/system/edgepilot-agent.service /usr/local/libexec/edgepilot-agent \
  /etc/logrotate.d/edgepilot /etc/ssh/sshd_config.d/100-edgepilot.conf
sudo rm -rf /var/lib/edgepilot /etc/edgepilot /var/log/edgepilot
sudo systemctl daemon-reload && sudo systemctl try-reload-or-restart ssh
```

Also decommission the device in EdgePilot. That's what revokes its certificate.
