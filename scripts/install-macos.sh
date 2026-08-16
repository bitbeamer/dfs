#!/usr/bin/env bash
set -euo pipefail

domain="gui/$(id -u)"
support_dir="$HOME/Library/Application Support/DFS"
app_dir="$support_dir/DFS.app"
install_path="$app_dir/Contents/MacOS/dfs"
legacy_install_path="$support_dir/bin/dfs"
info_path="$app_dir/Contents/Info.plist"
plist_dir="$HOME/Library/LaunchAgents"
log_dir="$HOME/Library/Logs/DFS"
pair_port=7843

usage() {
  printf 'Usage: %s [--pair-port PORT] <repository> <mountpoint> [dfs-binary]\n' "$0" >&2
  printf '       %s --uninstall <repository>\n' "$0" >&2
}

if [[ "${1:-}" == "--uninstall" ]]; then
  [[ $# -eq 2 ]] || { usage; exit 2; }
  repository=$(cd "$(dirname "$2")" && pwd -P)/$(basename "$2")
  filesystem_id=$(git -C "$repository" rev-list --max-parents=0 HEAD | sort | head -n 1)
  instance=${filesystem_id:0:12}
  [[ "$instance" =~ ^[0-9a-f]{12}$ ]] || { printf 'Cannot determine DFS filesystem ID for %s.\n' "$repository" >&2; exit 1; }
  label="io.bitbeamer.dfs.mount.$instance"
  core_label="io.bitbeamer.dfs.core.$instance"
  launchctl bootout "$domain/$label" 2>/dev/null || true
  launchctl bootout "$domain/$core_label" 2>/dev/null || true
  rm -f "$plist_dir/$label.plist" "$plist_dir/$core_label.plist"
  printf 'Removed %s and %s (application retained at %s).\n' "$core_label" "$label" "$app_dir"
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
mkdir -p "$mountpoint" "$app_dir/Contents/MacOS" "$support_dir/bin" "$plist_dir" "$log_dir"
mountpoint=$(absolute_path "$mountpoint")
filesystem_id=$(git -C "$repository" rev-list --max-parents=0 HEAD | sort | head -n 1)
instance=${filesystem_id:0:12}
[[ "$instance" =~ ^[0-9a-f]{12}$ ]] || { printf 'Cannot determine DFS filesystem ID for %s.\n' "$repository" >&2; exit 1; }
label="io.bitbeamer.dfs.mount.$instance"
plist_path="$plist_dir/$label.plist"
core_label="io.bitbeamer.dfs.core.$instance"
core_plist_path="$plist_dir/$core_label.plist"
legacy_plist_path="$plist_dir/io.bitbeamer.dfs.mount.plist"

for command in codesign git git-annex launchctl plutil; do
  command -v "$command" >/dev/null || { printf 'Required command not found: %s\n' "$command" >&2; exit 1; }
done
if [[ ! -x "$source_binary" ]]; then
  printf 'DFS binary is not executable: %s\n' "$source_binary" >&2
  exit 1
fi
# Migrate the original singleton launch agent only when it belongs to this
# repository. Other filesystem instances remain running.
if [[ -f "$legacy_plist_path" ]] && grep -Fq -- "$repository" "$legacy_plist_path"; then
  launchctl bootout "$domain/io.bitbeamer.dfs.mount" 2>/dev/null || true
  rm -f "$legacy_plist_path"
fi
if launchctl print "$domain/$label" >/dev/null 2>&1 || launchctl print "$domain/$core_label" >/dev/null 2>&1; then
  launchctl bootout "$domain/$label" 2>/dev/null || true
  launchctl bootout "$domain/$core_label" 2>/dev/null || true
  for _ in $(seq 1 30); do
    "$source_binary" --repo "$repository" health >/dev/null 2>&1 || break
    sleep 1
  done
fi
if "$source_binary" --repo "$repository" health >/dev/null 2>&1; then
  printf 'Repository core is already running outside the managed service; stop it before installing.\n' >&2
  exit 1
fi
if [[ "$source_binary" != "$install_path" ]]; then
  install -m 0755 "$source_binary" "$install_path"
fi

cat >"$info_path" <<'EOF'
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>CFBundleDevelopmentRegion</key>
  <string>en</string>
  <key>CFBundleDisplayName</key>
  <string>DFS</string>
  <key>CFBundleExecutable</key>
  <string>dfs</string>
  <key>CFBundleIdentifier</key>
  <string>io.bitbeamer.dfs</string>
  <key>CFBundleInfoDictionaryVersion</key>
  <string>6.0</string>
  <key>CFBundleName</key>
  <string>DFS</string>
  <key>CFBundlePackageType</key>
  <string>APPL</string>
  <key>CFBundleShortVersionString</key>
  <string>1.0</string>
  <key>CFBundleVersion</key>
  <string>1</string>
  <key>LSUIElement</key>
  <true/>
  <key>NSBonjourServices</key>
  <array>
    <string>_dfs._udp</string>
  </array>
  <key>NSLocalNetworkUsageDescription</key>
  <string>DFS discovers and connects to peers on your local network.</string>
</dict>
</plist>
EOF
plutil -lint "$info_path"
codesign --force --deep --sign - --identifier io.bitbeamer.dfs "$app_dir"
codesign --verify --deep --strict "$app_dir"
ln -sfn "../DFS.app/Contents/MacOS/dfs" "$legacy_install_path"

binary_xml=$(xml_escape "$install_path")
repository_xml=$(xml_escape "$repository")
mountpoint_xml=$(xml_escape "$mountpoint")
stdout_xml=$(xml_escape "$log_dir/mount-$instance.stdout.log")
stderr_xml=$(xml_escape "$log_dir/mount-$instance.stderr.log")
core_stdout_xml=$(xml_escape "$log_dir/core-$instance.stdout.log")
core_stderr_xml=$(xml_escape "$log_dir/core-$instance.stderr.log")

cat >"$core_plist_path" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key>
  <string>$core_label</string>
  <key>ProgramArguments</key>
  <array>
    <string>$binary_xml</string>
    <string>--repo</string>
    <string>$repository_xml</string>
    <string>daemon</string>
    <string>--managed</string>
    <string>--pair-port</string>
    <string>$pair_port</string>
    <string>--mountpoint</string>
    <string>$mountpoint_xml</string>
    <string>--log-level</string>
    <string>info</string>
    <string>--log-format</string>
    <string>json</string>
  </array>
  <key>EnvironmentVariables</key>
  <dict><key>PATH</key><string>/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin:/usr/sbin:/sbin</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><dict><key>SuccessfulExit</key><false/></dict>
  <key>ProcessType</key><string>Background</string>
  <key>ThrottleInterval</key><integer>5</integer>
  <key>StandardOutPath</key><string>$core_stdout_xml</string>
  <key>StandardErrorPath</key><string>$core_stderr_xml</string>
</dict>
</plist>
EOF

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
plutil -lint "$core_plist_path"
launchctl bootout "$domain/$label" 2>/dev/null || true
launchctl bootout "$domain/$core_label" 2>/dev/null || true
bootstrapped=false
for _ in $(seq 1 30); do
  if launchctl bootstrap "$domain" "$core_plist_path"; then
    bootstrapped=true
    break
  fi
  sleep 1
done
if [[ "$bootstrapped" != true ]]; then
  printf 'Could not bootstrap %s after waiting for launchd teardown.\n' "$core_label" >&2
  exit 1
fi
launchctl kickstart "$domain/$core_label"

for _ in $(seq 1 120); do
  if "$install_path" --repo "$repository" health >/dev/null 2>&1; then
    launchctl bootstrap "$domain" "$plist_path"
    launchctl kickstart "$domain/$label"
    for _ in $(seq 1 60); do
      if mount | grep -Fq " on $mountpoint "; then
        printf 'Installed %s and %s; mounted %s.\n' "$core_label" "$label" "$mountpoint"
        exit 0
      fi
      sleep 1
    done
    printf 'Core is healthy but %s did not mount; inspect %s.\n' "$label" "$stderr_xml" >&2
    exit 1
  fi
  sleep 1
done
printf 'Core did not become healthy; inspect %s and run: launchctl print %s/%s\n' "$core_stderr_xml" "$domain" "$core_label" >&2
exit 1
