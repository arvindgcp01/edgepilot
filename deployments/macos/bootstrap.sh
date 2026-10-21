#!/bin/bash
#
# One-line install of the EdgePilot agent.
#
#   curl -fsSL https://edgepilot.visionify.ai/install.sh | sudo bash -s -- <enrollment-token>
#
# This is a thin front for deployments/macos/install.sh: it fetches the agent
# bundle from the same deployment the Mac is about to enrol into, then hands off
# to the installer inside it. There is deliberately only one installer — a
# second copy of the install logic living in the download path is a copy that
# drifts, and the drift only ever surfaces on a customer's machine.
#
# The bundle is served unauthenticated, and has to be: a Mac that has not
# enrolled yet holds no credential to present. What admits a device to the fleet
# is the enrollment token, not possession of the binary.

set -euo pipefail

# The deployment this Mac will enrol into. Also the default download origin, so
# an agent can never be installed from one deployment and pointed at another
# without saying so explicitly.
SERVER_URL="${EP_SERVER_URL:-https://edgepilot.visionify.ai}"
BUNDLE_URL="${EP_BUNDLE_URL:-${SERVER_URL}/dl/edgepilot-agent.tgz}"
SUMS_URL="${EP_SUMS_URL:-${SERVER_URL}/dl/SHA256SUMS}"

# Argument first, environment second. The argument is the documented form; the
# environment is there because `bash -s -- <token>` puts the token in the
# process list, which is visible to every user on the machine for as long as the
# install runs.
TOKEN="${1:-${EP_ENROLL_TOKEN:-}}"

if [[ $EUID -ne 0 ]]; then
  echo "This must run as root: it installs a LaunchDaemon and writes to /var/db." >&2
  echo "  curl -fsSL ${SERVER_URL}/install.sh | sudo bash -s -- <enrollment-token>" >&2
  exit 1
fi
if [[ -z "$TOKEN" && ! -f /var/db/edgepilot/device.json ]]; then
  echo "An enrollment token is required on a Mac that has not enrolled yet." >&2
  echo "  curl -fsSL ${SERVER_URL}/install.sh | sudo bash -s -- <enrollment-token>" >&2
  echo "Mint one in the dashboard under Enrollment, or with 'epctl enrollment-token'." >&2
  exit 2
fi

WORK="$(mktemp -d /tmp/edgepilot-install.XXXXXX)"
trap 'rm -rf "$WORK"' EXIT

echo "==> Downloading the agent bundle from $SERVER_URL"
# --proto '=https' refuses a plaintext redirect. The bundle is an unsigned
# binary that is about to run as root, so the TLS session is the only thing
# establishing where it came from; downgrading it to http would make the
# checksum below pointless as well, since both would come from the same
# unauthenticated hop.
curl -fsSL --proto '=https' --tlsv1.2 --retry 3 --retry-delay 2 \
  -o "$WORK/edgepilot-agent.tgz" "$BUNDLE_URL"

# Checksum when the deployment publishes one. Absent is not fatal: it means an
# older server, and the download already came over verified TLS. A mismatch IS
# fatal — that is a bundle rewritten in transit or a half-finished publish.
if curl -fsSL --proto '=https' --tlsv1.2 -o "$WORK/SHA256SUMS" "$SUMS_URL" 2>/dev/null; then
  expected="$(awk '$2 ~ /edgepilot-agent\.tgz$/ { print $1; exit }' "$WORK/SHA256SUMS")"
  actual="$(shasum -a 256 "$WORK/edgepilot-agent.tgz" | awk '{ print $1 }')"
  if [[ -z "$expected" ]]; then
    echo "==> SHA256SUMS does not list the bundle; skipping verification"
  elif [[ "$expected" != "$actual" ]]; then
    echo "Checksum mismatch — refusing to install." >&2
    echo "  expected $expected" >&2
    echo "  got      $actual" >&2
    exit 1
  else
    echo "==> Checksum verified"
  fi
else
  echo "==> No SHA256SUMS published; skipping verification"
fi

tar xzf "$WORK/edgepilot-agent.tgz" -C "$WORK"

# Locate the installer rather than assuming the directory name inside the
# tarball, so repackaging under a versioned directory does not break this.
INSTALLER="$(find "$WORK" -maxdepth 3 -type f -name install.sh -print -quit)"
if [[ -z "$INSTALLER" ]]; then
  echo "The bundle contains no install.sh — it is not an agent bundle." >&2
  exit 1
fi
chmod +x "$INSTALLER"

echo "==> Handing off to the installer"
cd "$(dirname "$INSTALLER")"
# Not exec: the EXIT trap has to run so the token does not sit in /tmp.
EP_SERVER_URL="$SERVER_URL" ./install.sh "$TOKEN"
