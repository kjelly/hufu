#!/usr/bin/env bash
# Minimal Ubuntu/Debian Chrome desktop, shared by noVNC and Chrome Remote Desktop.
set -euo pipefail

usage() {
	cat <<'HELP'
Usage: scripts/init-chrome-desktop.sh <install|start|session|password> [URL]

  install   Install Chrome, Xvfb, Openbox, x11vnc and noVNC (uses sudo).
  start     Share the running CRD desktop via noVNC, or create an Xvfb desktop.
  session   Run Openbox + Chrome on an existing DISPLAY (e.g. CRD).
  password  Create/change the noVNC password interactively.

Environment:
  CHROME_DESKTOP_DIR  State/profile directory (default: ~/.local/share/chrome-desktop)
  CHROME_DISPLAY     Xvfb display number (default: 99)
  CHROME_RESOLUTION  Xvfb screen size (default: 1440x900)
  NOVNC_PORT         HTTP/WebSocket port on 127.0.0.1 (default: 6080)
  NOVNC_WEB_DIR      noVNC assets (default: /usr/share/novnc)
  VNC_PORT           VNC port on 127.0.0.1 (default: 5999)
  CHROME_CDP_PORT     Chrome debugging port on localhost (default: 9222)

Chrome always uses a dedicated persistent profile and enables CDP. Run start or
session as a regular user. Ctrl+C stops owned processes; a shared CRD desktop
keeps running. The profile stays.
HELP
}
fail() { echo "Error: $*" >&2; exit 1; }
need() { command -v "$1" >/dev/null || fail "Missing $1; run '$0 install' first."; }
port() { [[ "$2" =~ ^[0-9]{1,5}$ ]] && ((10#$2 >= 1024 && 10#$2 <= 65535)) || fail "$1 must be 1024..65535"; }

mode=${1:---help}
case "$mode" in -h|--help) usage; exit 0 ;; install|start|session|password) ;; *) usage >&2; exit 2 ;; esac
shift
(($# <= 1)) || fail 'Expected at most one URL'
url=${1:-about:blank}
[[ "$url" == about:blank || "$url" == http://* || "$url" == https://* ]] || fail 'URL must use http:// or https://'
self=$(readlink -f -- "${BASH_SOURCE[0]}")

if [[ "$mode" == install ]]; then
	[[ -f /etc/debian_version ]] || fail 'Installer supports Ubuntu/Debian only'
	arch=$(dpkg --print-architecture)
	case "$arch" in amd64|arm64) ;; *) fail "Chrome installer does not support $arch" ;; esac
	admin=()
	if ((EUID != 0)); then need sudo; admin=(sudo); fi
	"${admin[@]}" apt-get update
	"${admin[@]}" apt-get install -y --no-install-recommends ca-certificates curl \
		xvfb xauth x11-utils openbox dbus-x11 x11vnc novnc websockify \
		fonts-liberation fonts-noto-cjk python3 util-linux
	if ! command -v google-chrome >/dev/null; then
		scratch=$(mktemp -d /tmp/chrome-desktop-install.XXXXXX)
		trap 'rm -rf -- "$scratch"' EXIT
		curl --fail --location --proto '=https' --tlsv1.2 --retry 3 \
			"https://dl.google.com/linux/direct/google-chrome-stable_current_${arch}.deb" \
			-o "$scratch/chrome.deb"
		# apt's unprivileged downloader must be able to read this one package.
		chmod 755 "$scratch"
		chmod 644 "$scratch/chrome.deb"
		"${admin[@]}" apt-get install -y --no-install-recommends "$scratch/chrome.deb"
	fi
	echo "Installed. As a regular user, run: $self start"
	exit 0
fi

((EUID != 0)) || fail 'Run the desktop as a regular user; Chrome sandbox stays enabled'
umask 077
state=${CHROME_DESKTOP_DIR:-$HOME/.local/share/chrome-desktop}
[[ "$state" == /* ]] || fail 'CHROME_DESKTOP_DIR must be an absolute path'
mkdir -p -- "$state"
chmod 700 "$state"
export CHROME_DESKTOP_DIR="$state"
need flock
# A session owns Chrome; noVNC can attach without opening the profile again.
shared=false
exec 9>"$state/session.lock"
if [[ "$mode" == start ]]; then
	exec 8>"$state/novnc.lock"
	flock -n 8 || fail 'noVNC is already running for this desktop'
fi
if [[ "$mode" != password ]] && ! flock -n 9; then
	[[ "$mode" == start && -s "$state/session-display" ]] || fail 'Desktop is starting or already running; retry start when CDP is ready'
	mapfile -t connection <"$state/session-display"
	[[ ${#connection[@]} == 3 && "${connection[0]}" =~ ^:[0-9]+(\.[0-9]+)?$ ]] || fail 'Invalid saved desktop connection'
	export DISPLAY="${connection[0]}" XAUTHORITY="${connection[1]}"
	CHROME_CDP_PORT=${connection[2]}
	shared=true
fi
if [[ "$mode" == password || ( "$mode" == start && ! -s "$state/vnc.pass" ) ]]; then
	need x11vnc
	[[ -t 0 ]] || fail "Create the VNC password first: $self password"
	x11vnc -storepasswd "$state/vnc.pass"
	chmod 600 "$state/vnc.pass"
	[[ "$mode" != password ]] || exit 0
fi

cdp=${CHROME_CDP_PORT:-9222}
port CHROME_CDP_PORT "$cdp"
need python3
# Fail before starting Chrome if another process already owns the requested port.
if [[ "$shared" == false ]]; then
python3 - "$cdp" <<'PY'
import socket, sys
with socket.socket() as sock:
    try:
        sock.bind(('127.0.0.1', int(sys.argv[1])))
    except OSError as exc:
        sys.exit(f'CDP port unavailable: {exc}')
PY
fi
need setsid
pids=()
session_published=false
cleanup() {
	trap - EXIT INT TERM
	for pid in "${pids[@]}"; do
		kill -TERM -- "-$pid" 2>/dev/null || kill -TERM "$pid" 2>/dev/null || true
	done
	for pid in "${pids[@]}"; do wait "$pid" 2>/dev/null || true; done
	if [[ "$shared" == false && "$session_published" == true ]]; then rm -f -- "$state/session-display"; fi
	[[ -z "${runtime:-}" ]] || rm -rf -- "$runtime"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM
runtime=$(mktemp -d)
spawn() { setsid "$@" & pids+=("$!"); }

if [[ "$mode" == start ]]; then
	for tool in xdpyinfo x11vnc websockify; do need "$tool"; done
	web=${NOVNC_WEB_DIR:-/usr/share/novnc}
	[[ -f "$web/vnc.html" ]] || fail "Missing $web/vnc.html"
	if [[ "$shared" == false ]]; then
		need Xvfb
		need xauth
		display=${CHROME_DISPLAY:-99}
		[[ "$display" =~ ^[0-9]{1,4}$ ]] || fail 'CHROME_DISPLAY must be a display number'
		export DISPLAY=":$display"
		[[ ! -e "/tmp/.X${display}-lock" && ! -S "/tmp/.X11-unix/X$display" ]] || fail "Display $DISPLAY is occupied; choose CHROME_DISPLAY"
		resolution=${CHROME_RESOLUTION:-1440x900}
		[[ "$resolution" =~ ^[1-9][0-9]{2,3}x[1-9][0-9]{2,3}$ ]] || fail 'Invalid CHROME_RESOLUTION (example: 1440x900)'
		export XAUTHORITY="$runtime/Xauthority"
		touch "$XAUTHORITY"
		cookie=$(python3 -c 'import secrets; print(secrets.token_hex(16))')
		xauth add "$DISPLAY" MIT-MAGIC-COOKIE-1 "$cookie"
		spawn Xvfb "$DISPLAY" -screen 0 "${resolution}x24" -nolisten tcp -auth "$XAUTHORITY"
		ready=false
		for ((i=0; i<100; i++)); do
			if xdpyinfo >/dev/null 2>&1; then ready=true; break; fi
			kill -0 "${pids[0]}" 2>/dev/null || fail 'Xvfb exited before becoming ready'
			sleep 0.1
		done
		[[ "$ready" == true ]] || fail 'Xvfb readiness timed out'
	fi
	novnc=${NOVNC_PORT:-6080}
	vnc=${VNC_PORT:-5999}
	port NOVNC_PORT "$novnc"
	port VNC_PORT "$vnc"
	[[ "$novnc" != "$vnc" && "$novnc" != "$cdp" && "$vnc" != "$cdp" ]] || fail 'Ports must be distinct'
	xdpyinfo >/dev/null 2>&1 || fail "Cannot access shared display $DISPLAY"
	spawn x11vnc -display "$DISPLAY" -auth "$XAUTHORITY" -localhost -listen 127.0.0.1 -no6 \
		-rfbport "$vnc" -rfbauth "$state/vnc.pass" -forever -shared -noxdamage
	spawn websockify --web="$web" "127.0.0.1:$novnc" "127.0.0.1:$vnc"
else
	[[ -n "${DISPLAY:-}" ]] || fail 'session needs an existing DISPLAY (provided by CRD)'
fi

need curl
if [[ "$shared" == false ]]; then
	for tool in openbox dbus-run-session google-chrome xdpyinfo; do need "$tool"; done
	xdpyinfo >/dev/null 2>&1 || fail "Cannot access display $DISPLAY"
	export XAUTHORITY=${XAUTHORITY:-$HOME/.Xauthority}
	[[ "$DISPLAY" =~ ^:[0-9]+(\.[0-9]+)?$ ]] || fail 'Expected a local X display'
	# Publish only connection metadata. noVNC never launches a second Chrome.
	printf '%s\n' "$DISPLAY" "$XAUTHORITY" "$cdp" >"$runtime/session-display"
	mv -f -- "$runtime/session-display" "$state/session-display"
	session_published=true
	# A private Openbox configuration avoids desktop menus, terminals and autostarts.
	cat >"$runtime/openbox.xml" <<-'XML'
	<?xml version="1.0" encoding="UTF-8"?>
	<openbox_config xmlns="http://openbox.org/3.4/rc">
	  <desktops><number>1</number></desktops>
	  <applications><application class="*"><maximized>yes</maximized></application></applications>
	  <keyboard/>
	  <mouse/>
	</openbox_config>
	XML
	spawn openbox --config-file "$runtime/openbox.xml"
	spawn dbus-run-session -- google-chrome --ozone-platform=x11 \
		--user-data-dir="$state/profile" --remote-debugging-address=127.0.0.1 \
		--remote-debugging-port="$cdp" --no-first-run --no-default-browser-check \
		--start-maximized "$url"
fi
ready=false
for ((i=0; i<150; i++)); do
	for pid in "${pids[@]}"; do kill -0 "$pid" 2>/dev/null || fail 'A desktop component exited during startup'; done
	if curl --noproxy '*' --fail --silent --max-time 1 "http://127.0.0.1:$cdp/json/version" \
		| python3 -c 'import json,sys; assert json.load(sys.stdin)["webSocketDebuggerUrl"]' 2>/dev/null; then
		ready=true
		break
	fi
	sleep 0.2
done
[[ "$ready" == true ]] || fail "Chrome CDP did not become ready on 127.0.0.1:$cdp"
echo "Chrome CDP ready: http://127.0.0.1:$cdp/json/version"
if [[ "$mode" == start ]]; then
	echo "noVNC: http://127.0.0.1:$novnc/vnc.html?autoconnect=true&resize=scale"
	echo "From your computer: ssh -N -L $novnc:127.0.0.1:$novnc USER@SERVER"
fi
if [[ "$shared" == true ]]; then
	echo "Sharing existing desktop $DISPLAY; URL argument is ignored. Ctrl+C stops only noVNC."
else
	echo 'Desktop running. Close Chrome or press Ctrl+C to stop.'
fi
# Any failed component or normal Chrome exit ends the whole private desktop.
wait -n "${pids[@]}"
