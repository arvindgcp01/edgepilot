# Installing the agent on a Mac

The agent is a single static binary plus two configuration files. It runs as a
root LaunchDaemon, so it starts before anyone logs in — these Macs are headless.

## Layout

| Path | Owner | Mode | What it is |
|---|---|---|---|
| `/usr/local/libexec/edgepilot-agent` | `root:wheel` | `0755` | The binary |
| `/Library/LaunchDaemons/ai.visionify.edgepilot.agent.plist` | `root:wheel` | `0644` | The daemon definition |
| `/etc/newsyslog.d/edgepilot.conf` | `root:wheel` | `0644` | Log rotation |
| `/etc/edgepilot/enroll` | `root:wheel` | `0600` | The enrollment token — **removed by the agent once used** |
| `/var/db/edgepilot/` | `root:wheel` | `0700` | Device identity: key, certificate, CA bundle |
| `/var/log/edgepilot/` | `root:wheel` | `0755` | Logs |

launchd refuses to load a daemon whose plist is writable by anyone but root, and
the agent refuses to load an identity whose key is readable by anyone but root.
Both checks exist because the alternative is a local user who can impersonate the
device.

## Install

```bash
# 1. The binary. Universal, so one package covers Apple silicon and Intel.
sudo install -o root -g wheel -m 0755 edgepilot-agent /usr/local/libexec/edgepilot-agent

# 2. The daemon definition, with the customer's server substituted in.
sudo install -o root -g wheel -m 0644 \
  ai.visionify.edgepilot.agent.plist \
  /Library/LaunchDaemons/ai.visionify.edgepilot.agent.plist
sudo /usr/libexec/PlistBuddy \
  -c "Set :EnvironmentVariables:EP_SERVER_URL https://edgepilot.acme.com" \
  /Library/LaunchDaemons/ai.visionify.edgepilot.agent.plist

# 3. Log rotation.
sudo install -o root -g wheel -m 0644 \
  edgepilot.newsyslog.conf /etc/newsyslog.d/edgepilot.conf

# 4. The enrollment token, from `epctl enrollment-token` or the dashboard.
sudo mkdir -p -m 0700 /etc/edgepilot
printf '%s' "$EP_ENROLL_TOKEN" | sudo tee /etc/edgepilot/enroll >/dev/null
sudo chmod 0600 /etc/edgepilot/enroll

# 5. Start it.
sudo launchctl load -w /Library/LaunchDaemons/ai.visionify.edgepilot.agent.plist
```

The agent enrolls on its first run, writes its identity to `/var/db/edgepilot`,
and **deletes `/etc/edgepilot/enroll`**. A single-use credential left on disk
outlives its usefulness immediately and outlives the machine's ownership for
years.

## Checking on it

```bash
sudo launchctl print system/ai.visionify.edgepilot.agent
sudo tail -f /var/log/edgepilot/agent.log
```

## Exit codes

launchd restarts the agent whenever it exits. The exit code says whether that
restart can accomplish anything:

| Code | Meaning | What to do |
|---|---|---|
| `0` | Stopped cleanly | Nothing |
| `78` | The configuration is wrong or incomplete | Fix the plist's environment, then `launchctl kickstart -k` |
| `77` | The server revoked or decommissioned this device, or its certificate expired | Re-enroll it; restarting will fail identically |
| `1` | Something else | Read `/var/log/edgepilot/agent.log` |

A device that hits `77` keeps being restarted by launchd and keeps exiting `77`,
sixty seconds apart. That is intentional: it is visible in `launchctl print`, it
costs nothing, and it recovers by itself if an administrator re-approves the
device.

## Uninstall

```bash
sudo launchctl unload -w /Library/LaunchDaemons/ai.visionify.edgepilot.agent.plist
sudo rm -f /Library/LaunchDaemons/ai.visionify.edgepilot.agent.plist
sudo rm -f /usr/local/libexec/edgepilot-agent
sudo rm -f /etc/newsyslog.d/edgepilot.conf
sudo rm -rf /var/db/edgepilot /etc/edgepilot
```

Decommission the device in EdgePilot as well as removing it here. Removing the
agent stops it reporting; decommissioning is what revokes its certificate, and an
un-revoked certificate on a wiped Mac is a credential still capable of
connecting.
