#!/bin/sh
# Install cox from the latest GitHub release.
#   curl -fsSL https://raw.githubusercontent.com/hua-ri/coxswain/main/install.sh | sh
set -eu

REPO=hua-ri/coxswain
BIN_DIR=${COX_BIN_DIR:-/usr/local/bin}

[ "$(uname -s)" = Darwin ] || { echo "cox supports macOS only" >&2; exit 1; }

tag=${COX_VERSION:-$(curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" | sed -n 's/.*"tag_name": *"\(v[^"]*\)".*/\1/p')}
[ -n "$tag" ] || { echo "could not find the latest release" >&2; exit 1; }
version=${tag#v}
archive="coxswain_${version}_darwin_universal.tar.gz"
base="https://github.com/$REPO/releases/download/$tag"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
curl -fsSL -o "$tmp/$archive" "$base/$archive"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt"
(cd "$tmp" && grep " $archive\$" checksums.txt | shasum -a 256 -c -)
tar -xzf "$tmp/$archive" -C "$tmp"
xattr -d com.apple.quarantine "$tmp/cox" 2>/dev/null || true

if [ -w "$BIN_DIR" ]; then
  install -m 0755 "$tmp/cox" "$BIN_DIR/cox"
else
  sudo install -m 0755 "$tmp/cox" "$BIN_DIR/cox"
fi
echo "installed cox $version to $BIN_DIR/cox — next: cox init"
