#!/usr/bin/env bash
# Set the video settings the ws-scrcpy sidecar asks each Android device for, and
# rebuild it. Run on the farm host: scripts/tune-live-sidecar.sh
#
# Why this exists: ws-scrcpy's MSE player — the one the dashboard uses — ships
# defaults meant for looking at one phone full screen: 7 Mbit/s at 60 fps. The
# wall shows several phones at ~300px each, so that is several times the encode
# work on the phone, the USB traffic and the browser-side H.264 decoding that
# anyone actually sees. These numbers are a wall, not a cinema.
#
# Override per run, e.g.:
#   BITRATE=4000000 MAX_FPS=30 scripts/tune-live-sidecar.sh
set -euo pipefail
export PATH="/usr/local/bin:$HOME/opt/nodejs/bin:/opt/homebrew/bin:$PATH"

WS_DIR="${WS_SCRCPY_DIR:-$HOME/poligon-sidecar/ws-scrcpy}"
BITRATE="${BITRATE:-2000000}"      # bits/s per device
MAX_FPS="${MAX_FPS:-24}"
IFRAME_INTERVAL="${IFRAME_INTERVAL:-5}"   # seconds between keyframes
BOUNDS="${BOUNDS:-720}"            # longest edge the device encodes to

[ -d "$WS_DIR" ] || { echo "ws-scrcpy not found at $WS_DIR — run scripts/install-live-sidecar.sh first" >&2; exit 1; }

echo "==> patching player defaults: ${BITRATE}bps, ${MAX_FPS}fps, i-frame ${IFRAME_INTERVAL}s, bounds ${BOUNDS}"
python3 - "$WS_DIR" "$BITRATE" "$MAX_FPS" "$IFRAME_INTERVAL" "$BOUNDS" <<'PY'
import glob, os, re, sys

ws, bitrate, fps, iframe, bounds = sys.argv[1:6]
changed = []
for path in sorted(glob.glob(os.path.join(ws, "src/app/player/*.ts"))):
    src = open(path).read()
    if "preferredVideoSettings" not in src:
        continue
    out = src

    # rewrite the fields inside every `new VideoSettings({ ... })` literal that
    # belongs to a preferredVideoSettings declaration
    def patch(block):
        block = re.sub(r"bitrate:\s*\d+", "bitrate: " + bitrate, block)
        block = re.sub(r"maxFps:\s*\d+", "maxFps: " + fps, block)
        block = re.sub(r"iFrameInterval:\s*\d+", "iFrameInterval: " + iframe, block)
        block = re.sub(r"bounds:\s*new Size\(\d+,\s*\d+\)",
                       "bounds: new Size(%s, %s)" % (bounds, bounds), block)
        return block

    for m in re.finditer(r"preferredVideoSettings[^=]*=\s*new VideoSettings\(\{.*?\}\)", src, re.S):
        out = out.replace(m.group(0), patch(m.group(0)))

    if out != src:
        open(path, "w").write(out)
        changed.append(os.path.basename(path))

print("    patched:", ", ".join(changed) if changed else "nothing (already at these values)")
PY

echo "==> restoring the MSE player's own buffering"
python3 - "$WS_DIR" <<'PY'
# An earlier version of this script shrank the MSE player's buffer to ~150 ms
# to cut latency. In practice the player then kept jumping to the live edge and
# the picture stuttered far worse than the lag it removed, so undo that patch
# on hosts that still carry it.
import os, sys
path = os.path.join(sys.argv[1], "src/app/player/MsePlayer.ts")
src = open(path).read()
out = src.replace(
    "private MAX_BUFFER = this.isSafari ? 0.5 : 0.15;",
    "private MAX_BUFFER = this.isSafari ? 2 : this.isChrome && this.isMac ? 0.9 : 0.2;",
)
out = out.replace("if (end - currentTime > this.MAX_BUFFER) {", "if ((end | 0) - currentTime > this.MAX_BUFFER) {")
out = "".join(l for l in out.splitlines(True) if "poligon: drift back" not in l)
if out != src:
    open(path, "w").write(out)
    print("    restored MsePlayer.ts")
else:
    print("    MsePlayer.ts already stock")
PY

echo "==> rebuilding ws-scrcpy (takes a minute)"
cd "$WS_DIR"
npm run dist >/dev/null

echo "==> restarting the sidecar (live Android screens blink)"
# only the server half of ws-scrcpy needs this; the players live in
# dist/public/bundle.js, which browsers refetch on their own

# Restarting a LaunchDaemon needs root. Over ssh without a tty, a cached sudo
# ticket does not always carry into this script, and the old code printed
# "not loaded" for that case too — so a deploy looked like it had restarted the
# sidecar when it had not. Tell the truth, and say exactly what to run.
restart_daemon() {
  local label="$1"
  if ! sudo -n launchctl print "system/$label" >/dev/null 2>&1; then
    if ! launchctl print "system/$label" >/dev/null 2>&1; then
      echo "   $label is not loaded — install it with scripts/install-all.sh"
      return 0
    fi
  fi
  if sudo -n launchctl kickstart -k "system/$label" >/dev/null 2>&1; then
    echo "   restarted $label"
    return 0
  fi
  echo "   could NOT restart $label (needs sudo). Run this on the host:"
  echo "     sudo launchctl kickstart -k system/$label"
  return 0
}
restart_daemon com.pancir.poligon-live

echo "==> done"
