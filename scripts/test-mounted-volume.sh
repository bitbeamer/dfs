#!/usr/bin/env bash

# Cross-platform acceptance tests for a live DFS mount. All test data is kept
# in one uniquely named directory and removed on exit.
set -u

usage() {
  printf 'Usage: %s [--sync-timeout SECONDS] <dfs-mountpoint> <host:remote-mountpoint>...\n' "$0" >&2
  printf '       %s --local-only <dfs-mountpoint>\n' "$0" >&2
}

# The daemon's default synchronization interval is 30 seconds. Leave enough
# room for one scheduled pass plus command and network latency.
sync_timeout=45
local_only=false
while [[ $# -gt 0 ]]; do
  case "$1" in
    --sync-timeout)
      [[ $# -ge 2 ]] || { usage; exit 2; }
      sync_timeout=$2
      shift 2
      ;;
    --local-only)
      local_only=true
      shift
      ;;
    --help|-h)
      usage
      exit 0
      ;;
    --)
      shift
      break
      ;;
    -*)
      usage
      exit 2
      ;;
    *) break ;;
  esac
done

case "$sync_timeout" in
  ''|*[!0-9]*) printf 'Sync timeout must be a positive integer.\n' >&2; exit 2 ;;
esac
if [[ "$sync_timeout" -le 0 || $# -lt 1 ]]; then
  usage
  exit 2
fi

mountpoint=${1%/}
shift
peers=("$@")
if [[ "$local_only" != true && ${#peers[@]} -eq 0 ]]; then
  printf 'At least one host:mountpoint peer is required for sync verification.\n' >&2
  printf 'Use --local-only only when no other DFS peer is available.\n' >&2
  exit 2
fi
if [[ "$local_only" == true && ${#peers[@]} -ne 0 ]]; then
  printf -- '--local-only cannot be combined with peer arguments.\n' >&2
  exit 2
fi
if [[ -z "$mountpoint" || ! -d "$mountpoint" ]]; then
  printf 'DFS mountpoint is not a directory: %s\n' "$mountpoint" >&2
  exit 2
fi
mountpoint=$(cd "$mountpoint" && pwd -P)

host=$(hostname 2>/dev/null || printf unknown)
host=${host%%.*}
host=$(printf '%s' "$host" | tr -cd 'A-Za-z0-9_-')
test_root="$mountpoint/.dfs-mount-test-${host:-unknown}-$$-$(date +%s)"
tests=0
skipped=0
# macOS limits Unix socket paths to 103 bytes and its default TMPDIR is long.
# Keep this deliberately beneath /tmp so OpenSSH's hashed control name fits.
ssh_control_dir=$(mktemp -d "/tmp/dfs-ssh.XXXXXX") || {
  printf 'Could not create SSH control directory.\n' >&2
  exit 2
}
ssh_options=(
  -o BatchMode=yes
  -o ConnectTimeout=5
  -o ControlMaster=auto
  -o ControlPersist=15
  -o "ControlPath=$ssh_control_dir/%C"
)

cleanup() {
  case "$test_root" in
    "$mountpoint"/.dfs-mount-test-*)
      if [[ -e "$test_root" ]]; then
        chmod -R u+rwX "$test_root" 2>/dev/null || true
        rm -rf -- "$test_root"
      fi
      ;;
    *)
      printf 'Refusing unsafe cleanup path: %s\n' "$test_root" >&2
      ;;
  esac
  rm -rf -- "$ssh_control_dir"
}
trap cleanup EXIT HUP INT TERM

pass() {
  tests=$((tests + 1))
  printf 'ok %d - %s\n' "$tests" "$1"
}

fail() {
  tests=$((tests + 1))
  printf 'not ok %d - %s\n' "$tests" "$1" >&2
  exit 1
}

skip() {
  tests=$((tests + 1))
  skipped=$((skipped + 1))
  printf 'ok %d - %s # SKIP %s\n' "$tests" "$1" "$2"
}

shell_quote() {
  quoted=$(printf '%s' "$1" | sed "s/'/'\\\\''/g")
  printf "'%s'" "$quoted"
}

peer_parts() {
  peer_spec=$1
  peer_host=${peer_spec%%:*}
  peer_mount=${peer_spec#*:}
  if [[ -z "$peer_host" || -z "$peer_mount" || "$peer_host" == "$peer_spec" ]]; then
    return 1
  fi
  return 0
}

remote_content() {
  remote_host=$1
  remote_path=$2
  command_timeout=${3:-10}
  remote_quoted=$(shell_quote "$remote_path")
  remote_directory_quoted=$(shell_quote "$(dirname "$remote_path")")
  run_with_timeout "$command_timeout" ssh "${ssh_options[@]}" "$remote_host" \
    "ls -1A $remote_directory_quoted >/dev/null && test -f $remote_quoted && cat -- $remote_quoted" 2>/dev/null
}

remote_entry_exists() {
  remote_host=$1
  remote_path=$2
  command_timeout=${3:-2}
  remote_directory_quoted=$(shell_quote "$(dirname "$remote_path")")
  remote_name_quoted=$(shell_quote "$(basename "$remote_path")")
  run_with_timeout "$command_timeout" ssh "${ssh_options[@]}" "$remote_host" \
    "ls -1A $remote_directory_quoted 2>/dev/null | grep -F -x -- $remote_name_quoted >/dev/null" 2>/dev/null
}

remote_absent() {
  remote_host=$1
  remote_path=$2
  command_timeout=${3:-10}
  remote_quoted=$(shell_quote "$remote_path")
  remote_directory_quoted=$(shell_quote "$(dirname "$remote_path")")
  run_with_timeout "$command_timeout" ssh "${ssh_options[@]}" "$remote_host" \
    "ls -1A $remote_directory_quoted >/dev/null 2>&1 || true; test ! -e $remote_quoted" 2>/dev/null
}

# ConnectTimeout only covers opening SSH. A FUSE operation can hang after the
# connection succeeds, so bound the complete remote command without relying on
# GNU timeout, which is not installed by default on macOS.
run_with_timeout() {
  local timeout_seconds=$1
  local command_pid command_status started
  shift
  "$@" &
  command_pid=$!
  started=$SECONDS
  while kill -0 "$command_pid" 2>/dev/null; do
    if (( SECONDS - started >= timeout_seconds )); then
      kill -TERM "$command_pid" 2>/dev/null || true
      break
    fi
    sleep 0.1
  done
  wait "$command_pid"
  command_status=$?
  return "$command_status"
}

wait_for_content() {
  description=$1
  remote_host=$2
  remote_path=$3
  expected=$4
  started=$(date +%s)
  deadline=$((started + sync_timeout))
  while (( $(date +%s) < deadline )); do
    remaining=$((deadline - $(date +%s)))
    if ! remote_entry_exists "$remote_host" "$remote_path" 2; then
      sleep 1
      continue
    fi
    remaining=$((deadline - $(date +%s)))
    (( remaining > 0 )) || break
    actual=$(remote_content "$remote_host" "$remote_path" "$remaining")
    if [[ $? -eq 0 && "$actual" == "$expected" ]]; then
      elapsed=$(($(date +%s) - started))
      pass "$description on $remote_host (${elapsed}s)"
      return 0
    fi
    sleep 1
  done
  fail "$description on $remote_host (not observed within ${sync_timeout}s)"
}

wait_for_absence() {
  description=$1
  remote_host=$2
  remote_path=$3
  started=$(date +%s)
  deadline=$((started + sync_timeout))
  while (( $(date +%s) < deadline )); do
    remaining=$((deadline - $(date +%s)))
    (( remaining > 5 )) && remaining=5
    if remote_absent "$remote_host" "$remote_path" "$remaining"; then
      elapsed=$(($(date +%s) - started))
      pass "$description on $remote_host (${elapsed}s)"
      return 0
    fi
    sleep 1
  done
  fail "$description on $remote_host (not observed within ${sync_timeout}s)"
}

assert_content() {
  description=$1
  path=$2
  expected=$3
  actual=$(cat "$path" 2>/dev/null) || fail "$description (read failed)"
  [[ "$actual" == "$expected" ]] || fail "$description (got: $actual)"
  pass "$description"
}

mkdir "$test_root" || fail "create isolated test directory"
pass "create isolated test directory"

if [[ "$local_only" == true ]]; then
  skip "cross-peer create/content/rename/update/delete" "explicit --local-only mode"
else
  for peer_spec in "${peers[@]}"; do
    peer_parts "$peer_spec" || fail "parse peer specification: $peer_spec"
    peer_mount=${peer_mount%/}
    remote_mount_quoted=$(shell_quote "$peer_mount")
    if ! run_with_timeout 10 ssh "${ssh_options[@]}" "$peer_host" \
      "test -d $remote_mount_quoted" 2>/dev/null; then
      fail "reach mounted peer $peer_host:$peer_mount"
    fi
    pass "reach mounted peer $peer_host:$peer_mount"
  done

  sync_directory="$test_root/synchronized directory"
  sync_original="$sync_directory/created on ${host}.txt"
  sync_renamed="$sync_directory/renamed on ${host}.txt"
  sync_content="dfs-sync-${host}-$$-$(date +%s)"
  mkdir "$sync_directory" || fail "create synchronization test directory"
  printf '%s\n' "$sync_content" >"$sync_original" || fail "create synchronization test file"
  for peer_spec in "${peers[@]}"; do
    peer_parts "$peer_spec"
    remote_root="${peer_mount%/}/$(basename "$test_root")/synchronized directory"
    wait_for_content "sync exact created filename and content" "$peer_host" \
      "$remote_root/created on ${host}.txt" "$sync_content"
  done

  mv "$sync_original" "$sync_renamed" || fail "rename synchronization test file"
  for peer_spec in "${peers[@]}"; do
    peer_parts "$peer_spec"
    remote_root="${peer_mount%/}/$(basename "$test_root")/synchronized directory"
    wait_for_content "sync renamed filename and content" "$peer_host" \
      "$remote_root/renamed on ${host}.txt" "$sync_content"
    wait_for_absence "remove old filename after rename" "$peer_host" \
      "$remote_root/created on ${host}.txt"
  done

  sync_updated="${sync_content}-updated"
  printf '%s\n' "$sync_updated" >"$sync_renamed" || fail "update synchronization test content"
  for peer_spec in "${peers[@]}"; do
    peer_parts "$peer_spec"
    remote_root="${peer_mount%/}/$(basename "$test_root")/synchronized directory"
    wait_for_content "sync updated content" "$peer_host" \
      "$remote_root/renamed on ${host}.txt" "$sync_updated"
  done

  rm -- "$sync_renamed" || fail "delete synchronization test file"
  for peer_spec in "${peers[@]}"; do
    peer_parts "$peer_spec"
    remote_root="${peer_mount%/}/$(basename "$test_root")/synchronized directory"
    wait_for_absence "sync file deletion" "$peer_host" \
      "$remote_root/renamed on ${host}.txt"
  done
fi

printf 'alpha\n' >"$test_root/basic.txt" || fail "create file"
assert_content "create and read file" "$test_root/basic.txt" "alpha"

printf 'beta\n' >>"$test_root/basic.txt" || fail "append to file"
assert_content "append to file" "$test_root/basic.txt" $'alpha\nbeta'

cp "$test_root/basic.txt" "$test_root/copied.txt" || fail "copy file"
cmp -s "$test_root/basic.txt" "$test_root/copied.txt" || fail "copied content matches"
pass "copy file with identical content"

mkdir -p "$test_root/directory with spaces/child" || fail "create nested directories"
printf 'nested\n' >"$test_root/directory with spaces/child/item.txt" || fail "write nested file"
assert_content "nested path with spaces" "$test_root/directory with spaces/child/item.txt" "nested"

mv "$test_root/copied.txt" "$test_root/renamed.txt" || fail "rename file"
[[ ! -e "$test_root/copied.txt" ]] || fail "rename removes old file name"
assert_content "rename file" "$test_root/renamed.txt" $'alpha\nbeta'

mv "$test_root/directory with spaces" "$test_root/renamed directory" || fail "rename directory"
assert_content "rename non-empty directory" "$test_root/renamed directory/child/item.txt" "nested"

printf 'old\n' >"$test_root/destination.txt" || fail "prepare overwrite destination"
printf 'replacement\n' >"$test_root/replacement.txt" || fail "prepare overwrite source"
mv -f "$test_root/replacement.txt" "$test_root/destination.txt" || fail "atomic overwrite rename"
assert_content "rename over existing file" "$test_root/destination.txt" "replacement"

printf 'held-open\n' >"$test_root/open-rename.txt" || fail "prepare open rename"
exec 3<"$test_root/open-rename.txt" || fail "open file before rename"
mv "$test_root/open-rename.txt" "$test_root/open-renamed.txt" || fail "rename open file"
IFS= read -r open_line <&3 || fail "read renamed open descriptor"
exec 3<&-
[[ "$open_line" == "held-open" ]] || fail "open descriptor survives rename"
pass "open descriptor survives rename"

printf 'held-unlinked\n' >"$test_root/open-unlink.txt" || fail "prepare open unlink"
exec 3<"$test_root/open-unlink.txt" || fail "open file before unlink"
rm -- "$test_root/open-unlink.txt" || fail "unlink open file"
IFS= read -r unlink_line <&3 || fail "read unlinked open descriptor"
exec 3<&-
[[ "$unlink_line" == "held-unlinked" ]] || fail "open descriptor survives unlink"
pass "open descriptor survives unlink"

printf 'olddata\n' >"$test_root/two-writers.txt" || fail "prepare multiple writers"
exec 3<>"$test_root/two-writers.txt" || fail "open first writable descriptor"
exec 4<>"$test_root/two-writers.txt" || fail "open second writable descriptor"
printf 'newdata\n' >&3 || fail "write through first descriptor"
exec 3>&-
exec 4>&-
assert_content "multiple writable descriptors publish on final close" "$test_root/two-writers.txt" "newdata"

: >"$test_root/two-writers.txt" || fail "truncate file"
[[ ! -s "$test_root/two-writers.txt" ]] || fail "truncate produces empty file"
pass "truncate file"

chmod 640 "$test_root/basic.txt" || fail "change permissions"
case $(uname -s) in
  Darwin) mode=$(stat -f '%Lp' "$test_root/basic.txt") ;;
  *) mode=$(stat -c '%a' "$test_root/basic.txt") ;;
esac
[[ "$mode" == "640" ]] || fail "permissions persist (got: $mode)"
pass "change and read permissions"

touch -t 202001020304.05 "$test_root/basic.txt" || fail "set timestamp"
case $(uname -s) in
  Darwin) year=$(stat -f '%Sm' -t '%Y' "$test_root/basic.txt") ;;
  *) year=$(stat -c '%y' "$test_root/basic.txt" | cut -c1-4) ;;
esac
[[ "$year" == "2020" ]] || fail "timestamp persists (got year: $year)"
pass "change and read timestamp"

printf 'link-target\n' >"$test_root/link-target.txt" || fail "prepare symbolic link"
ln -s "link-target.txt" "$test_root/symbolic-link" || fail "create symbolic link"
assert_content "symbolic link resolves" "$test_root/symbolic-link" "link-target"

if ln "$test_root/link-target.txt" "$test_root/hard-link" 2>/dev/null; then
  printf 'through-link\n' >>"$test_root/hard-link" || fail "write through hard link"
  assert_content "hard link shares content" "$test_root/link-target.txt" $'link-target\nthrough-link'
else
  skip "hard link" "not supported by this DFS mount"
fi

unicode_name=$'R\303\251sum\303\251 \316\224.txt'
printf 'unicode\n' >"$test_root/$unicode_name" || fail "create Unicode file"
assert_content "Unicode filename" "$test_root/$unicode_name" "unicode"

printf 'case\n' >"$test_root/CaseName.txt" || fail "prepare case-only rename"
mv "$test_root/CaseName.txt" "$test_root/casename.txt" || fail "case-only rename"
assert_content "case-only rename" "$test_root/casename.txt" "case"

dd if=/dev/urandom of="$test_root/random.bin" bs=65536 count=16 2>/dev/null || fail "write 1 MiB file"
cp "$test_root/random.bin" "$test_root/random-copy.bin" || fail "copy 1 MiB file"
cmp -s "$test_root/random.bin" "$test_root/random-copy.bin" || fail "verify 1 MiB copy"
pass "write, copy, and verify 1 MiB file"

if command -v xattr >/dev/null 2>&1; then
  xattr -w user.dfs-test preserved "$test_root/basic.txt" || fail "set extended attribute"
  value=$(xattr -p user.dfs-test "$test_root/basic.txt" 2>/dev/null) || fail "read extended attribute"
  [[ "$value" == "preserved" ]] || fail "extended attribute value (got: $value)"
  xattr -d user.dfs-test "$test_root/basic.txt" || fail "remove extended attribute"
  pass "set, read, and remove extended attribute"
elif command -v setfattr >/dev/null 2>&1 && command -v getfattr >/dev/null 2>&1; then
  setfattr -n user.dfs-test -v preserved "$test_root/basic.txt" || fail "set extended attribute"
  value=$(getfattr --only-values -n user.dfs-test "$test_root/basic.txt" 2>/dev/null) || fail "read extended attribute"
  [[ "$value" == "preserved" ]] || fail "extended attribute value (got: $value)"
  setfattr -x user.dfs-test "$test_root/basic.txt" || fail "remove extended attribute"
  pass "set, read, and remove extended attribute"
else
  skip "extended attributes" "install attr on Arch Linux"
fi

find "$test_root" -mindepth 1 -maxdepth 4 -print >/dev/null || fail "enumerate test tree"
pass "enumerate directory tree"

cleanup
trap - EXIT HUP INT TERM
[[ ! -e "$test_root" ]] || fail "remove isolated test directory"
pass "remove isolated test directory"

printf 'PASS: %d mounted-volume checks (%d skipped) on %s\n' "$tests" "$skipped" "$mountpoint"
