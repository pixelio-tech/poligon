-- poligon schema. Applied idempotently on startup.

CREATE TABLE IF NOT EXISTS devices (
    id            TEXT PRIMARY KEY,
    platform      TEXT NOT NULL,
    serial        TEXT NOT NULL DEFAULT '',
    udid          TEXT NOT NULL DEFAULT '',
    tags          TEXT NOT NULL DEFAULT '[]',   -- json array
    status        TEXT NOT NULL DEFAULT 'offline',
    source        TEXT NOT NULL DEFAULT 'config', -- config | auto
    specs         TEXT NOT NULL DEFAULT '{}',   -- json Specs
    last_seen     TIMESTAMP,
    adopted       INTEGER NOT NULL DEFAULT 0    -- 1 = pool member, 0 = candidate
);

-- persisted iOS live-screen endpoints (WebDriverAgent + mjpeg, forwarded to the
-- host by iproxy). Written when a device is adopted; reloaded on startup so
-- screens survive a restart. pids let poligon check/respawn the processes.
CREATE TABLE IF NOT EXISTS ios_screen (
    device_id     TEXT PRIMARY KEY REFERENCES devices(id),
    wda           TEXT NOT NULL DEFAULT '',   -- host:port of WDA http api
    mjpeg         TEXT NOT NULL DEFAULT '',   -- host:port of WDA mjpeg server
    wda_run_pid   INTEGER NOT NULL DEFAULT 0, -- xcodebuild test-without-building
    wda_pid       INTEGER NOT NULL DEFAULT 0, -- iproxy wda
    mjpeg_pid     INTEGER NOT NULL DEFAULT 0  -- iproxy mjpeg
);

-- Farm accounts. Registration is open (anyone who can reach poligon signs up
-- with an email + password); every account is equal, there is no admin role.
CREATE TABLE IF NOT EXISTS users (
    name          TEXT PRIMARY KEY,          -- login id (email address)
    token_hash    TEXT NOT NULL DEFAULT '',  -- legacy bearer token; unused by new logins
    disabled      INTEGER NOT NULL DEFAULT 0,
    pass_hash     TEXT NOT NULL DEFAULT '',  -- bcrypt
    pass_set      INTEGER NOT NULL DEFAULT 0,-- 0 = account created, password not set yet
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- browser login sessions. token_hash = sha256(random 32 bytes); the raw token
-- is only ever in the client cookie. Looked up by the unique index, not scanned.
CREATE TABLE IF NOT EXISTS sessions (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    user_name   TEXT NOT NULL REFERENCES users(name),
    token_hash  TEXT NOT NULL,
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_seen   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at  TIMESTAMP NOT NULL,
    ip          TEXT NOT NULL DEFAULT '',
    user_agent  TEXT NOT NULL DEFAULT '',
    revoked     INTEGER NOT NULL DEFAULT 0
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_sessions_token ON sessions(token_hash);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_name);

-- one-time "set your password" links handed out by "poligon user add".
CREATE TABLE IF NOT EXISTS enroll_tokens (
    token_hash  TEXT PRIMARY KEY,
    user_name   TEXT NOT NULL REFERENCES users(name),
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at  TIMESTAMP NOT NULL,
    used_at     TIMESTAMP
);

CREATE TABLE IF NOT EXISTS reservations (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id   TEXT NOT NULL REFERENCES devices(id),
    user        TEXT NOT NULL REFERENCES users(name),
    batch       TEXT NOT NULL DEFAULT '',   -- groups reservations taken together
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at  TIMESTAMP NOT NULL,
    renewed_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    released    INTEGER NOT NULL DEFAULT 0
);

-- at most one active reservation per device
CREATE UNIQUE INDEX IF NOT EXISTS idx_res_active_device
    ON reservations(device_id) WHERE released = 0;

CREATE TABLE IF NOT EXISTS installs (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    device_id   TEXT NOT NULL REFERENCES devices(id),
    user        TEXT NOT NULL,
    artifact    TEXT NOT NULL,   -- original filename
    package     TEXT NOT NULL DEFAULT '',
    version     TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,   -- ok | failed
    detail      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- Phase 3: automated test runs on a set of reserved devices.
CREATE TABLE IF NOT EXISTS runs (
    id          TEXT PRIMARY KEY,           -- short random id
    user        TEXT NOT NULL,
    type        TEXT NOT NULL,              -- install_smoke | maestro | ...
    status      TEXT NOT NULL,              -- queued | running | passed | failed | error | canceled
    trigger     TEXT NOT NULL DEFAULT 'ui', -- ui | api
    batch       TEXT NOT NULL DEFAULT '',   -- reservation batch held for the run
    spec        TEXT NOT NULL DEFAULT '{}', -- json: artifact names, options
    detail      TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    started_at  TIMESTAMP,
    finished_at TIMESTAMP
);

CREATE TABLE IF NOT EXISTS run_devices (
    run_id      TEXT NOT NULL REFERENCES runs(id) ON DELETE CASCADE,
    device_id   TEXT NOT NULL,
    platform    TEXT NOT NULL DEFAULT '',
    status      TEXT NOT NULL,              -- pending | running | passed | failed | error | skipped
    detail      TEXT NOT NULL DEFAULT '',
    package     TEXT NOT NULL DEFAULT '',
    artifacts   TEXT NOT NULL DEFAULT '[]', -- json array of file names under runs/<id>/<device_id>/
    started_at  TIMESTAMP,
    finished_at TIMESTAMP,
    PRIMARY KEY (run_id, device_id)
);
CREATE INDEX IF NOT EXISTS idx_run_devices_run ON run_devices(run_id);

-- personal API tokens for scripted / CI callers. token_hash = sha256(raw); the
-- raw token is shown once at creation and never stored.
CREATE TABLE IF NOT EXISTS api_tokens (
    token_hash   TEXT PRIMARY KEY,
    user_name    TEXT NOT NULL REFERENCES users(name),
    name         TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_api_tokens_user ON api_tokens(user_name);

-- one adb-tunnel port per user, assigned once from a fixed range and kept
-- forever so the ANDROID_ADB_SERVER_PORT a developer puts in their shell
-- profile never changes across restarts.
CREATE TABLE IF NOT EXISTS adb_tunnel_ports (
    user_name  TEXT PRIMARY KEY REFERENCES users(name),
    port       INTEGER NOT NULL UNIQUE,
    created_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- every app build that has been installed onto a farm phone, from any path
-- (dashboard, batch, MCP agent, test run). One row per distinct file (sha256);
-- the file itself lives at <StorageDir>/<path>. uploaded_* describe the first
-- time it arrived; build_installs records every install since.
CREATE TABLE IF NOT EXISTS builds (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    sha256       TEXT NOT NULL UNIQUE,
    filename     TEXT NOT NULL,
    platform     TEXT NOT NULL,              -- android | ios
    format       TEXT NOT NULL,              -- apk | aab | apks | ipa
    size         INTEGER NOT NULL,
    package      TEXT NOT NULL DEFAULT '',   -- android package / iOS bundle id
    app_name     TEXT NOT NULL DEFAULT '',
    version      TEXT NOT NULL DEFAULT '',   -- versionName / CFBundleShortVersionString
    build_code   TEXT NOT NULL DEFAULT '',   -- versionCode / CFBundleVersion
    min_os       TEXT NOT NULL DEFAULT '',   -- minSdkVersion / MinimumOSVersion
    path         TEXT NOT NULL,              -- relative to the storage dir
    uploaded_by  TEXT NOT NULL,
    uploaded_at  TIMESTAMP NOT NULL,
    via          TEXT NOT NULL DEFAULT '',   -- web | batch | mcp | run
    source_url   TEXT NOT NULL DEFAULT '',   -- when fetched from a URL
    note         TEXT NOT NULL DEFAULT '',
    deleted_at   TIMESTAMP
);
CREATE INDEX IF NOT EXISTS idx_builds_uploaded ON builds(uploaded_at);

-- no foreign key to devices: history outlives a device being removed
CREATE TABLE IF NOT EXISTS build_installs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    build_id     INTEGER NOT NULL REFERENCES builds(id) ON DELETE CASCADE,
    device_id    TEXT NOT NULL,
    device_model TEXT NOT NULL DEFAULT '',
    os_version   TEXT NOT NULL DEFAULT '',
    user         TEXT NOT NULL,
    via          TEXT NOT NULL DEFAULT '',
    ref          TEXT NOT NULL DEFAULT '',   -- run id or batch id it belonged to
    status       TEXT NOT NULL,              -- ok | failed
    detail       TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMP NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_build_installs_build ON build_installs(build_id, created_at);
