#!/bin/bash
#
# Install the EdgePilot agent on a Mac.
#
#   sudo ./install.sh <enrollment-token>
#
# Everything it writes is listed in deployments/macos/README.md. It is safe to
# re-run: an already-enrolled Mac keeps its identity and is not registered twice.

set -euo pipefail

SERVER_URL="${EP_SERVER_URL:-https://edgepilot.visionify.ai}"
TOKEN="${1:-}"

if [[ $EUID -ne 0 ]]; then
  echo "This must run as root: it installs a LaunchDaemon and writes to /var/db." >&2
  exit 1
fi
if [[ -z "$TOKEN" && ! -f /var/db/edgepilot/device.json ]]; then
  echo "Usage: sudo ./install.sh <enrollment-token>" >&2
  echo "Mint one in the dashboard under Enrollment, or with 'epctl enrollment-token'." >&2
  exit 2
fi

cd "$(dirname "$0")"

# Apple silicon or Intel. A universal binary would be tidier, but shipping the
# right one is simpler than requiring lipo on the build host.
case "$(uname -m)" in
  arm64)  BINARY=edgepilot-agent-arm64 ;;
  x86_64) BINARY=edgepilot-agent-amd64 ;;
  *) echo "Unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

echo "==> Installing the agent ($BINARY)"
# BSD install does not create parent directories, and /usr/local/libexec does
# not exist on a Mac that has never had Homebrew or a similar tool on it. The
# failure is "No such file or directory" naming install's own temporary file,
# which is not obviously about the destination at all.
install -d -o root -g wheel -m 0755 /usr/local /usr/local/libexec
install -o root -g wheel -m 0755 "$BINARY" /usr/local/libexec/edgepilot-agent

# Strip the quarantine attribute. Anything downloaded through a browser carries
# it, and Gatekeeper refuses to execute a quarantined unsigned binary — which
# launchd reports only as the daemon failing to start.
#
# The right long-term answer is to sign and notarise the agent, so this stops
# being necessary. Until then it is removed deliberately and visibly rather
# than by telling an operator to disable Gatekeeper.
xattr -d com.apple.quarantine /usr/local/libexec/edgepilot-agent 2>/dev/null || true

# No CA to pin: the server presents a publicly-trusted certificate, so the
# agent verifies it against the system trust store like anything else. If you
# move this deployment behind a private CA, copy its root here and set
# EP_SERVER_CA_FILE in the plist — the agent has no insecure mode, deliberately.
install -d -o root -g wheel -m 0755 /etc/edgepilot

if [[ -n "$TOKEN" ]]; then
  echo "==> Writing the enrollment token"
  # 0600: it is a credential until the moment it is redeemed, and the agent
  # deletes it the instant enrollment succeeds.
  printf '%s' "$TOKEN" > /etc/edgepilot/enroll
  chown root:wheel /etc/edgepilot/enroll
  chmod 0600 /etc/edgepilot/enroll
fi

echo "==> Creating the state directory"
# launchd refuses to spawn a process whose WorkingDirectory does not exist, and
# the plist sets it to this path. The agent would create it on first save, but
# it never gets to run — the failure appears as a daemon that will not start,
# with nothing in any log, because the agent is what writes the logs.
#
# 0700: the device's private key lives here.
install -d -o root -g wheel -m 0700 /var/db/edgepilot

echo "==> Installing log rotation"
install -o root -g wheel -m 0644 edgepilot.newsyslog.conf /etc/newsyslog.d/edgepilot.conf
install -d -o root -g wheel -m 0755 /var/log/edgepilot

echo "==> Installing the LaunchDaemon"
PLIST=/Library/LaunchDaemons/ai.visionify.edgepilot.agent.plist
install -o root -g wheel -m 0644 ai.visionify.edgepilot.agent.plist "$PLIST"
/usr/libexec/PlistBuddy -c "Set :EnvironmentVariables:EP_SERVER_URL $SERVER_URL" "$PLIST"

echo "==> Starting"
launchctl unload -w "$PLIST" 2>/dev/null || true
launchctl load -w "$PLIST"

echo
echo "Installed. Watch it enrol and connect with:"
echo "  sudo tail -f /var/log/edgepilot/agent.log"
echo
echo "It should appear in the dashboard at $SERVER_URL within a few seconds."
