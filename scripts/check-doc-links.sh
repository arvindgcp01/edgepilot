#!/usr/bin/env bash
#
# Verify that every relative link in the Markdown documentation resolves to a
# file that exists. Documentation that points at nothing is worse than absent
# documentation: it costs a reader time before it fails them.
#
# External (http/https) links and pure anchors are out of scope.

set -uo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "$REPO_ROOT"

red()   { printf '\033[31m%s\033[0m\n' "$*"; }
green() { printf '\033[32m%s\033[0m\n' "$*"; }

broken=0
checked=0

while IFS= read -r doc; do
    dir="$(dirname "$doc")"

    # Extract the target of every ](...) link.
    while IFS= read -r target; do
        [ -z "$target" ] && continue
        case "$target" in
            http://*|https://*|mailto:*|'#'*) continue ;;
        esac

        # Strip any trailing anchor.
        path="${target%%#*}"
        [ -z "$path" ] && continue

        if [ "${path#/}" != "$path" ]; then
            resolved="${REPO_ROOT}${path}"      # repo-absolute
        else
            resolved="${dir}/${path}"           # relative to the document
        fi

        checked=$((checked + 1))
        if [ ! -e "$resolved" ]; then
            red "  broken: $doc -> $target"
            broken=$((broken + 1))
        fi
    done < <(grep -oE '\]\([^)]+\)' "$doc" | sed -E 's/^\]\(//; s/\)$//' | sed -E 's/ +".*"$//')
# node_modules is excluded wherever it appears, not only at the root: the
# dashboard has its own, and a vendored package's README is not documentation
# this repository is responsible for.
done < <(find . -name '*.md' \
    -not -path '*/node_modules/*' \
    -not -path './.git/*' \
    -not -path '*/.next/*' | sort)

if [ "$broken" -gt 0 ]; then
    red "$broken broken link(s) out of $checked checked"
    exit 1
fi

green "all $checked internal documentation links resolve"
