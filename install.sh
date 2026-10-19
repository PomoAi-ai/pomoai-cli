#!/bin/sh
# Install the pomoai CLI from GitHub Releases.
#   curl -fsSL https://raw.githubusercontent.com/PomoAi-ai/pomoai-cli/main/install.sh | sh
# Env: POMOAI_VERSION (e.g. 0.1.0; default latest), POMOAI_INSTALL_DIR (default ~/.local/bin).
set -eu

repo="PomoAi-ai/pomoai-cli"
install_dir="${POMOAI_INSTALL_DIR:-$HOME/.local/bin}"

fail() { echo "pomoai install: $*" >&2; exit 1; }

case "$(uname -s)" in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  *) fail "unsupported OS $(uname -s); on Windows use: npm i -g https://github.com/PomoAi-ai/pomoai-cli/releases/latest/download/pomoai-cli.tgz" ;;
esac
case "$(uname -m)" in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) fail "unsupported architecture $(uname -m)" ;;
esac

version="${POMOAI_VERSION:-latest}"
if [ "$version" = latest ]; then
  base="https://github.com/$repo/releases/latest/download"
else
  base="https://github.com/$repo/releases/download/v${version#v}"
fi

if command -v sha256sum >/dev/null 2>&1; then
  sha() { sha256sum "$1" | cut -d' ' -f1; }
elif command -v shasum >/dev/null 2>&1; then
  sha() { shasum -a 256 "$1" | cut -d' ' -f1; }
else
  fail "need sha256sum or shasum to verify the download"
fi

asset="pomoai_${os}_${arch}.tar.gz"
tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT INT TERM

echo "downloading $base/$asset" >&2
curl -fsSL -o "$tmp/$asset" "$base/$asset" || fail "download failed: $base/$asset"
curl -fsSL -o "$tmp/checksums.txt" "$base/checksums.txt" || fail "download failed: $base/checksums.txt"

want="$(awk -v f="$asset" '$2 == f { print $1 }' "$tmp/checksums.txt")"
[ -n "$want" ] || fail "$asset not listed in checksums.txt"
got="$(sha "$tmp/$asset")"
[ "$want" = "$got" ] || fail "checksum mismatch for $asset (want $want, got $got)"

tar -xzf "$tmp/$asset" -C "$tmp" pomoai
mkdir -p "$install_dir"
install -m 0755 "$tmp/pomoai" "$install_dir/pomoai"
echo "installed $install_dir/pomoai" >&2
"$install_dir/pomoai" version >&2

case ":$PATH:" in
  *":$install_dir:"*) ;;
  *) echo "note: $install_dir is not on PATH; add: export PATH=\"$install_dir:\$PATH\"" >&2 ;;
esac
