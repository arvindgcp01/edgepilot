#!/bin/bash
#
# Install the EdgePilot agent on a Linux machine (Ubuntu, Debian, RHEL family).
#
#   sudo ./install.sh <enrollment-token>
#
# Normally reached through the one-liner, which fetches this bundle first:
#
#   curl -fsSL https://edgepilot.visionify.ai/install.sh | sudo bash -s -- <enrollment-token>
#
# What "installed" means here is that an operator can SSH in afterwards. So
# beyond the agent itself this makes sure an OpenSSH server is present and
# running, because a machine without one enrolls, shows up green in the
# dashboard, and then refuses every connection — the worst possible way to find
# out. EP_SKIP_SSH_SERVER=1 leaves sshd entirely alone.
#
# Everything it writes is listed in deployments/linux/README.md. It is safe to
# re-run: an enrolled machine keeps its identity and is not registered twice,
# and a re-run with a new bundle is how the agent is upgraded.

set -euo pipefail

SERVER_URL="${EP_SERVER_URL:-https://edgepilot.visionify.ai}"
TOKEN="${1:-${EP_ENROLL_TOKEN:-}}"
STATE_DIR=/var/lib/edgepilot
UNIT=/etc/systemd/system/edgepilot-agent.service

say()  { echo "==> $*"; }
warn() { echo "!!  $*" >&2; }

if [[ $EUID -ne 0 ]]; then
  echo "This must run as root: it installs a systemd service and manages sshd." >&2
  exit 1
fi
if [[ -z "$TOKEN" && ! -f "$STATE_DIR/device.json" ]]; then
  echo "Usage: sudo ./install.sh <enrollment-token>" >&2
  echo "Mint one in the dashboard under Enrollment, or with 'epctl enrollment-token'." >&2
  exit 2
fi
if ! command -v systemctl >/dev/null 2>&1 || [[ ! -d /run/systemd/system ]]; then
  echo "systemd is not running on this machine; the agent runs as a systemd service." >&2
  exit 1
fi

cd "$(dirname "$0")"

case "$(uname -m)" in
  x86_64)          BINARY=edgepilot-agent-linux-amd64 ;;
  aarch64 | arm64) BINARY=edgepilot-agent-linux-arm64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

# ── SSH server ───────────────────────────────────────────────────────────────
# The agent's tunnel connects to sshd on 127.0.0.1:22 and nowhere else (the
# allowlist is compiled in). So sshd must exist, run, and listen on loopback
# port 22.
ensure_sshd() {
  if [[ "${EP_SKIP_SSH_SERVER:-}" == 1 ]]; then
    say "Leaving the SSH server alone (EP_SKIP_SSH_SERVER=1)"
    return
  fi

  if [[ ! -x /usr/sbin/sshd ]]; then
    say "Installing the OpenSSH server"
    if command -v apt-get >/dev/null 2>&1; then
      # Noninteractive: a debconf prompt would hang a piped install forever.
      DEBIAN_FRONTEND=noninteractive apt-get update -qq
      DEBIAN_FRONTEND=noninteractive apt-get install -y -qq openssh-server >/dev/null
    elif command -v dnf >/dev/null 2>&1; then
      dnf install -y -q openssh-server
    elif command -v yum >/dev/null 2>&1; then
      yum install -y -q openssh-server
    else
      warn "No OpenSSH server and no apt/dnf/yum to install one. Install openssh-server, then re-run."
      exit 1
    fi
  fi

  # Debian/Ubuntu call the unit ssh, everything else sshd. On Ubuntu 22.10+
  # it is socket-activated: ssh.socket listens and starts the service on the
  # first connection, so the socket is what has to be enabled.
  local unit=sshd
  systemctl list-unit-files ssh.service >/dev/null 2>&1 && unit=ssh
  if systemctl list-unit-files "$unit.socket" 2>/dev/null | grep -q enabled; then
    systemctl start "$unit.socket"
  else
    systemctl enable --now "$unit.service" >/dev/null 2>&1 || systemctl start "$unit.service"
  fi
  say "SSH server is running ($unit)"

  # sshd -T needs its privilege-separation directory, which exists only while
  # the service runs; an idle socket-activated sshd has none yet.
  mkdir -p /run/sshd
  local port
  port="$(/usr/sbin/sshd -T 2>/dev/null | awk '$1 == "port" {print $2; exit}')"
  if [[ -n "$port" && "$port" != 22 ]]; then
    warn "sshd listens on port $port, but EdgePilot connects to 127.0.0.1:22."
    warn "Add 'Port 22' (it can sit alongside $port) or SSH through EdgePilot will be refused."
  fi
  if /usr/sbin/sshd -T 2>/dev/null | grep -qi '^listenaddress' && \
     ! /usr/sbin/sshd -T 2>/dev/null | grep -Eqi '^listenaddress (0\.0\.0\.0|127\.0\.0\.1|\[?::\]?)(:|$)'; then
    warn "sshd's ListenAddress excludes 127.0.0.1; SSH through EdgePilot will be refused until it listens on loopback."
  fi
}

ensure_sshd

# ── The agent ────────────────────────────────────────────────────────────────
say "Installing the agent ($BINARY)"
install -d -o root -g root -m 0755 /usr/local/libexec
# Write beside and rename, so an upgrade never leaves a half-copied binary for
# systemd to restart into.
install -o root -g root -m 0755 "$BINARY" /usr/local/libexec/edgepilot-agent.new
mv -f /usr/local/libexec/edgepilot-agent.new /usr/local/libexec/edgepilot-agent

# 0755, not 0700: sshd opens /etc/edgepilot/authorized_keys.d/<user> with the
# *target user's* privileges, so every directory on that path must be
# traversable by them. The secrets inside (the enrollment token) are 0600 on
# their own.
install -d -o root -g root -m 0755 /etc/edgepilot

if [[ -n "$TOKEN" ]]; then
  say "Writing the enrollment token"
  # 0600 from the moment it exists: umask, not a chmod after the fact.
  ( umask 077; printf '%s' "$TOKEN" > /etc/edgepilot/enroll )
fi

say "Installing log rotation"
install -o root -g root -m 0644 edgepilot.logrotate /etc/logrotate.d/edgepilot

say "Installing the systemd service"
sed "s#^Environment=EP_SERVER_URL=.*#Environment=EP_SERVER_URL=${SERVER_URL}#" \
  edgepilot-agent.service > "$UNIT.tmp"
install -o root -g root -m 0644 "$UNIT.tmp" "$UNIT"
rm -f "$UNIT.tmp"
systemctl daemon-reload
systemctl enable edgepilot-agent.service >/dev/null 2>&1
systemctl restart edgepilot-agent.service

# ── Wait for it to enroll ────────────────────────────────────────────────────
say "Enrolling with $SERVER_URL"
for _ in $(seq 1 30); do
  [[ -f "$STATE_DIR/device.json" && ! -f /etc/edgepilot/enroll ]] && break
  sleep 1
done

echo
if [[ ! -f "$STATE_DIR/device.json" ]]; then
  warn "The agent has not enrolled after 30 seconds. Its log:"
  tail -n 20 /var/log/edgepilot/agent.log 2>/dev/null >&2 || journalctl -u edgepilot-agent -n 20 --no-pager >&2
  echo >&2
  warn "It keeps retrying; follow it with: sudo tail -f /var/log/edgepilot/agent.log"
  exit 1
fi

HOST="${SERVER_URL#*://}"; HOST="${HOST%%/*}"; HOST="${HOST%%:*}"
NAME="$(hostname -s 2>/dev/null || hostname)"
# Accounts a person could log into: real uid range and a real shell.
ACCOUNTS="$(awk -F: '$3 >= 1000 && $3 < 60000 && $7 !~ /(nologin|false)$/ {print $1}' /etc/passwd | paste -sd' ' -)"

echo "Installed and enrolled: $NAME is in EdgePilot at $SERVER_URL"
echo
echo "To SSH in from your own machine:"
echo "  1. If the device shows as pending, approve it in the dashboard (Devices)."
echo "  2. Register your public key under SSH keys, for one of this machine's"
echo "     accounts${ACCOUNTS:+: $ACCOUNTS}."
echo "  3. ssh -J ${HOST}:2222 <account>@${NAME}"
echo
echo "The agent's log: sudo tail -f /var/log/edgepilot/agent.log"
