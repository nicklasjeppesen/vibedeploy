#!/bin/sh
# Installerer Vibe Deploy på en Linux-server (Debian/Ubuntu) med én kommando:
#
#   curl -fsSL https://github.com/nicklasjeppesen/vibedeploy/releases/latest/download/install.sh | sh
#
# Scriptet installerer Docker, Caddy, git og en firewall, henter vibedeploy fra GitHub Releases
# og kører «vibedeploy install», som spørger om domæne og e-mail og starter tjenesten.
#
# Uden terminal (fx i Hetzners «Cloud config») gives svarene som miljøvariabler:
#   VIBEDEPLOY_DOMAIN, VIBEDEPLOY_EMAIL og VIBEDEPLOY_SETUP_CODE (en kode på mindst 12 tegn, du selv vælger).
# VIBEDEPLOY_REPO (ejer/repo) eller VIBEDEPLOY_URL (direkte link, ARCH erstattes) vælger, hvor binæren hentes.
set -eu

REPO="${VIBEDEPLOY_REPO:-nicklasjeppesen/vibedeploy}"
INSTALL_DIR='/usr/local/bin'

main() {
  check_os
  arch=$(detect_arch)
  sudo_cmd=$(sudo_prefix)
  install_packages
  ensure_docker
  ensure_caddy
  setup_firewall
  install_binary "$arch"
  run_install
}

say() { printf '\n==> %s\n' "$*"; }
die() { printf '\nFEJL: %s\n' "$*" >&2; exit 1; }

check_os() {
  [ "$(uname -s)" = "Linux" ] || die "Vibe Deploy installeres på en Linux-server. På din egen Mac: go run . -runtime demo"
  command -v apt-get >/dev/null 2>&1 || die "Scriptet kræver Debian eller Ubuntu (apt-get)."
  command -v systemctl >/dev/null 2>&1 || die "Scriptet kræver systemd."
}

detect_arch() {
  case "$(uname -m)" in
  x86_64) echo "amd64" ;;
  aarch64 | arm64) echo "arm64" ;;
  *) die "Ukendt processortype: $(uname -m)" ;;
  esac
}

sudo_prefix() {
  if [ "$(id -u)" -eq 0 ]; then
    echo ""
  elif command -v sudo >/dev/null 2>&1; then
    echo "sudo"
  else
    die "Kør scriptet som root, eller installér sudo."
  fi
}

as_root() { $sudo_cmd "$@"; }

install_packages() {
  say "Systempakker"
  as_root env DEBIAN_FRONTEND=noninteractive apt-get update -q
  as_root env DEBIAN_FRONTEND=noninteractive apt-get install -yq ca-certificates curl gnupg git openssh-client ufw unattended-upgrades debian-keyring debian-archive-keyring apt-transport-https
}

ensure_docker() {
  if command -v docker >/dev/null 2>&1; then
    say "Docker er allerede installeret"
  else
    say "Docker"
    curl -fsSL https://get.docker.com | as_root sh
  fi
  as_root systemctl enable --now docker
}

ensure_caddy() {
  if command -v caddy >/dev/null 2>&1; then
    say "Caddy er allerede installeret"
    return
  fi
  say "Caddy (https og certifikater)"
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/gpg.key | as_root gpg --batch --yes --dearmor -o /usr/share/keyrings/caddy-stable-archive-keyring.gpg
  curl -1sLf https://dl.cloudsmith.io/public/caddy/stable/debian.deb.txt | as_root tee /etc/apt/sources.list.d/caddy-stable.list >/dev/null
  as_root env DEBIAN_FRONTEND=noninteractive apt-get update -q
  as_root env DEBIAN_FRONTEND=noninteractive apt-get install -yq caddy
}

setup_firewall() {
  say "Firewall (kun SSH, http og https)"
  as_root ufw allow OpenSSH >/dev/null
  as_root ufw allow 80/tcp >/dev/null
  as_root ufw allow 443/tcp >/dev/null
  as_root ufw --force enable >/dev/null
}

install_binary() {
  arch="$1"
  if [ -n "${VIBEDEPLOY_URL:-}" ]; then
    url=$(echo "$VIBEDEPLOY_URL" | sed "s/ARCH/$arch/g")
  else
    url="https://github.com/${REPO}/releases/latest/download/vibedeploy-linux-${arch}"
  fi
  say "Vibe Deploy hentes fra $url"
  tmp=$(mktemp)
  curl -fsSL "$url" -o "$tmp" || die "Kunne ikke hente $url"
  as_root install -m 755 "$tmp" "${INSTALL_DIR}/vibedeploy"
  rm -f "$tmp"
}

run_install() {
  # Miljøvariablerne sendes med gennem sudo; spørgsmålene stilles via /dev/tty, så det virker med «curl | sh».
  set -- env VIBEDEPLOY_DOMAIN="${VIBEDEPLOY_DOMAIN:-}" VIBEDEPLOY_EMAIL="${VIBEDEPLOY_EMAIL:-}" VIBEDEPLOY_SETUP_CODE="${VIBEDEPLOY_SETUP_CODE:-}" "${INSTALL_DIR}/vibedeploy" install
  if [ -t 1 ] && { : </dev/tty; } 2>/dev/null; then
    as_root "$@" </dev/tty
  else
    as_root "$@"
  fi
}

main "$@"
