#!/bin/sh
# Installs toli on macOS or Linux:
#
#   curl -fsSL https://raw.githubusercontent.com/arjunvlama/toli/main/install.sh | sh
#
# TOLI_VERSION=v0.1.0 picks a release (default: the latest).
# TOLI_INSTALL_DIR picks where toli goes (default: ~/.local/bin).
# TOLI_DOWNLOAD_URL downloads from somewhere else, e.g. a local build for testing.
set -eu

repo="arjunvlama/toli"
dir="${TOLI_INSTALL_DIR:-$HOME/.local/bin}"

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) echo "toli: unsupported OS $(uname -s); on Windows, use install.ps1" >&2; exit 1 ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) echo "toli: unsupported CPU $(uname -m)" >&2; exit 1 ;;
esac

if [ -n "${TOLI_DOWNLOAD_URL:-}" ]; then
  base="$TOLI_DOWNLOAD_URL"
elif [ -n "${TOLI_VERSION:-}" ]; then
  base="https://github.com/$repo/releases/download/$TOLI_VERSION"
else
  base="https://github.com/$repo/releases/latest/download"
fi
archive="toli_${os}_${arch}.tar.gz"

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $archive"
curl -fsSL "$base/$archive" -o "$tmp/$archive"
curl -fsSL "$base/checksums.txt" -o "$tmp/checksums.txt"

want="$(grep " $archive\$" "$tmp/checksums.txt" | cut -d' ' -f1)"
if command -v sha256sum >/dev/null 2>&1; then
  got="$(sha256sum "$tmp/$archive" | cut -d' ' -f1)"
else
  got="$(shasum -a 256 "$tmp/$archive" | cut -d' ' -f1)"
fi
if [ -z "$want" ] || [ "$want" != "$got" ]; then
  echo "toli: checksum mismatch for $archive" >&2
  exit 1
fi

tar -xzf "$tmp/$archive" -C "$tmp" toli
mkdir -p "$dir"
mv "$tmp/toli" "$dir/toli"
chmod +x "$dir/toli"
echo "Installed toli to $dir/toli"

case ":$PATH:" in
  *":$dir:"*) ;;
  *) echo "Add $dir to your PATH, e.g. in ~/.zshrc or ~/.bashrc:"
     echo "  export PATH=\"$dir:\$PATH\"" ;;
esac
echo 'Next: run `toli daemon install`, then `toli`.'
