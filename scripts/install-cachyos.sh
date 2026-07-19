#!/usr/bin/env bash
set -euo pipefail

unit_name="dfs-mount.service"
unit_dir="${XDG_CONFIG_HOME:-$HOME/.config}/systemd/user"
unit_path="$unit_dir/$unit_name"
install_path="$HOME/.local/bin/dfs"

usage() {
  printf 'Usage: %s <repository> <mountpoint> [dfs-binary]\n' "$0" >&2
  printf '       %s --uninstall\n' "$0" >&2
}

if [[ "${1:-}" == "--uninstall" ]]; then
  systemctl --user disable --now "$unit_name" 2>/dev/null || true
  rm -f "$unit_path"
  systemctl --user daemon-reload
  printf 'Removed %s (binary retained at %s).\n' "$unit_name" "$install_path"
  exit 0
fi

if (( $# < 2 || $# > 3 )); then
  usage
  exit 2
fi

repository=$(realpath "$1")
mountpoint=$2
source_binary=${3:-./bin/dfs}
source_binary=$(realpath "$source_binary")
mkdir -p "$mountpoint" "$unit_dir" "$(dirname "$install_path")"
mountpoint=$(realpath "$mountpoint")

for command in git git-annex fusermount3 systemctl; do
  command -v "$command" >/dev/null || { printf 'Required command not found: %s\n' "$command" >&2; exit 1; }
done
if [[ ! -x "$source_binary" ]]; then
  printf 'DFS binary is not executable: %s\n' "$source_binary" >&2
  exit 1
fi
if systemctl --user is-active --quiet "$unit_name" 2>/dev/null; then
  systemctl --user stop "$unit_name"
  for _ in $(seq 1 30); do
    "$source_binary" --repo "$repository" health >/dev/null 2>&1 || break
    sleep 1
  done
fi
if "$source_binary" --repo "$repository" health >/dev/null 2>&1; then
  printf 'Repository is already mounted outside the managed service; stop that mount before installing.\n' >&2
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

cat >"$unit_path" <<EOF
[Unit]
Description=DFS managed FUSE mount
Documentation=https://github.com/bitbeamer/dfs
Wants=network-online.target
After=network-online.target

[Service]
Type=notify
NotifyAccess=main
ExecStart=$binary_arg --repo $repository_arg mount --managed --log-level info --log-format json $mountpoint_arg
ExecStartPost=$binary_arg --repo $repository_arg health
Restart=on-failure
RestartSec=5s
TimeoutStartSec=10min
TimeoutStopSec=30s
WatchdogSec=90s
UMask=0077
Environment="PATH=$HOME/.local/bin:/usr/local/bin:/usr/bin:/bin"

[Install]
WantedBy=default.target
EOF

systemctl --user daemon-reload
systemctl --user enable --now "$unit_name"
systemctl --user --no-pager --full status "$unit_name"
printf 'Installed %s. Health: %s --repo %s health\n' "$unit_name" "$install_path" "$repository"
