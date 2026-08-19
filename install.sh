#!/bin/sh
# Tendrils installer for Linux and macOS.
#
#   curl -fsSL https://raw.githubusercontent.com/punkscience/tendrils/main/install.sh | sh
#
# Downloads the latest release, verifies it against the release's checksums.txt,
# and installs tendrils + blossomd. No toolchain required.
#
# Falls back to building from source (needs Go 1.26+ and git) when there is no
# release for this platform.
#
# Env: TENDRILS_BIN_DIR  install directory (default ~/.local/bin)
#      TENDRILS_VERSION  install a specific tag, e.g. v0.1.0 (default: latest)
set -eu

SLUG="punkscience/tendrils"
REPO="https://github.com/$SLUG.git"
BIN="tendrils"
DEST="${TENDRILS_BIN_DIR:-$HOME/.local/bin}"

info() { printf '\033[1;36m==>\033[0m %s\n' "$1"; }
warn() { printf '\033[1;33mwarn:\033[0m %s\n' "$1" >&2; }
die()  { printf '\033[1;31mERROR:\033[0m %s\n' "$1" >&2; exit 1; }

TMP=$(mktemp -d)
trap 'rm -rf "$TMP"' EXIT

# --- platform -----------------------------------------------------------------

case "$(uname -s)" in
  Linux)  OS=linux  ;;
  Darwin) OS=darwin ;;
  *)      OS="" ;;
esac
case "$(uname -m)" in
  x86_64|amd64)  ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  *)             ARCH="" ;;
esac

# --- helpers ------------------------------------------------------------------

sha256_of() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "$1" | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 "$1" | awk '{print $1}'
  else
    # No verification is available, so there is nothing to compare against. An
    # unverified binary is not worth installing silently.
    die "neither sha256sum nor shasum is available; cannot verify the download"
  fi
}

latest_version() {
  curl -fsSL "https://api.github.com/repos/$SLUG/releases/latest" 2>/dev/null |
    sed -n 's/.*"tag_name"[[:space:]]*:[[:space:]]*"\([^"]*\)".*/\1/p' | head -1
}

install_release() {
  ver="$1"
  archive="tendrils_${ver#v}_${OS}_${ARCH}.tar.gz"
  base="https://github.com/$SLUG/releases/download/$ver"

  info "Downloading $archive"
  curl -fsSL -o "$TMP/$archive" "$base/$archive" || return 1
  curl -fsSL -o "$TMP/checksums.txt" "$base/checksums.txt" || return 1

  want=$(awk -v f="$archive" '$2 == f {print $1}' "$TMP/checksums.txt")
  [ -n "$want" ] || die "$archive is not listed in the release checksums"
  got=$(sha256_of "$TMP/$archive")
  [ "$want" = "$got" ] || die "checksum mismatch for $archive (expected $want, got $got)"
  info "Checksum verified"

  tar -xzf "$TMP/$archive" -C "$TMP" || return 1
  [ -f "$TMP/$BIN" ] || return 1
}

build_from_source() {
  command -v go  >/dev/null 2>&1 || die "no release available for this platform and Go 1.26+ is not installed (https://go.dev/dl/)"
  command -v git >/dev/null 2>&1 || die "no release available for this platform and git is not installed"
  info "Building from source ($(go version | awk '{print $3}'))"
  git clone --depth 1 "$REPO" "$TMP/src" >/dev/null 2>&1 || die "git clone failed"
  ( cd "$TMP/src" && go build -o "$TMP/$BIN" ./cmd/tendrils && go build -o "$TMP/blossomd" ./cmd/blossomd ) || die "build failed"
}

# --- install ------------------------------------------------------------------

info "Detected $(uname -s)/$(uname -m)"

installed=""
if [ -n "$OS" ] && [ -n "$ARCH" ]; then
  VERSION="${TENDRILS_VERSION:-$(latest_version)}"
  if [ -n "$VERSION" ]; then
    if install_release "$VERSION"; then
      installed="$VERSION"
    else
      warn "could not install the $VERSION release; falling back to a source build"
    fi
  else
    warn "no published release found; falling back to a source build"
  fi
else
  warn "$(uname -s)/$(uname -m) has no prebuilt release; falling back to a source build"
fi
[ -n "$installed" ] || build_from_source

mkdir -p "$DEST"
install -m 0755 "$TMP/$BIN" "$DEST/$BIN"
info "Installed $DEST/$BIN"
# blossomd is the optional self-hosted blob server. Shipping it here is what
# lets someone run their own storage without installing a Go toolchain.
if [ -f "$TMP/blossomd" ]; then
  install -m 0755 "$TMP/blossomd" "$DEST/blossomd"
  info "Installed $DEST/blossomd (optional Blossom blob server)"
fi

info "$("$DEST/$BIN" version | head -1)"

case ":$PATH:" in
  *":$DEST:"*) : ;;
  *) info "NOTE: $DEST is not on your PATH — add it:  export PATH=\"$DEST:\$PATH\"" ;;
esac

cat <<EOF

Tendrils installed. Next steps:
  1. $BIN keygen                            # create your master key — BACK UP the nsec
  2. $BIN enroll --key <nsec> --root <folder> \
       --relay wss://<relay> --blossom http://<blossom>:8091
  3. $BIN daemon --interval 1m              # start syncing

You need a Nostr relay and a Blossom server (run the bundled 'blossomd' to
self-host one). Enroll every device with the SAME key to sync them.
See https://github.com/punkscience/tendrils for details.
EOF
