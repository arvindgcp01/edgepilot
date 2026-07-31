#!/usr/bin/env bash
#
# Verify the toolchain and install dependencies.
#
# Version checks are up front and fail loudly, because a Go or Node version
# mismatch surfaces later as a confusing compile error rather than as the
# obvious "you have the wrong version" it actually is.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

GO_MIN=1.25
NODE_MIN=20

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }
warn()  { printf '\033[33m%s\033[0m\n' "$*"; }
bold()  { printf '\033[1m%s\033[0m\n' "$*"; }

missing=0

# True when $1 >= $2 under version ordering.
version_ge() { [ "$(printf '%s\n%s\n' "$2" "$1" | sort -V | head -1)" = "$2" ]; }

require() {
    local name="$1" cmd="$2" min="${3:-}" got=""

    if ! command -v "$cmd" >/dev/null 2>&1; then
        red "  missing   $name"
        missing=$((missing + 1))
        return
    fi

    case "$cmd" in
        go)     got="$(go version | sed -E 's/.*go([0-9]+\.[0-9]+(\.[0-9]+)?).*/\1/')" ;;
        node)   got="$(node --version | tr -d 'v')" ;;
        pnpm)   got="$(pnpm --version)" ;;
        docker) got="$(docker --version | sed -E 's/.*version ([0-9.]+).*/\1/')" ;;
        *)      got="present" ;;
    esac

    if [ -n "$min" ] && [ "$got" != "present" ] && ! version_ge "$got" "$min"; then
        red "  too old   $name $got (need >= $min)"
        missing=$((missing + 1))
        return
    fi

    green "  ok        $name $got"
}

bold "Toolchain"
require "Go"     go     "$GO_MIN"
require "Node"   node   "$NODE_MIN"
require "pnpm"   pnpm
require "Docker" docker

if ! docker compose version >/dev/null 2>&1; then
    red "  missing   Docker Compose v2 (\`docker compose\`, not \`docker-compose\`)"
    missing=$((missing + 1))
else
    green "  ok        Docker Compose $(docker compose version --short 2>/dev/null)"
fi

if [ "$missing" -gt 0 ]; then
    red ""
    red "$missing prerequisite(s) missing or out of date. See README.md."
    exit 1
fi

bold $'\nGo tools'
for tool in \
    "mvdan.cc/gofumpt@latest" \
    "golang.org/x/tools/cmd/goimports@latest" \
    "golang.org/x/vuln/cmd/govulncheck@latest"
do
    name="${tool%@*}"; name="${name##*/}"
    if command -v "$name" >/dev/null 2>&1; then
        green "  ok        $name"
    else
        echo "  installing $name"
        go install "$tool" >/dev/null 2>&1 && green "  ok        $name" || warn "  failed     $name (install manually)"
    fi
done

if ! command -v golangci-lint >/dev/null 2>&1; then
    warn "  golangci-lint not found -- https://golangci-lint.run/usage/install/"
fi

bold $'\nDependencies'
for mod in apps/server apps/agent; do
    if [ -f "$mod/go.mod" ]; then
        (cd "$mod" && go mod download) && green "  ok        $mod"
    else
        warn "  skipped   $mod (no go.mod yet)"
    fi
done

if [ -f apps/dashboard/package.json ]; then
    (cd apps/dashboard && pnpm install --frozen-lockfile) && green "  ok        apps/dashboard"
else
    warn "  skipped   apps/dashboard (no package.json yet)"
fi

bold $'\nGit hooks'
if [ -d .git ]; then
    mkdir -p .git/hooks
    cat > .git/hooks/pre-commit <<'HOOK'
#!/usr/bin/env bash
# Cheap checks only. Anything slower belongs in CI, or it gets bypassed.
set -e
make lint-arch
HOOK
    chmod +x .git/hooks/pre-commit
    green "  ok        pre-commit hook installed"
else
    warn "  skipped   not a git repository"
fi

green $'\nSetup complete. Next: make dev && make migrate && make bootstrap'
