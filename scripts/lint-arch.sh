#!/usr/bin/env bash
#
# Enforce the clean-architecture import boundaries described in
# docs/Contributing.md. These rules are the difference between a layered design
# and a folder structure that merely looks layered, and they decay silently
# unless a machine checks them.
#
#   domain      imports nothing but the standard library
#   service     must not import transport, repository implementations, or a driver
#   repository  must not import transport or service
#   transport   must not import repository implementations
#
# Runs against apps/server. Passes trivially before any Go code exists, and
# starts biting the moment it does.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
SERVER="${REPO_ROOT}/apps/server/internal"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }

violations=0

# Reports any file under `layer` whose import block matches `pattern`.
# An optional fourth argument is an extended regex of allowed imports, applied
# after the pattern match.
check() {
    local layer="$1" pattern="$2" reason="$3" allow="${4:-}"
    local dir="${SERVER}/${layer}"
    [ -d "$dir" ] || return 0

    while IFS= read -r file; do
        # Only look inside the import block: a package path mentioned in a
        # comment or a string literal is not a dependency.
        local hits
        hits="$(awk '
            /^import \(/      { inblock = 1; next }
            inblock && /^\)/  { inblock = 0; next }
            inblock           { print }
            /^import "/       { print }
        ' "$file" | grep -E "$pattern" || true)"

        if [ -n "$allow" ] && [ -n "$hits" ]; then
            hits="$(printf '%s\n' "$hits" | grep -Ev "$allow" || true)"
        fi

        if [ -n "$hits" ]; then
            red "  ${file#"$REPO_ROOT"/}"
            while IFS= read -r h; do
                [ -n "$h" ] && red "      $(echo "$h" | tr -d '\t')   -- $reason"
            done <<< "$hits"
            violations=$((violations + 1))
        fi
    done < <(find "$dir" -name '*.go' -not -name '*_test.go' 2>/dev/null)
}

# domain is the core. The rule it actually needs to enforce is "no frameworks and
# no infrastructure" — not "no third-party code at all". A handful of pure value
# types are allowed by name below; anything else with a dot in its import path
# (hence a domain name, hence an external module) is rejected.
#
# The allowlist is deliberately tiny and requires a deliberate edit to extend.
# google/uuid is on it because identifiers are a domain concept, the package is a
# value type with no behaviour of its own, and the alternative — a hand-rolled
# UUIDv7 implementation — would be more code to own for no benefit.
DOMAIN_ALLOWED='github\.com/google/uuid'
check domain \
    "\"[^\"]*\.[^\"]*\"" \
    'domain must import only the standard library (plus the allowlist in this script)' \
    "$DOMAIN_ALLOWED"

check service \
    'edgepilot/(apps/server/)?internal/(transport|repository)|github.com/gin-gonic|gorm.io|google.golang.org/grpc|github.com/redis' \
    'service depends on interfaces, never on transport or a concrete repository'

check repository \
    'edgepilot/(apps/server/)?internal/(transport|service)|github.com/gin-gonic' \
    'repository must not reach upward into service or transport'

check transport \
    'edgepilot/(apps/server/)?internal/repository/' \
    'transport talks to services, never directly to a repository implementation'

# The agent must never import server internals: it is a separate binary shipped
# to customer hardware, and a shared internal package is how a server dependency
# ends up on 10,000 Macs.
if [ -d "${REPO_ROOT}/apps/agent" ]; then
    while IFS= read -r file; do
        if grep -qE '"[^"]*apps/server/internal' "$file"; then
            red "  ${file#"$REPO_ROOT"/}"
            red "      imports apps/server/internal -- the agent must not depend on server internals"
            violations=$((violations + 1))
        fi
    done < <(find "${REPO_ROOT}/apps/agent" -name '*.go' 2>/dev/null)
fi

if [ "$violations" -gt 0 ]; then
    red ""
    red "$violations file(s) violate the architecture boundaries (docs/Contributing.md)"
    exit 1
fi

green "architecture boundaries intact"
