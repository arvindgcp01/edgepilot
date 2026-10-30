#!/bin/bash
#
# One-line install of the EdgePilot agent.
#
#   curl -fsSL https://edgepilot.visionify.ai/install.sh | sudo bash -s -- <enrollment-token>
#
# The same command on a Mac and on Linux. This is a thin front for the
# platform's installer (deployments/macos/install.sh or
# deployments/linux/install.sh): it fetches that platform's agent bundle from
# the same deployment the machine is about to enrol into, then hands off to the
# installer inside it. There is deliberately only one installer — a
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

case "$(uname -s)" in
  Darwin) BUNDLE=edgepilot-agent.tgz;       STATE_DIR=/var/db/edgepilot ;;
  Linux)  BUNDLE=edgepilot-agent-linux.tgz; STATE_DIR=/var/lib/edgepilot ;;
  *) echo "EdgePilot runs on macOS and Linux, not $(uname -s)." >&2; exit 1 ;;
esac

BUNDLE_URL="${EP_BUNDLE_URL:-${SERVER_URL}/dl/${BUNDLE}}"
SUMS_URL="${EP_SUMS_URL:-${SERVER_URL}/dl/SHA256SUMS}"

# Argument first, environment second. The argument is the documented form; the
# environment is there because `bash -s -- <token>` puts the token in the
# process list, which is visible to every user on the machine for as long as the
# install runs.
TOKEN="${1:-${EP_ENROLL_TOKEN:-}}"

if [[ $EUID -ne 0 ]]; then
  echo "This must run as root: it installs a system service and writes to $STATE_DIR." >&2
  echo "  curl -fsSL ${SERVER_URL}/install.sh | sudo bash -s -- <enrollment-token>" >&2
  exit 1
fi
if [[ -z "$TOKEN" && ! -f "$STATE_DIR/device.json" ]]; then
  echo "An enrollment token is required on a machine that has not enrolled yet." >&2
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
  -o "$WORK/$BUNDLE" "$BUNDLE_URL"

# Checksum when the deployment publishes one. Absent is not fatal: it means an
# older server, and the download already came over verified TLS. A mismatch IS
# fatal — that is a bundle rewritten in transit or a half-finished publish.
if curl -fsSL --proto '=https' --tlsv1.2 -o "$WORK/SHA256SUMS" "$SUMS_URL" 2>/dev/null; then
  expected="$(awk -v f="$BUNDLE" '$2 == f || $2 == "*" f { print $1; exit }' "$WORK/SHA256SUMS")"
  # sha256sum on Linux, shasum on a Mac; neither is guaranteed on the other.
  if command -v sha256sum >/dev/null 2>&1; then
    actual="$(sha256sum "$WORK/$BUNDLE" | awk '{ print $1 }')"
  else
    actual="$(shasum -a 256 "$WORK/$BUNDLE" | awk '{ print $1 }')"
  fi
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

tar xzf "$WORK/$BUNDLE" -C "$WORK"

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
