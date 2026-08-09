#!/bin/sh
set -eu

repo="${HOP_INSTALL_REPO:-janiorvalle/hop}"
install_dir="${HOP_INSTALL_DIR:-$HOME/.local/bin}"
base_url="${HOP_INSTALL_BASE_URL:-}"
version="${HOP_INSTALL_VERSION:-}"
archive_name="${HOP_INSTALL_ARCHIVE:-}"

fail() {
  printf 'hop installer: %s\n' "$*" >&2
  exit 1
}

command -v curl >/dev/null 2>&1 || fail "curl is required"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported operating system; Windows users should download the release zip from https://github.com/$repo/releases" ;;
esac

case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) fail "unsupported architecture: $(uname -m)" ;;
esac

if [ -z "$version" ]; then
  release_json=$(curl -fsSL "https://api.github.com/repos/$repo/releases/latest") || fail "no published release found for $repo yet - releases appear at https://github.com/$repo/releases, or build from source: go install github.com/$repo@latest"
  version=$(printf '%s\n' "$release_json" | sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"v\{0,1\}\([^"]*\)".*/\1/p' | head -n 1)
  [ -n "$version" ] || fail "latest release did not include a tag_name"
fi
version=${version#v}

if [ -z "$archive_name" ]; then
  archive_name="hop_${version}_${os}_${arch}.tar.gz"
fi
if [ -z "$base_url" ]; then
  base_url="https://github.com/$repo/releases/download/v$version"
fi
base_url=${base_url%/}

tmp_dir=$(mktemp -d 2>/dev/null || mktemp -d -t hop-install)
stage=""
destination="$install_dir/hop"
cleanup() {
  rm -rf "$tmp_dir"
  [ -z "$stage" ] || rm -f "$stage"
}
trap cleanup EXIT HUP INT TERM
archive="$tmp_dir/$archive_name"
checksums="$tmp_dir/checksums.txt"

printf 'Downloading hop %s for %s/%s...\n' "$version" "$os" "$arch"
curl -fsSL "$base_url/$archive_name" -o "$archive" || fail "could not download $archive_name"
curl -fsSL "$base_url/checksums.txt" -o "$checksums" || fail "could not download checksums.txt"

expected=$(awk -v file="$archive_name" '$2 == file || $2 == "*" file { print $1; exit }' "$checksums")
[ -n "$expected" ] || fail "checksums.txt has no entry for $archive_name"
if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$archive" | awk '{print $1}')
else
  fail "sha256sum or shasum is required to verify the download"
fi
[ "$actual" = "$expected" ] || fail "checksum mismatch for $archive_name"

tar -xzf "$archive" -C "$tmp_dir"
[ -x "$tmp_dir/hop" ] || fail "archive did not contain hop"
# Prove the download runs before it replaces an installation that already works.
"$tmp_dir/hop" --version >/dev/null || fail "release hop failed its version smoke test"

mkdir -p "$install_dir"
stage="$install_dir/.hop.new.$$"
install -m 0755 "$tmp_dir/hop" "$stage"
mv -f "$stage" "$destination" || fail "could not replace $destination"
stage=""

"$destination" --version >/dev/null || fail "installed hop failed its version smoke test"
printf 'Installed hop to %s\n' "$destination"
case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) printf 'Add %s to PATH to run hop from any directory.\n' "$install_dir" ;;
esac
