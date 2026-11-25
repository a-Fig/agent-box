#!/bin/sh
# Installs agent-box's client, box, on Linux or macOS: downloads the latest stable release's binary
# for this machine, checks it against the release's SHA256SUMS and puts it in ~/.local/bin. It never
# needs sudo. Windows has install.ps1. Shaped like a-Fig/murmur's install.sh.
#
#   curl -fsSL https://raw.githubusercontent.com/a-Fig/agent-box/main/install.sh | sh
#
# Environment:
#   BOX_VERSION      install this release (v0.1.2 or 0.1.2) instead of the latest stable one
#   BOX_INSTALL_DIR  install here instead of ~/.local/bin
set -eu

repo=a-Fig/agent-box

say() { printf '%s\n' "$*"; }
die() { printf 'box install: %s\n' "$*" >&2; exit 1; }

[ -n "${HOME:-}" ] || die 'HOME is not set, so there is nowhere to install to.'
command -v curl >/dev/null 2>&1 || die 'curl is required and was not found.'

case $(uname -s) in
  Darwin) os=darwin ;;
  Linux) os=linux ;;
  MINGW* | MSYS* | CYGWIN*) die 'this is Windows: run install.ps1 in PowerShell instead.' ;;
  *) die "unsupported operating system $(uname -s); box has binaries for Linux, macOS and Windows." ;;
esac
case $(uname -m) in
  x86_64 | amd64) arch=amd64 ;;
  arm64 | aarch64) arch=arm64 ;;
  *) die "unsupported architecture $(uname -m); box has binaries for amd64 and arm64." ;;
esac
# A shell under Rosetta reports x86_64 on Apple silicon; the hardware answers truthfully.
if [ "$os" = darwin ] && [ "$(sysctl -n hw.optional.arm64 2>/dev/null || true)" = 1 ]; then
  arch=arm64
fi

asset=box-$os-$arch
dir=${BOX_INSTALL_DIR:-$HOME/.local/bin}
tmp=$(mktemp -d "${TMPDIR:-/tmp}/box-install.XXXXXX") || die 'could not make a temporary folder.'
trap 'rm -rf "$tmp"; rm -f "$dir/.box.new.$$"' EXIT INT TERM

if [ -n "${BOX_VERSION:-}" ]; then
  case $BOX_VERSION in v*) tag=$BOX_VERSION ;; *) tag=v$BOX_VERSION ;; esac
  base=https://github.com/$repo/releases/download/$tag
else
  tag=
  base=https://github.com/$repo/releases/latest/download
fi
fetch() {
  curl -q -fsSL --retry 2 -o "$tmp/$1" "$base/$1" || die "could not download $base/$1."
}

say "Downloading $asset${tag:+ ($tag)} from $repo..."
fetch "$asset"
fetch SHA256SUMS

# SHA256SUMS lines read "<hash>  <name>", or "<hash> *<name>" for a binary file.
want=$(awk -v n="$asset" '{ f = $2; sub(/^\*/, "", f); if (f == n) { print tolower($1); exit } }' "$tmp/SHA256SUMS")
[ -n "$want" ] || die "SHA256SUMS has no line for $asset."
if command -v sha256sum >/dev/null 2>&1; then got=$(sha256sum <"$tmp/$asset" | awk '{ print tolower($1) }')
elif command -v shasum >/dev/null 2>&1; then got=$(shasum -a 256 <"$tmp/$asset" | awk '{ print tolower($1) }')
else die 'no sha256sum or shasum to check the download with, so nothing was installed.'
fi
[ "$got" = "$want" ] || die "the checksum of $asset does not match SHA256SUMS (wanted $want, got $got); nothing was installed."
say 'Checksum OK.'

mkdir -p "$dir" || die "could not create $dir."
# Copied beside the destination and renamed over it: the rename is atomic, and safe while a box
# process runs the old file (copying over it would fail with "text file busy").
cp "$tmp/$asset" "$dir/.box.new.$$" || die "could not write to $dir."
chmod 755 "$dir/.box.new.$$"
mv -f "$dir/.box.new.$$" "$dir/box" || die "could not put box at $dir/box."

"$dir/box" --version || die "installed $dir/box, but it did not run."
say "Installed: $dir/box"
case ":$PATH:" in
  *":$dir:"*) ;;
  *) say "$dir is not on PATH. Agents don't need it (box join registers the full path), but your shell does:"
     say "  export PATH=\"$dir:\$PATH\"" ;;
esac
say ''
say 'Next:'
command -v incus >/dev/null 2>&1 ||
  say '  - install the incus client (Debian/Ubuntu: apt install incus-client; macOS: brew install incus)'
say '  - add the host as a remote named pc, with the line its setup printed:'
say '      incus remote add pc https://<host>:8443 --token <token>'
say "  - give your coding agents the box tools: $dir/box join"
