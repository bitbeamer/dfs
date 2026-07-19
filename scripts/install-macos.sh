#!/usr/bin/env bash
set -euo pipefail

label="io.bitbeamer.dfs.mount"
domain="gui/$(id -u)"
support_dir="$HOME/Library/Application Support/DFS"
install_path="$support_dir/bin/dfs"
plist_dir="$HOME/Library/LaunchAgents"
plist_path="$plist_dir/$label.plist"
log_dir="$HOME/Library/Logs/DFS"

usage() {
  printf 'Usage: %s <repository> <mountpoint> [dfs-binary]\n' "$0" >&2
  printf '       %s --uninstall\n' "$0" >&2
}

if [[ "${1:-}" == "--uninstall" ]]; then
  launchctl bootout "$domain/$label" 2>/dev/null || true
  rm -f "$plist_path"
  printf 'Removed %s (binary retained at %s).\n' "$label" "$install_path"
  exit 0
fi

if (( $# < 2 || $# > 3 )); then
  usage
  exit 2
fi

absolute_path() {
  local directory base
  directory=$(cd "$(dirname "$1")" && pwd -P)
  base=$(basename "$1")
  printf '%s/%s\n' "$directory" "$base"
}

xml_escape() {
  printf '%s' "$1" | sed -e 's/&/\&amp;/g' -e 's/</\&lt;/g' -e 's/>/\&gt;/g' -e 's/"/\&quot;/g' -e "s/'/\\\&apos;/g"
}

repository=$(absolute_path "$1")
mountpoint=$2
source_binary=${3:-./bin/dfs}
source_binary=$(absolute_path "$source_binary")
mkdir -p "$mountpoint" "$support_dir/bin" "$plist_dir" "$log_dir"
mountpoint=$(absolute_path "$mountpoint")

for command in git git-annex launchctl plutil; do
  command -v "$command" >/dev/null || { printf 'Required command not found: %s\n' "$command" >&2; exit 1; }
done
if [[ ! -x "$source_binary" ]]; then
  printf 'DFS binary is not executable: %s\n' "$source_binary" >&2
  exit 1
fi
if launchctl print "$domain/$label" >/dev/null 2>&1; then
  launchctl bootout "$domain/$label"
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

binary_xml=$(xml_escape "$install_path")
repository_xml=$(xml_escape "$repository")
mountpoint_xml=$(xml_escape "$mountpoint")
stdout_xml=$(xml_escape "$log_dir/mount.stdout.log")
stderr_xml=$(xml_escape "$log_dir/mount.stderr.log")

cat >"$plist_path" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$label</string>
  <key>ProgramArguments</key>
  <array>
    <string>$binary_xml</string>
    <string>--repo</string>
    <string>$repository_xml</string>
    <string>mount</string>
    <string>--managed</string>
    <string>--log-level</string>
    <string>info</string>
    <string>--log-format</string>
    <string>json</string>
    <string>$mountpoint_xml</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict>
    <key>PATH</key>
    <string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string>
  </dict>
  <key>RunAtLoad</key>
  <true/>
  <key>KeepAlive</key>
  <dict>
    <key>SuccessfulExit</key>
    <false/>
  </dict>
  <key>ProcessType</key>
  <string>Background</string>
  <key>ThrottleInterval</key>
  <integer>5</integer>
  <key>StandardOutPath</key>
  <string>$stdout_xml</string>
  <key>StandardErrorPath</key>
  <string>$stderr_xml</string>
</dict>
</plist>
EOF

plutil -lint "$plist_path"
launchctl bootout "$domain/$label" 2>/dev/null || true
launchctl bootstrap "$domain" "$plist_path"
launchctl kickstart "$domain/$label"

for _ in $(seq 1 120); do
  if "$install_path" --repo "$repository" health >/dev/null 2>&1; then
    printf 'Installed %s and mounted %s.\n' "$label" "$mountpoint"
    exit 0
  fi
  sleep 1
done
printf 'Service did not become healthy; inspect %s and run: launchctl print %s/%s\n' "$stderr_xml" "$domain" "$label" >&2
exit 1
