#!/usr/bin/env bash
# codex_command for ralphex's external codex review: routes the run through agrouter.
# ralphex execs codex_command as one executable (no word splitting), calls it as
# `<codex_command> exec <codex options>` and pipes the prompt on stdin.
# agrouter emits codex's own `exec` (exec_args in .agrouter/config), so the leading
# one is dropped here; every other option reaches codex unchanged after "--".
# --doc files feed agrouter's project-complexity stage only; codex never sees them.
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

docs=(
  --doc="$root/AGENTS.md"
  --doc="$root/README.md"
)

if [[ "${1:-}" == exec ]]; then
  shift
fi

exec agrouter exec codex "${docs[@]}" -- "$@"
