#!/bin/sh
set -e

REPO="jlimas/parley"
BINARIES="parley parleyd"

# ── platform detection ────────────────────────────────────────────────────────

os=$(uname -s | tr '[:upper:]' '[:lower:]')
arch=$(uname -m)

case "$arch" in
  x86_64)  arch="amd64" ;;
  aarch64|arm64) arch="arm64" ;;
  *)
    echo "Unsupported architecture: $arch" >&2
    exit 1
    ;;
esac

case "$os" in
  darwin|linux) ;;
  *)
    echo "Unsupported OS: $os" >&2
    exit 1
    ;;
esac

# ── resolve install dir ───────────────────────────────────────────────────────

# Candidates, in preference order.  We pick the first one that already exists
# in PATH so the installed binaries are immediately usable.
candidates="$HOME/.local/bin $HOME/bin /usr/local/bin"

install_dir=""
for dir in $candidates; do
  case ":$PATH:" in
    *":$dir:"*)
      install_dir="$dir"
      break
      ;;
  esac
done

if [ -z "$install_dir" ]; then
  # Fall back to ~/.local/bin even if it's not in PATH yet; we'll warn below.
  install_dir="$HOME/.local/bin"
  warn_path=1
fi

mkdir -p "$install_dir"

# ── resolve latest release tag ───────────────────────────────────────────────

if [ -z "$PARLEY_VERSION" ]; then
  echo "Fetching latest release..."
  PARLEY_VERSION=$(
    curl -fsSL "https://api.github.com/repos/$REPO/releases/latest" \
      | grep '"tag_name"' \
      | sed 's/.*"tag_name": *"\([^"]*\)".*/\1/'
  )
fi

if [ -z "$PARLEY_VERSION" ]; then
  echo "Could not determine the latest release version." >&2
  echo "Set PARLEY_VERSION=vX.Y.Z and re-run to install a specific version." >&2
  exit 1
fi

echo "Installing parley $PARLEY_VERSION ($os/$arch) → $install_dir"

# ── download & verify ─────────────────────────────────────────────────────────

base_url="https://github.com/$REPO/releases/download/$PARLEY_VERSION"
archive="parley_${os}_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT

echo "Downloading $archive..."
curl -fsSL "$base_url/$archive"          -o "$tmp/$archive"
curl -fsSL "$base_url/checksums.txt"     -o "$tmp/checksums.txt"

echo "Verifying checksum..."
# checksums.txt lines are:  <sha256>  <filename>
expected=$(grep "  $archive$" "$tmp/checksums.txt" | awk '{print $1}')
if [ -z "$expected" ]; then
  echo "No checksum entry found for $archive" >&2
  exit 1
fi

if command -v sha256sum >/dev/null 2>&1; then
  actual=$(sha256sum "$tmp/$archive" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  actual=$(shasum -a 256 "$tmp/$archive" | awk '{print $1}')
else
  echo "Warning: no sha256sum or shasum found — skipping checksum verification." >&2
  actual="$expected"
fi

if [ "$actual" != "$expected" ]; then
  echo "Checksum mismatch for $archive" >&2
  echo "  expected: $expected" >&2
  echo "  got:      $actual" >&2
  exit 1
fi

# ── extract & install ─────────────────────────────────────────────────────────

tar -xzf "$tmp/$archive" -C "$tmp"

for bin in $BINARIES; do
  src="$tmp/$bin"
  if [ ! -f "$src" ]; then
    echo "Binary not found in archive: $bin" >&2
    exit 1
  fi
  # Use rm -f + cp to avoid macOS codesign-cache issues with in-place overwrite.
  rm -f "$install_dir/$bin"
  cp "$src" "$install_dir/$bin"
  chmod +x "$install_dir/$bin"
done

# ── done ──────────────────────────────────────────────────────────────────────

echo ""
echo "Installed:"
for bin in $BINARIES; do
  echo "  $install_dir/$bin"
done

if [ -n "$warn_path" ]; then
  echo ""
  echo "Warning: $install_dir is not in your PATH."
  echo "Add the following line to your shell profile (~/.bashrc, ~/.zshrc, etc.):"
  echo ""
  echo "  export PATH=\"$install_dir:\$PATH\""
  echo ""
  echo "Then restart your shell or run:  source ~/.bashrc  (or ~/.zshrc)"
fi
