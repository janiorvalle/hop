#!/bin/sh
set -eu

root=$(CDPATH='' cd -- "$(dirname "$0")/.." && pwd)
case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) printf 'the installer smoke only covers the Unix release targets\n'; exit 0 ;;
esac
case "$(uname -m)" in
  x86_64|amd64) arch=amd64 ;;
  arm64|aarch64) arch=arm64 ;;
  *) printf 'unsupported smoke architecture: %s\n' "$(uname -m)" >&2; exit 1 ;;
esac

archive=$(find "$root/dist" -maxdepth 1 -type f -name "hop_*_${os}_${arch}.tar.gz" -print | head -n 1)
[ -n "$archive" ] || { printf 'snapshot archive not found for %s/%s; run make snapshot first\n' "$os" "$arch" >&2; exit 1; }
archive_name=$(basename "$archive")
version=${archive_name#hop_}
version=${version%_"${os}"_"${arch}".tar.gz}
work_dir=$(mktemp -d 2>/dev/null || mktemp -d -t hop-install-smoke)
trap 'rm -rf "$work_dir"' EXIT HUP INT TERM

install_from_dist() {
  HOP_INSTALL_BASE_URL="file://$1" \
  HOP_INSTALL_VERSION="$version" \
  HOP_INSTALL_ARCHIVE="$archive_name" \
  HOP_INSTALL_DIR="$2" \
    "$root/install.sh"
}

install_from_dist "$root/dist" "$work_dir/bin"
# Re-running over an existing installation is intentionally supported.
install_from_dist "$root/dist" "$work_dir/bin"
"$work_dir/bin/hop" --version

corrupt_dist="$work_dir/corrupt-dist"
mkdir -p "$corrupt_dist"
cp "$archive" "$corrupt_dist/$archive_name"
printf '%064d  %s\n' 0 "$archive_name" > "$corrupt_dist/checksums.txt"
if install_from_dist "$corrupt_dist" "$work_dir/corrupt-bin" >"$work_dir/corrupt.log" 2>&1; then
  printf 'the installer accepted a checksum mismatch\n' >&2
  exit 1
fi
grep -q 'checksum mismatch' "$work_dir/corrupt.log"
printf 'installer smoke passed for %s/%s at version %s\n' "$os" "$arch" "$version"
