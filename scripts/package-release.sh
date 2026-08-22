#!/usr/bin/env bash
set -euo pipefail

usage() {
	printf 'Usage: %s <version> <linux|darwin> <amd64|arm64> [output-directory]\n' "$0" >&2
}

if (( $# < 3 || $# > 4 )); then
	usage
	exit 2
fi

version=$1
target_os=$2
target_arch=$3
output_directory=${4:-dist}

if [[ ! "$version" =~ ^v[0-9]+\.[0-9]+\.[0-9]+([.-][0-9A-Za-z]+)*$ ]]; then
	printf 'Release version must be a v-prefixed semantic version: %s\n' "$version" >&2
	exit 2
fi
case "$target_os" in
	linux) installer=install-cachyos.sh ;;
	darwin) installer=install-macos.sh ;;
	*) usage; exit 2 ;;
esac
case "$target_arch" in
	amd64|arm64) ;;
	*) usage; exit 2 ;;
esac

repository=$(cd "$(dirname "$0")/.." && pwd -P)
mkdir -p "$output_directory"
output_directory=$(cd "$output_directory" && pwd -P)
archive_name="dfs-$version-$target_os-$target_arch"
temporary_directory=$(mktemp -d)
trap 'rm -rf "$temporary_directory"' EXIT
archive_root="$temporary_directory/$archive_name"
mkdir -p "$archive_root/bin" "$archive_root/scripts"

(
	cd "$repository"
	CGO_ENABLED=0 GOOS="$target_os" GOARCH="$target_arch" go build \
		-trimpath \
		-ldflags "-s -w -X github.com/bitbeamer/dfs/internal/cli.Version=$version" \
		-o "$archive_root/bin/dfs" ./cmd/dfs
)
install -m 0755 "$repository/scripts/$installer" "$archive_root/scripts/$installer"
install -m 0644 "$repository/README.md" "$repository/INSTALL.md" "$repository/LICENSE" "$archive_root/"

archive="$output_directory/$archive_name.tar.gz"
tar -C "$temporary_directory" -czf "$archive" "$archive_name"
if command -v sha256sum >/dev/null 2>&1; then
	(cd "$output_directory" && sha256sum "$(basename "$archive")" >"$(basename "$archive").sha256")
else
	(cd "$output_directory" && shasum -a 256 "$(basename "$archive")" >"$(basename "$archive").sha256")
fi

printf 'Created %s\n' "$archive"
