#!/usr/bin/env bash
set -euo pipefail

unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
install_path="$HOME/.local/bin/dfs"
pair_port=7843

usage() {
  printf 'Usage: %s [--pair-port PORT] <repository> <mountpoint> [dfs-binary]\n' "$0" >&2
  printf '       %s --uninstall <repository>\n' "$0" >&2
}

if [[ "${1:-}" == "--uninstall" ]]; then
  [[ $# -eq 2 ]] || { usage; exit 2; }
  repository=$(realpath "$2")
  filesystem_id=$(git -C "$repository" rev-list --max-parents=0 HEAD | sort | head -n 1)
  instance=${filesystem_id:0:12}
  [[ "$instance" =~ ^[0-9a-f]{12}$ ]] || { printf 'Cannot determine DFS filesystem ID for %s.\n' "$repository" >&2; exit 1; }
  mount_unit_name="dfs-mount-$instance.service"
  core_unit_name="dfs-core-$instance.service"
  systemctl --user disable --now "$mount_unit_name" "$core_unit_name" 2>/dev/null || true
  rm -f "$unit_dir/$mount_unit_name" "$unit_dir/$core_unit_name"
  systemctl --user daemon-reload
  printf 'Removed %s and %s (binary retained at %s).\n' "$mount_unit_name" "$core_unit_name" "$install_path"
  exit 0
fi

if [[ "${1:-}" == "--pair-port" ]]; then
  [[ $# -ge 3 ]] || { usage; exit 2; }
  pair_port=$2
  shift 2
fi
if (( $# < 2 || $# > 3 )); then
  usage
  exit 2
fi
[[ "$pair_port" =~ ^[0-9]+$ ]] && (( pair_port >= 1 && pair_port <= 65535 )) || {
  printf 'Pairing port must be between 1 and 65535.\n' >&2
  exit 2
}

repository=$(realpath "$1")
mountpoint=$2
source_binary=${3:-./bin/dfs}
source_binary=$(realpath "$source_binary")
mkdir -p "$mountpoint" "$unit_dir" "$(dirname "$install_path")"
mountpoint=$(realpath "$mountpoint")
filesystem_id=$(git -C "$repository" rev-list --max-parents=0 HEAD | sort | head -n 1)
instance=${filesystem_id:0:12}
[[ "$instance" =~ ^[0-9a-f]{12}$ ]] || { printf 'Cannot determine DFS filesystem ID for %s.\n' "$repository" >&2; exit 1; }
unit_name="dfs-mount-$instance.service"
unit_path="$unit_dir/$unit_name"
core_unit_name="dfs-core-$instance.service"
core_unit_path="$unit_dir/$core_unit_name"
legacy_unit_path="$unit_dir/dfs-mount.service"

for command in git git-annex fusermount3 mountpoint systemctl; do
  command -v "$command" >/dev/null || { printf 'Required command not found: %s\n' "$command" >&2; exit 1; }
done
if [[ ! -x "$source_binary" ]]; then
  printf 'DFS binary is not executable: %s\n' "$source_binary" >&2
  exit 1
fi
# Migrate the original singleton unit only when it belongs to this repository.
if [[ -f "$legacy_unit_path" ]] && grep -Fq -- "$repository" "$legacy_unit_path"; then
  systemctl --user disable --now dfs-mount.service 2>/dev/null || true
  rm -f "$legacy_unit_path"
  systemctl --user daemon-reload
fi
if systemctl --user is-active --quiet "$unit_name" 2>/dev/null || systemctl --user is-active --quiet "$core_unit_name" 2>/dev/null; then
  systemctl --user stop "$unit_name" "$core_unit_name"
  for _ in $(seq 1 30); do
    "$source_binary" --repo "$repository" health >/dev/null 2>&1 || break
    sleep 1
  done
fi
if mountpoint -q "$mountpoint"; then
  printf 'Mountpoint is already active outside the managed service; unmount it before installing.\n' >&2
  exit 1
fi
if [[ "$source_binary" != "$install_path" ]]; then
  install -m 0755 "$source_binary" "$install_path"
fi

systemd_quote() {
  local value=$1
  value=${value//\\/\\\\}
  value=${value//\"/\\\"}
  value=${value//%/%%}
  printf '"%s"' "$value"
}

binary_arg=$(systemd_quote "$install_path")
repository_arg=$(systemd_quote "$repository")
mountpoint_arg=$(systemd_quote "$mountpoint")

cat >"$core_unit_path" <<EOF
[Unit]
Description=DFS independent core daemon
Documentation=https://github.com/bitbeamer/dfs
Wants=network-online.target
After=network-online.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=$binary_arg --repo $repository_arg daemon --managed --pair-port $pair_port --mountpoint $mountpoint_arg --log-level info --log-format json
ExecStartPost=$binary_arg --repo $repository_arg health
Restart=on-failure
RestartSec=5s
TimeoutStartSec=2min
TimeoutStopSec=30s
WatchdogSec=90s
UMask=0077
Environment="PATH=$HOME/.local/bin:/usr/local/bin:/usr/bin:/bin"

[Install]
WantedBy=default.target
EOF

cat >"$unit_path" <<EOF
[Unit]
Description=DFS FUSE frontend
Documentation=https://github.com/bitbeamer/dfs
Requires=$core_unit_name
After=$core_unit_name

[Service]
Type=simple
ExecStart=$binary_arg --repo $repository_arg mount --managed --log-level info --log-format json $mountpoint_arg
Restart=on-failure
RestartSec=5s
TimeoutStopSec=30s
UMask=0077
Environment="PATH=$HOME/.local/bin:/usr/local/bin:/usr/bin:/bin"

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now "$core_unit_name"
systemctl --user enable --now "$unit_name"
systemctl --user --no-pager --full status "$unit_name"
printf 'Installed %s and %s. Health: %s --repo %s health\n' "$core_unit_name" "$unit_name" "$install_path" "$repository"
