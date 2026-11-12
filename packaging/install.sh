#!/bin/sh
# Install towamori-agent as a systemd service.
#
#   curl -fsSL https://app.towamori.com/install.sh | sudo sh
#
# Without a token it prints a pairing code; type it in the Towamori console
# (Add server) and the script picks up the server's token by itself.
# With TOWAMORI_TOKEN=<token> set it skips pairing, for scripted installs.
#
# Optional: TOWAMORI_VERSION=0.1.0 to pin a release, TOWAMORI_ENDPOINT to override the endpoint,
# TOWAMORI_DOWNLOAD_BASE=https://… to download from somewhere other than GitHub releases.
# Running it again upgrades the binary and keeps your existing /etc/towamori/agent.env.
set -eu

REPO="A1oneSKy/towamori"
BIN="/usr/local/bin/towamori-agent"
CONF_DIR="/etc/towamori"
CONF="$CONF_DIR/agent.env"
UNIT="/etc/systemd/system/towamori-agent.service"

say() { printf '%s\n' "towamori: $*"; }
die() { printf '%s\n' "towamori: error: $*" >&2; exit 1; }

[ "$(id -u)" -eq 0 ] || die "run as root (pipe into 'sudo sh')"
[ "$(uname -s)" = "Linux" ] || die "only Linux is supported"
command -v systemctl >/dev/null 2>&1 || die "systemd is required"
command -v curl >/dev/null 2>&1 || die "curl is required"
command -v sha256sum >/dev/null 2>&1 || die "sha256sum is required"

case "$(uname -m)" in
  x86_64 | amd64) ARCH=amd64 ;;
  aarch64 | arm64) ARCH=arm64 ;;
  *) die "unsupported CPU architecture: $(uname -m)" ;;
esac

APP_URL="${TOWAMORI_APP_URL:-https://app.towamori.com}"
APP_URL="${APP_URL%/}"

# pair asks the platform for a code, shows it, and waits until someone adds
# the server in the console. It sets TOWAMORI_TOKEN.
pair() {
  host="$(cat /proc/sys/kernel/hostname 2>/dev/null || hostname)"
  resp="$(curl -fsS -X POST --data-urlencode "hostname=$host" "$APP_URL/v1/pair")" \
    || die "could not reach $APP_URL to get a pairing code"
  code="$(printf '%s\n' "$resp" | sed -n 's/^code=//p')"
  secret="$(printf '%s\n' "$resp" | sed -n 's/^secret=//p')"
  url="$(printf '%s\n' "$resp" | sed -n 's/^url=//p')"
  [ -n "$code" ] && [ -n "$secret" ] || die "unexpected answer from $APP_URL"

  printf '\n'
  printf '  ┌──────────────────────────────────────────────┐\n'
  printf '  │  Pairing code:  %-29s│\n' "$code"
  printf '  └──────────────────────────────────────────────┘\n'
  printf '\n'
  printf '  Towamori console → Add server → enter the code,\n'
  printf '  or open: %s\n' "$url"
  printf '\n'
  say "waiting to be added (the code is valid for 15 minutes; Ctrl-C to stop)"

  i=0
  while [ "$i" -lt 450 ]; do
    sleep 2
    i=$((i + 1))
    out="$(curl -sS -o "$TMP/pair" -w '%{http_code}' "$APP_URL/v1/pair/$secret" 2>/dev/null || true)"
    case "$out" in
      200)
        TOWAMORI_TOKEN="$(sed -n 's/^token=//p' "$TMP/pair")"
        rm -f "$TMP/pair"
        [ -n "$TOWAMORI_TOKEN" ] || die "unexpected answer from $APP_URL"
        say "added to your account"
        return 0
        ;;
      404) die "the pairing code expired; run the command again" ;;
    esac
  done
  die "nobody entered the code in time; run the command again"
}

VERSION="${TOWAMORI_VERSION:-latest}"
if [ -n "${TOWAMORI_DOWNLOAD_BASE:-}" ]; then
  # Download from a Towamori platform's /dl/ instead of GitHub releases.
  case "$TOWAMORI_DOWNLOAD_BASE" in
    https://*) BASE="${TOWAMORI_DOWNLOAD_BASE%/}" ;;
    *) die "TOWAMORI_DOWNLOAD_BASE must be an https:// URL" ;;
  esac
elif [ "$VERSION" = "latest" ]; then
  BASE="https://github.com/$REPO/releases/latest/download"
else
  BASE="https://github.com/$REPO/releases/download/v${VERSION#v}"
fi
ARCHIVE="towamori-agent_linux_${ARCH}.tar.gz"

TMP="$(mktemp -d)"
trap 'rm -rf "$TMP"' EXIT INT TERM

say "downloading $ARCHIVE ($VERSION)"
curl -fsSL -o "$TMP/$ARCHIVE" "$BASE/$ARCHIVE"
curl -fsSL -o "$TMP/checksums.txt" "$BASE/checksums.txt"

say "verifying checksum"
(cd "$TMP" && grep " $ARCHIVE\$" checksums.txt | sha256sum -c -) >/dev/null \
  || die "checksum mismatch for $ARCHIVE; not installing"

tar -xzf "$TMP/$ARCHIVE" -C "$TMP" towamori-agent
install -m 0755 "$TMP/towamori-agent" "$BIN"

if [ -z "${TOWAMORI_TOKEN:-}" ] && [ ! -f "$CONF" ]; then
  pair
fi

install -d -m 0755 "$CONF_DIR"
if [ -n "${TOWAMORI_TOKEN:-}" ]; then
  (
    umask 077
    echo "TOWAMORI_TOKEN=$TOWAMORI_TOKEN"
    if [ -n "${TOWAMORI_ENDPOINT:-}" ]; then echo "TOWAMORI_ENDPOINT=$TOWAMORI_ENDPOINT"; fi
  ) >"$CONF"
  chmod 0600 "$CONF"
  say "wrote $CONF"
else
  say "keeping existing $CONF"
fi

cat >"$UNIT" <<'UNIT_EOF'
[Unit]
Description=Towamori host agent
Documentation=https://github.com/A1oneSKy/towamori
Wants=network-online.target
After=network-online.target

[Service]
ExecStart=/usr/local/bin/towamori-agent
EnvironmentFile=-/etc/towamori/agent.env
Restart=always
RestartSec=5
DynamicUser=yes
NoNewPrivileges=yes
CapabilityBoundingSet=
AmbientCapabilities=
ProtectSystem=strict
ProtectHome=read-only
PrivateTmp=yes
PrivateDevices=yes
ProtectKernelTunables=yes
ProtectKernelModules=yes
ProtectKernelLogs=yes
ProtectControlGroups=yes
ProtectClock=yes
RestrictAddressFamilies=AF_INET AF_INET6 AF_UNIX
RestrictNamespaces=yes
RestrictRealtime=yes
RestrictSUIDSGID=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
SystemCallArchitectures=native
SystemCallFilter=@system-service
SystemCallFilter=~@privileged @resources
SystemCallErrorNumber=EPERM
MemoryMax=64M

[Install]
WantedBy=multi-user.target
UNIT_EOF

systemctl daemon-reload
systemctl enable towamori-agent >/dev/null 2>&1
systemctl restart towamori-agent

# "systemctl restart" succeeds as soon as the process has been started, even if
# it exits a moment later. Look again before saying it works.
sleep 3
if ! systemctl is-active --quiet towamori-agent; then
  printf '%s\n' "towamori: the service did not stay up. Last log lines:" >&2
  journalctl -u towamori-agent -n 15 --no-pager >&2 || true
  die "installed $("$BIN" -version) but the service is not running (see above)"
fi

say "installed $("$BIN" -version); service is running"
say "logs:      journalctl -u towamori-agent -f"
say "preview:   $BIN --once"
