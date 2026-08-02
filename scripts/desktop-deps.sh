#!/usr/bin/env bash
#
# Install the system libraries the Tauri desktop shell needs to compile.
#
# Tauri links against the platform webview, so a Linux build needs GTK,
# WebKit2GTK and their development headers. Without them the first failure is
# inside a transitive crate — "Package 'glib-2.0' not found" from glib-sys —
# which names a Rust crate rather than the missing distribution package, and
# sends people looking in the wrong place.
#
#   ./scripts/desktop-deps.sh
#
# Idempotent: already-satisfied packages are skipped by the package manager.

set -euo pipefail

say()  { printf '\n\033[1m%s\033[0m\n' "$*"; }
ok()   { printf '  \033[32m✔\033[0m %s\n' "$*"; }
warn() { printf '  \033[33m!\033[0m %s\n' "$*"; }
die()  { printf '  \033[31m✘\033[0m %s\n' "$*"; exit 1; }

SUDO=""
if [[ $EUID -ne 0 ]]; then
  command -v sudo >/dev/null || die "run as root or install sudo"
  SUDO="sudo"
fi

say "Detecting the platform"

if [[ "$(uname -s)" != "Linux" ]]; then
  ok "$(uname -s) needs no extra packages; Tauri uses the system webview"
  exit 0
fi

# shellcheck disable=SC1091
DISTRO="$( . /etc/os-release 2>/dev/null && echo "${ID_LIKE:-${ID:-unknown}}" )"
ok "Linux (${DISTRO})"

say "Installing packages"

case "$DISTRO" in
  *debian*|*ubuntu*)
    $SUDO apt-get update -qq

    # WebKit2GTK moved from the 4.0 ABI to 4.1 (libsoup2 → libsoup3). Tauri 2
    # wants 4.1; older distributions only carry 4.0, so try the modern name
    # and fall back rather than failing on a package that cannot exist there.
    WEBKIT="libwebkit2gtk-4.1-dev"
    if ! apt-cache show "$WEBKIT" >/dev/null 2>&1; then
      WEBKIT="libwebkit2gtk-4.0-dev"
      warn "webkit2gtk 4.1 is unavailable; falling back to 4.0"
    fi

    $SUDO apt-get install -y -q \
      build-essential \
      curl \
      file \
      libgtk-3-dev \
      "$WEBKIT" \
      libayatana-appindicator3-dev \
      librsvg2-dev \
      libssl-dev \
      libxdo-dev \
      pkg-config \
      wget
    ;;

  *fedora*|*rhel*|*centos*)
    $SUDO dnf install -y \
      gcc gcc-c++ make \
      gtk3-devel \
      webkit2gtk4.1-devel \
      libappindicator-gtk3-devel \
      librsvg2-devel \
      openssl-devel \
      libxdo-devel \
      file
    ;;

  *arch*)
    $SUDO pacman -S --needed --noconfirm \
      base-devel \
      gtk3 \
      webkit2gtk-4.1 \
      libappindicator-gtk3 \
      librsvg \
      openssl \
      xdotool
    ;;

  *suse*)
    $SUDO zypper install -y \
      gcc gcc-c++ make \
      gtk3-devel \
      webkit2gtk3-soup2-devel \
      libappindicator3-devel \
      librsvg-devel \
      libopenssl-devel \
      xdotool-devel
    ;;

  *)
    die "unrecognised distribution '${DISTRO}'. Install the equivalents of:
       gtk3, webkit2gtk 4.1, libappindicator3, librsvg2, openssl, libxdo
       and a C toolchain, then re-run the build."
    ;;
esac

say "Verifying"

MISSING=0
for pkg in glib-2.0 gobject-2.0 gtk+-3.0; do
  if pkg-config --exists "$pkg" 2>/dev/null; then
    ok "$pkg $(pkg-config --modversion "$pkg")"
  else
    warn "$pkg not found"
    MISSING=1
  fi
done

# Either webview ABI is acceptable; report which one is present.
if pkg-config --exists webkit2gtk-4.1 2>/dev/null; then
  ok "webkit2gtk-4.1 $(pkg-config --modversion webkit2gtk-4.1)"
elif pkg-config --exists webkit2gtk-4.0 2>/dev/null; then
  ok "webkit2gtk-4.0 $(pkg-config --modversion webkit2gtk-4.0)"
else
  warn "no webkit2gtk development package found"
  MISSING=1
fi

say "Rust toolchain"

if ! command -v cargo >/dev/null 2>&1; then
  warn "cargo is not installed. Install it with:"
  echo "      curl --proto '=https' --tlsv1.2 -sSf https://sh.rustup.rs | sh"
else
  RUST_VERSION="$(rustc --version | awk '{print $2}')"
  ok "rustc ${RUST_VERSION}"

  # Tauri 2's dependency tree contains manifests declaring edition2024, which
  # Cargo below 1.85 cannot parse: resolution fails before compilation with an
  # error naming an unrelated crate.
  MAJOR="${RUST_VERSION%%.*}"
  MINOR="$(cut -d. -f2 <<<"$RUST_VERSION")"
  if (( MAJOR == 1 && MINOR < 85 )); then
    warn "Rust ${RUST_VERSION} is too old; Tauri 2 needs 1.85+ for edition2024"
    echo "      rustup update stable"
  fi

  # A directory override silently pins an old toolchain, and `rustc --version`
  # above would then be reporting that pin rather than the default.
  if command -v rustup >/dev/null 2>&1; then
    ACTIVE="$(rustup show active-toolchain 2>/dev/null || true)"
    if grep -q "directory override" <<<"$ACTIVE"; then
      warn "a rustup directory override is active: ${ACTIVE%%$'\n'*}"
      echo "      clear it with: rustup override unset"
    fi
  fi
fi

if (( MISSING )); then
  die "some libraries are still missing; see the warnings above"
fi

say "Ready"
echo "  Build the desktop app with:  make desktop"
