# poligon

Self-hosted phone farm for testing built Flutter apps (`.apk` / `.aab` / `.ipa`)
on real wired devices. poligon does **not** build apps — it takes finished
artifacts, installs them on reserved devices, and lets you test.

Host: Mac mini (`ssh dev-mac@172.24.18.20`). Single Go binary + SQLite, no Redis/Postgres.

## Status

Done:
- device inventory from `config/devices.yaml`, health poll (adb / libimobiledevice), flap → `degraded`
- hardware specs per device (model, SoC, RAM, screen, battery, OS)
- auth: open self-service signup (email + password), server-side sessions, no admin role; personal API tokens for scripts / CI
- reservations: one holder per device, heartbeat lease, idle + hard-cap auto-release; multi-device batches
- manual install via dashboard / API: apk direct, aab via bundletool, ipa re-signed with farm profiles then `ios-deploy`; mixed Android+iOS batches take one artifact per platform
- live screens: ws-scrcpy for Android, WebDriverAgent for iOS, one grid for a batch
- per-device diagnostics: screenshot + logcat from the grid (`internal/capture`)
- **test runs** (`internal/runner`): `install_smoke` (install → launch → assert alive + no crash), `maestro` (run a `.yaml` flow, collect report + recording), `command` (generic escape hatch — appium etc.), `integration_test` (Android: install the app + its androidTest apk, run the instrumentation). Per-device artifacts under `<storage_dir>/runs/<id>/<device>/`, results at `/runs.html` (status/type filters, paged)
- **live debugging from VS Code** (`internal/adbtunnel`, Android): exposes the host's adb server on the network automatically while any Android device is reserved, so `flutter run -d <serial>` / VS Code attaches to a farm device directly — hot reload, breakpoints, not just install-and-collect. One-time setup, then reserve+open VS Code is all it takes. See "VS Code / live debugging" below.

- **coding agents over MCP** (`/mcp`): Claude Code / Codex drive the farm themselves — reserve a phone, install a build, screenshot + UI tree, tap / swipe / type / keys / deep links, logs, adb shell, Maestro runs. See "Agents (MCP)" below.

Next:
- iOS `integration_test` — needs a `.xctestrun` bundle + `xcodebuild test-without-building`, and Xcode on the host

### Agents (MCP)

`POST /mcp` is an MCP server (streamable HTTP, stateless) authenticated with a
personal API token — `Authorization: Bearer plgn_…`, never the session cookie.
Tokens: dashboard → **Агенты** (create / revoke, shows ready-made commands) or
`poligon token create` on the host.

```sh
# Claude Code
claude mcp add --transport http --scope user poligon https://farm/mcp \
  --header "Authorization: Bearer plgn_…"

# Codex — ~/.codex/config.toml
[mcp_servers.poligon]
url = "https://farm/mcp"
bearer_token_env_var = "POLIGON_TOKEN"
tool_timeout_sec = 900   # installs/runs take minutes; Codex defaults to 60s
```

Tools: `list_devices`, `reserve_device`, `release_device`, `install_app`,
`list_apps`, `launch_app`, `stop_app`, `clear_app_data`, `uninstall_app`,
`screenshot`, `ui_tree`, `tap`, `swipe`, `type_text`, `press_key`, `open_url`,
`wait_for`, `get_logs`, `shell`, `start_run`, `get_run`, `get_run_artifact`,
`cancel_run`, `list_runs`.

- **Coordinates.** Screenshots come back scaled to ≤1280px on the long edge
  (iOS: to points), and every x/y in `tap`/`swipe`/`ui_tree` is in that image
  space — the server maps it to device pixels / points. Prefer
  `tap {text|id|index}` from `ui_tree` over raw coordinates.
- **Leases.** A device tool renews the reservation (≤ every 30s), so an agent
  holds a phone while it works and loses it after `idle_timeout` of silence.
- **Local builds.** Tool calls are JSON, so a file on the agent's machine goes
  up first: `curl -F file=@app.apk -H "Authorization: Bearer $POLIGON_TOKEN"
  https://farm/api/uploads` → `upload_id` (owner-only, kept 48h), then
  `install_app {upload_id}` or `start_run {app:[upload_id]}`. URLs work too.
- **Limits.** Android `type_text` is ASCII-only (`input text`); use a Maestro
  flow's `inputText` for Cyrillic. `shell`, `list_apps`, `clear_app_data`,
  `uninstall_app` are Android only. iOS input/UI tree need the device's
  WebDriverAgent screen up.

### Test-run API

```sh
# mint a token on the host
poligon token create you@company.com ci

# smoke-test a build on 2 free Android devices
curl -sX POST https://farm/api/runs \
  -H "Authorization: Bearer plgn_…" \
  -F type=install_smoke -F platform=android -F count=2 \
  -F artifact=@app-release.apk

# maestro flow, build pulled from CI artifact storage, callback on finish
curl -sX POST https://farm/api/runs \
  -H "Authorization: Bearer plgn_…" \
  --data-urlencode type=maestro --data-urlencode platform=android --data-urlencode count=1 \
  --data-urlencode artifact_url=https://ci/…/app.apk \
  --data-urlencode flow_url=https://ci/…/flow.yaml \
  --data-urlencode callback_url=https://ci/…/hook

# generic: run any command, ANDROID_SERIAL / DEVICE_UDID / POLIGON_RUN_DIR in env
curl -sX POST https://farm/api/runs -H "Authorization: Bearer plgn_…" \
  --data-urlencode type=command --data-urlencode device=pixel6-01 \
  --data-urlencode 'command=appium ... || exit 1'

# a whole Maestro workspace: zip the .maestro/ folder (config.yaml, flows/,
# subflows/) so runFlow: ../subflows/… resolves. flow_path picks one flow,
# otherwise maestro runs what config.yaml lists; env= becomes -e KEY=VALUE.
# Every flow also gets POLIGON_DEVICE_ID / POLIGON_PLATFORM.
zip -qr maestro.zip .maestro
curl -sX POST https://farm/api/runs -H "Authorization: Bearer plgn_…" \
  -F type=maestro -F platform=android -F count=1 \
  -F artifact=@app-release.apk -F flow=@maestro.zip \
  -F flow_path=flows/offline_full_flow.yaml -F include_tags=offline \
  -F env=PHONE=9525115368 -F env=CODE=0000

# Flutter integration_test (Android): the app apk + its separately built
# androidTest apk (flutter build apk --debug; cd android && ./gradlew
# app:assembleDebugAndroidTest), am instrument reads the runner class off the
# test apk's own manifest
curl -sX POST https://farm/api/runs -H "Authorization: Bearer plgn_…" \
  -F type=integration_test -F platform=android -F count=1 \
  -F artifact=@app-debug.apk -F test_artifact=@app-debug-androidTest.apk

# poll: GET /api/runs/{id} → {status: queued|running|passed|failed|error|canceled, devices:[…]}
```

Run types: `install_smoke`, `maestro`, `command`, `integration_test`. Device selection is
`device=<id>` (repeatable) **or** `platform=`/`count=`/`tag=`. Per-device cap
`timeout_seconds` (default 1200). Artifacts:
`GET /api/runs/{id}/artifacts/{device}/{path}`. Status SVG for a CI dashboard:
`GET /runs/{id}/badge.svg` (no auth). Use `--data-urlencode` for urlencoded
bodies (a raw `;` or space is rejected).

**LAN URLs need macOS "Local Network" access.** On macOS 15+, a launchd
service may not open connections to the local network until it is allowed in
System Settings → Privacy & Security → Local Network. Until then every
`artifact_url` / `flow_url` / `callback_url` pointing at a LAN host fails with
`connect: no route to host`, while the same `curl` over ssh works (ssh sessions
are exempt). Public URLs are unaffected. Granting it needs someone at the Mac's
screen; recheck after a rebuild of the binary.

### VS Code / live debugging (Android)

Everything above installs a pre-built artifact and collects results —
useful for CI, not for iterating on a debug build. Reserving an Android
device now opens **your own** adb tunnel to the farm by itself — no button,
no per-session commands — so your machine's `adb`/`flutter run`/VS Code
attaches to the farm device directly: hot reload, breakpoints, everything
works as if the phone were plugged into your own laptop. The tunnel is
filtered to only the Android device(s) *you* currently hold — someone else's
reserved device never shows up in your `adb devices`, even though the raw adb
protocol itself has no such concept (see "Trust model" below).

Each user gets one fixed port for life (assigned from `adb_tunnel_port_range_start`
`-adb_tunnel_port_range_end` in config, default `5040-5090`), so the setup is
a **one-time** thing per person, not per session or per device. Find yours at
`GET /api/debug-tunnel/info` (also shown by the "VS Code debug" button in the
screen grid):

```sh
# add to ~/.zshrc, or to a dedicated VS Code Profile's
# terminal.integrated.env.osx so your default profile keeps local emulators
export ANDROID_ADB_SERVER_ADDRESS=<farm-host>
export ANDROID_ADB_SERVER_PORT=<your personal port, from /api/debug-tunnel/info>
```

Restart VS Code once (macOS re-sources the shell profile on app launch, not
just in terminals), then: reserve a device in the dashboard, open VS Code,
pick it from **Flutter: Select Device** — only your own reserved devices show
up, like a local simulator.

The tunnel opens on reserve, self-heals on every reservation heartbeat, and
its allow-list empties out (not the port itself) once you hold no Android
device — a stale reservation drops out within a minute even without an
explicit release, via the same background sweep that expires reservations.

**Trust model:** adb's wire protocol itself has no per-device access control,
so poligon filters `adb devices`/`adb -s <serial> ...` per user at the proxy
layer (see `internal/adbfilter`) instead of relying on adb for isolation.
This matches the project's existing posture (open registration, "the network
is the perimeter") for who can reach the tunnel at all, but *which* devices
each tunnel shows is now scoped to that user's own reservations.
Set `adb_tunnel_port: 0` in config to disable the feature outright.
iOS isn't supported yet — there's no equivalent of "adb server over TCP"; a
USB-over-network proxy (`usbfluxd` or similar) would be needed instead.

## Run

```sh
cp config/devices.example.yaml config/devices.yaml   # edit: real serials / udids
go build -o poligon ./cmd/poligon
POLIGON_DEV_USER=me@company.com ./poligon serve --dev # dev: bypass auth
# open http://localhost:8080 → "Create account"
```

## Authentication

**Open registration.** Anyone who can reach the dashboard clicks *Create account*,
picks an **email + password**, and is in. There is no admin role and no invite
step — the network (LAN / VPN) is the perimeter. Passwords are bcrypt hashed.

- Forgot your password? Someone with shell access to the host runs
  `poligon user reset-password <email>` and hands you the one-time link it prints.
- `poligon user list | disable <email> | enable <email> | reset-password <email> | add <email>`
  — host-side moderation. `disable` and `reset-password` kill the user's active
  sessions immediately. A password change from the dashboard drops the user's
  other sessions too.
- Sessions live server-side in SQLite: `HttpOnly` cookie, 14-day cap with a
  24h sliding idle window, revoked on logout. CSRF is enforced (double-submit)
  on cookie-authenticated writes.
- Login and signup are rate-limited (5 failures per email/IP → 15-min lock).
- Personal API tokens (`plgn_…`) for scripts, CI and agents: dashboard →
  **Агенты**, or `poligon token create|list|revoke` on the host. Stored hashed,
  shown once. Legacy per-user `Authorization: Bearer <token>` still resolves.

Put poligon behind TLS for anything past the trusted LAN — either set `tls:` in
the config (direct HTTPS) or front it with `tailscale serve` / Caddy. Secure
cookies switch on automatically when the request arrives over HTTPS.

### Environment

| var | meaning |
|---|---|
| `POLIGON_CONFIG` | config path (default `config/devices.yaml`) |
| `POLIGON_DEV_USER` | bypass auth as this user; honored **only** on a loopback `listen` or with `serve --dev` |
| `POLIGON_BUNDLETOOL` | path to `bundletool.jar` for `.aab` |
| `POLIGON_SIGNING_IDENTITY` | codesign identity, e.g. `Apple Distribution: Company (TEAMID)` |
| `POLIGON_PROFILE_DIR` | farm `.mobileprovision` dir (default `config/profiles`) |

## iOS re-signing

An `.ipa` installs only if signed with a profile covering the target device.
poligon re-signs incoming builds with the farm's **ad-hoc** profiles (one
`<bundle-id>.mobileprovision` per app + extension in `config/profiles/`), using
the company signing identity. Same Team ID ⇒ entitlements (push, App Groups,
deeplinks) survive. Register new device UDIDs with `scripts/register-device.sh`.

## Host setup

`scripts/bootstrap-mac.sh` — installs adb, libimobiledevice, ios-deploy, go,
node, bundletool, maestro, fastlane, appium.

`scripts/install-live-sidecar.sh` — builds the ws-scrcpy sidecar.

`scripts/install-all.sh` — installs the three services (below) + log rotation.

`scripts/host-setup.sh` — the power-loss recovery settings (needs a reboot).

## Operations

Four services keep the farm running. All are `KeepAlive` and start at boot;
each (re)start is self-cleaning.

| service | what | logs |
|---|---|---|
| `com.pancir.poligon` (LaunchAgent, gui) | the Go binary — API, dashboard, device poll, iOS WebDriverAgent | `~/poligon/poligon.{out,err}.log` |
| `com.pancir.adb` (LaunchAgent, gui) | the farm's **only** adb server: `adb server nodaemon` via `deploy/adb-server-run.sh`, which kills every other adb first | `~/poligon/adb.log` |
| `com.pancir.poligon-live` (LaunchDaemon) | ws-scrcpy sidecar for Android screens; runs `deploy/ws-scrcpy-run.sh` which frees `:8000`, waits for the adb server, clears stale on-device state on every start | `~/poligon-sidecar/ws-scrcpy.{out,err}.log` |
| `com.pancir.go-ios-tunnel` (LaunchDaemon, root) | go-ios tunnel — required for iOS 17+ | `/var/log/com.pancir.go-ios-tunnel.{out,err}.log` |

- **Deploy:** `scripts/update.sh` — pull, atomic build (a broken build never
  replaces the running binary), restart poligon + the two daemons (adb stays
  up), wait for `/healthz`. New host or new service: `scripts/install-all.sh`.
- **adb ownership.** adb normally starts its server on demand from any client
  and daemonizes it — owned by no service, never stopped by a restart. A wedged
  one then hangs `adb kill-server`, a second server starts beside it, and the
  old one keeps the phones' USB: `adb devices` is empty with phones plugged in.
  So launchd owns the server (`com.pancir.adb`), poligon's adb client refuses to
  run while it is down instead of spawning one, and ws-scrcpy only waits for
  it. poligon's watchdog restarts `com.pancir.adb` when it stops answering or a
  second server appears (≤3× per 30 min), and restarts ws-scrcpy whenever the
  server's pid changes — via a NOPASSWD sudoers rule for exactly those
  `launchctl kickstart` commands (`/etc/sudoers.d/pancir-poligon`).
- **Clean stop.** Maestro and `command` runs live in their own process group:
  cancel, timeout or poligon shutdown sends the whole tree SIGTERM, then
  SIGKILL after 5s, and poligon waits for that before exiting — no orphaned
  JVMs holding a phone.
- **Diagnose:** `scripts/farm-doctor.sh` (report) / `--fix` (clean up + restart).
- **Health:** `GET /healthz` (unauthenticated) — poligon, ws-scrcpy, tunnel, adb
  device count, iOS screens ready/total. The dashboard shows it as a dot in the
  top bar.
- **On poligon restart:** iOS screens are torn down and rebuilt (≈1 min each) —
  no orphaned `ios runwda` / `ios forward` processes accumulate. A background
  watchdog also auto-restarts an iOS screen whose WebDriverAgent stops answering.
- **iOS screen cost:** poligon reads each device's WebDriverAgent mjpeg stream
  **once** and fans it out to every viewer, so ten open tiles are still one
  stream on the phone. How much the phone sends is set in `devices.yaml` under
  `ios_wda:` — `mjpeg_framerate` (12), `mjpeg_quality` (35) and `mjpeg_scale`
  (50, percent). Raise them for a sharper wall, lower them if the phones or the
  host struggle. Run screenshots ignore these and come from WDA's full-quality
  `/screenshot`.
- **Power loss:** with `scripts/host-setup.sh` applied (FileVault off, auto-login,
  `pmset autorestart 1`) the mac powers on, logs in, and all services come up
  with no human at the keyboard.

## Layout

```
cmd/poligon        entrypoint + CLI
internal/config    devices.yaml loader
internal/store     sqlite (schema.sql embedded)
internal/model     domain types
internal/adb       adb wrapper (list, specs, install)
internal/ios       libimobiledevice + ios-deploy wrapper
internal/devices   poll loop, flap detection, specs refresh
internal/reserve   booking, leases, auto-release
internal/auth      users + bearer tokens
internal/uitree    uiautomator / WDA source XML → tappable element list (MCP)
internal/install   apk / aab / ipa(re-sign) install pipeline
internal/capture   screenshot / logs / shell / files / apps / recording off a device
internal/runner    automated test runs (install_smoke, maestro, command, integration_test)
internal/api       JSON API + dashboard
internal/webui     embedded dashboard assets
```
