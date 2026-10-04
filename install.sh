#!/usr/bin/env bash
# shipit installer. Safe to run again: it upgrades the binary and keeps your
# configuration.
#
#   curl -fsSL https://raw.githubusercontent.com/beyenpay/shipit/main/install.sh | sudo bash
#
# Environment:
#   SHIPIT_VERSION  release tag to install (default: latest)
set -euo pipefail

REPO="beyenpay/shipit"
USER_NAME="shipit"
BIN="/usr/local/bin/shipit"
CONF_DIR="/etc/shipit"
CONF="$CONF_DIR/shipit.yaml"
UNIT="/etc/systemd/system/shipit.service"
HOME_DIR="/var/lib/shipit"

say() { printf '\033[1m==>\033[0m %s\n' "$*"; }
die() { printf '\033[31merror:\033[0m %s\n' "$*" >&2; exit 1; }

detect_arch() {
  case "$(uname -m)" in
    x86_64 | amd64) echo amd64 ;;
    aarch64 | arm64) echo arm64 ;;
    *) die "unsupported architecture: $(uname -m)" ;;
  esac
}

release_url() { # arch file
  if [ -n "${SHIPIT_VERSION:-}" ]; then
    echo "https://github.com/$REPO/releases/download/$SHIPIT_VERSION/$2"
  else
    echo "https://github.com/$REPO/releases/latest/download/$2"
  fi
}

# verify_sum <file> <name-in-checksums> <checksums-file>
verify_sum() {
  local want got
  want="$(awk -v n="$2" '{f=$2; sub(/^\*/, "", f)} f == n {print $1; exit}' "$3")"
  [ -n "$want" ] || die "checksums.txt has no entry for $2"
  got="$(sha256sum "$1" | awk '{print $1}')"
  [ "$want" = "$got" ] || die "checksum mismatch for $2 (want $want, got $got)"
}

gen_secret() {
  if command -v openssl > /dev/null 2>&1; then
    openssl rand -hex 32
  else
    head -c 32 /dev/urandom | od -An -tx1 | tr -d ' \n'
  fi
}

write_config() { # secret
  ( umask 077
    cat > "$CONF" << EOF
# shipit configuration. Keep this file private (mode 0600): it holds secrets.
# Edit it as the shipit user:  sudo -u $USER_NAME vi $CONF
# Changes to "projects" apply to the next request, no restart needed.
# Changing "listen" needs:     sudo systemctl restart shipit

listen: ":9000"

# Shared with CI. Put the same value into the SHIPIT_SECRET GitHub secret.
secret: "$1"

# Optional GitHub token (fine-grained, "Contents: read-only" on the repos).
# Needed for private repositories; recommended for public ones too.
token: ""

projects: {}
  # beyen-home:
  #   type: next                      # go | vite | next
  #   repo: beyenpay/home
  #   dir: /srv/app/beyen-home/web
  #   service: beyen-home             # go/next only
  #   health: http://127.0.0.1:8012/  # optional
EOF
  )
}

write_unit() {
  cat > "$UNIT" << EOF
[Unit]
Description=shipit deploy webhook
Documentation=https://github.com/$REPO
After=network-online.target
Wants=network-online.target

[Service]
User=$USER_NAME
ExecStart=$BIN -c $CONF serve
Restart=on-failure
RestartSec=3
# A deploy in progress is allowed to finish when the service stops.
TimeoutStopSec=150
# NoNewPrivileges must stay off: shipit restarts project services through sudo.

[Install]
WantedBy=multi-user.target
EOF
}

main() {
  [ "$(id -u)" -eq 0 ] || die "run as root (use sudo)"
  [ "$(uname -s)" = Linux ] || die "shipit only supports Linux"
  command -v systemctl > /dev/null 2>&1 || die "systemd is required"
  for c in curl sha256sum install awk; do
    command -v "$c" > /dev/null 2>&1 || die "missing required command: $c"
  done

  local arch name
  arch="$(detect_arch)"
  name="shipit-linux-$arch"
  # Not local: the EXIT trap runs after main returns.
  tmp="$(mktemp -d)"
  trap 'rm -rf "${tmp:-}"' EXIT

  say "downloading $name (${SHIPIT_VERSION:-latest})"
  curl -fsSL --retry 3 -o "$tmp/$name" "$(release_url "$arch" "$name")" || die "download failed"
  curl -fsSL --retry 3 -o "$tmp/checksums.txt" "$(release_url "$arch" checksums.txt)" || die "checksums download failed"
  verify_sum "$tmp/$name" "$name" "$tmp/checksums.txt"

  say "installing $BIN"
  install -m 0755 -o root -g root "$tmp/$name" "$BIN.new"
  mv -f "$BIN.new" "$BIN"

  if id "$USER_NAME" > /dev/null 2>&1; then
    say "user $USER_NAME already exists"
  else
    say "creating user $USER_NAME"
    useradd --system --create-home --home-dir "$HOME_DIR" --shell /usr/sbin/nologin "$USER_NAME"
  fi

  install -d -m 0700 -o "$USER_NAME" -g "$USER_NAME" "$CONF_DIR"
  if [ -e "$CONF" ]; then
    say "keeping existing $CONF"
  else
    say "writing $CONF (a random secret was generated)"
    write_config "$(gen_secret)"
    chown "$USER_NAME:$USER_NAME" "$CONF"
    chmod 0600 "$CONF"
  fi

  say "installing systemd unit"
  write_unit
  systemctl daemon-reload
  systemctl enable shipit.service > /dev/null 2>&1
  if systemctl is-active --quiet shipit.service; then
    systemctl restart shipit.service
  else
    systemctl start shipit.service
  fi

  "$BIN" version
  cat << EOF

shipit is installed and running.

Next steps:
  1. Edit the config:   sudo -u $USER_NAME vi $CONF
       - add projects, and a GitHub token if you deploy private repositories
       - the webhook secret is already set; copy it into the SHIPIT_SECRET GitHub secret
  2. Open the webhook port (default 9000/tcp) in your firewall.
  3. Create each project's directory (owned by $USER_NAME), its systemd unit
     (User=$USER_NAME, WorkingDirectory=<dir>/current) and a sudoers line:
       $USER_NAME ALL=(root) NOPASSWD: /usr/bin/systemctl restart <service>
     See https://github.com/$REPO/tree/main/examples
  4. Verify everything:  sudo shipit check

No need for 'sudo -u $USER_NAME': run shipit as root (or with sudo) and it
switches to the $USER_NAME user by itself where that matters.
EOF
}

# Allow tests to source this file without running the installer.
if [ "${SHIPIT_INSTALL_SOURCED:-}" != 1 ]; then
  main "$@"
fi
