#!/usr/bin/env bash
set -euo pipefail

usage() {
  printf 'Usage: %s [--dry-run] [--no-fetch]\n' "$0" >&2
  printf 'Fetch origin/main, build DFS, and transactionally upgrade this development host.\n' >&2
}

dry_run=false
fetch=true
while (( $# > 0 )); do
  case "$1" in
    --dry-run)
      dry_run=true
      shift
      ;;
    --no-fetch)
      fetch=false
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    *)
      usage
      exit 2
      ;;
  esac
done

script_dir=$(cd "$(dirname "$0")" && pwd -P)
source_dir=$(cd "$script_dir/.." && pwd -P)
cd "$source_dir"

# Upgrade is host-scoped. Ambient selectors must not narrow or redirect its
# inventory, and health verification below selects every filesystem explicitly.
unset DFS_FILESYSTEM DFS_REPO

for dependency in git go make; do
  command -v "$dependency" >/dev/null || {
    printf 'Required development command not found: %s\n' "$dependency" >&2
    exit 1
  }
done

if [[ -n "$(git status --porcelain --untracked-files=normal)" ]]; then
  printf 'Refusing to update from a dirty DFS source checkout: %s\n' "$source_dir" >&2
  git status --short >&2
  exit 1
fi

if [[ "$fetch" == true ]]; then
  if [[ -d .jj ]] && command -v jj >/dev/null; then
    if [[ "$(jj log -r @ --no-graph -T 'empty')" != true ]]; then
      printf 'Refusing to move a non-empty Jujutsu working-copy commit.\n' >&2
      exit 1
    fi
    jj git fetch --remote origin
    jj rebase -r @ -d main@origin
    jj bookmark set main -r @-
  else
    branch=$(git symbolic-ref --quiet --short HEAD || true)
    if [[ "$branch" != main ]]; then
      printf 'The DFS Git checkout must be on main, not %s.\n' "${branch:-detached HEAD}" >&2
      exit 1
    fi
    git pull --ff-only origin main
  fi
fi

make build
candidate="$source_dir/bin/dfs"
installed=$(command -v dfs || true)
if [[ -z "$installed" ]]; then
  printf 'No installed dfs executable is on PATH; complete dfs setup first.\n' >&2
  exit 1
fi

printf 'Candidate: %s\n' "$("$candidate" --version)"
printf 'Installed: %s\n' "$("$installed" --version)"

verify_host() {
  "$installed" service list
  service_json=$("$installed" service list --output json)
  filesystem_ids=$(printf '%s\n' "$service_json" | grep -o '"filesystem_id":"[^"]*"' | sed 's/.*:"\([^"]*\)"/\1/' | sort -u)
  if [[ -z "$filesystem_ids" ]]; then
    printf 'The upgraded host reports no managed DFS filesystems.\n' >&2
    return 1
  fi
  while IFS= read -r filesystem_id; do
    [[ -n "$filesystem_id" ]] || continue
    "$installed" health --filesystem "$filesystem_id"
  done <<<"$filesystem_ids"
}

if cmp -s "$candidate" "$installed"; then
  printf 'DFS is already current on this host.\n'
  verify_host
  exit 0
fi

"$candidate" upgrade --from "$candidate" --dry-run
if [[ "$dry_run" == true ]]; then
  printf 'Dry run complete; no installed executable or service was changed.\n'
  exit 0
fi

"$candidate" upgrade --from "$candidate" --yes
verify_host
printf 'Development upgrade completed: %s\n' "$("$installed" --version)"
