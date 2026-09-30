// Package model holds the core domain types shared across poligon.
package model

import "path/filepath"

import "time"

// Platform is the device OS family.
type Platform string

const (
	Android Platform = "android"
	IOS     Platform = "ios"
)

// DeviceStatus is the lifecycle state of a device in the pool.
type DeviceStatus string

const (
	StatusOffline      DeviceStatus = "offline"      // not physically connected
	StatusUnauthorized DeviceStatus = "unauthorized" // connected but adb/pairing not approved on the device
	StatusFree         DeviceStatus = "free"         // connected, nobody holds it
	StatusReserved     DeviceStatus = "reserved"     // held by a user, no live session yet
	StatusBusy         DeviceStatus = "busy"         // live session open / install running
	StatusRunningTest  DeviceStatus = "running"      // automated job in progress
	StatusDegraded     DeviceStatus = "degraded"     // flapping online/offline, excluded from scheduling
	StatusMaintenance  DeviceStatus = "maintenance"  // manually pulled out of the pool
)

// Source records how a device entered the pool.
type Source string

const (
	SourceConfig Source = "config" // declared in devices.yaml
	SourceAuto   Source = "auto"   // discovered on connect
)

// Device is one phone wired to the farm host.
type Device struct {
	ID       string       `json:"id"` // stable human id, e.g. "pixel6-01"
	Platform Platform     `json:"platform"`
	Serial   string       `json:"serial"` // adb serial (android)
	UDID     string       `json:"udid"`   // device udid (ios)
	Tags     []string     `json:"tags"`
	Status   DeviceStatus `json:"status"`
	Source   Source       `json:"source"`
	Specs    Specs        `json:"specs"`
	LastSeen time.Time    `json:"last_seen"`
	// Adopted is true once the device has been prepared and admitted to the
	// pool. Config devices are adopted on load; auto-discovered devices start
	// as candidates (adopted=false) until a user runs "connect to farm".
	Adopted bool `json:"adopted"`
}

// Specs are hardware/OS characteristics, refreshed periodically.
type Specs struct {
	Model         string `json:"model"`
	Manufacturer  string `json:"manufacturer"`
	SoC           string `json:"soc"`
	OSVersion     string `json:"os_version"`
	APILevel      string `json:"api_level,omitempty"` // android
	Build         string `json:"build,omitempty"`     // ios
	RAM           string `json:"ram"`
	Storage       string `json:"storage"`
	ScreenSize    string `json:"screen_size"` // px, e.g. "1080x2400"
	ScreenDensity string `json:"screen_density"`
	Battery       int    `json:"battery"` // percent, -1 unknown
	BatteryTempC  string `json:"battery_temp_c,omitempty"`
	// InputInjection is "ok" or "blocked" (android): "blocked" means the OS
	// refuses simulated taps/swipes — on MIUI the "USB debugging (Security
	// settings)" toggle is off, so the live screen shows but is not tappable.
	InputInjection string `json:"input_injection,omitempty"`
}

// Reservation is a user's hold on a device.
type Reservation struct {
	ID        int64     `json:"id"`
	DeviceID  string    `json:"device_id"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"` // hard cap
	RenewedAt time.Time `json:"renewed_at"` // last heartbeat
	Released  bool      `json:"released"`
}

// User is a farm account. Name is the login identifier (an email address);
// reservations and installs reference it, so it stays the primary key.
// Registration is open and every account is equal — there is no admin role.
type User struct {
	Name      string    `json:"name"`
	TokenHash string    `json:"-"` // legacy bearer token; unused by new logins
	Disabled  bool      `json:"disabled"`
	PassSet   bool      `json:"pass_set"` // password chosen; can log in
	CreatedAt time.Time `json:"created_at"`
}

// RunStatus is the lifecycle state of a test run or one device within it.
type RunStatus string

const (
	RunQueued   RunStatus = "queued"
	RunRunning  RunStatus = "running"
	RunPassed   RunStatus = "passed"
	RunFailed   RunStatus = "failed" // ran, assertions did not hold
	RunError    RunStatus = "error"  // could not run (install failed, device lost)
	RunCanceled RunStatus = "canceled"
	RunPending  RunStatus = "pending" // per-device: not started yet
	RunSkipped  RunStatus = "skipped" // per-device: run type unsupported on this device
)

// Run is one automated test run over a set of reserved devices.
type Run struct {
	ID         string      `json:"id"`
	User       string      `json:"user"`
	Type       string      `json:"type"`
	Status     RunStatus   `json:"status"`
	Trigger    string      `json:"trigger"`
	Batch      string      `json:"-"`
	Spec       RunSpec     `json:"spec"`
	Detail     string      `json:"detail,omitempty"`
	CreatedAt  time.Time   `json:"created_at"`
	StartedAt  *time.Time  `json:"started_at,omitempty"`
	FinishedAt *time.Time  `json:"finished_at,omitempty"`
	Devices    []RunDevice `json:"devices,omitempty"`
}

// RunSpec carries the run's inputs. Artifacts are stored under the run's upload
// dir keyed by platform; WatchSeconds is the smoke-test settle window.
type RunSpec struct {
	Artifacts      map[Platform]string `json:"artifacts,omitempty"`      // platform -> stored filename (app under test)
	TestArtifacts  map[Platform]string `json:"test_artifacts,omitempty"` // integration_test: platform -> androidTest apk
	WatchSeconds   int                 `json:"watch_seconds,omitempty"`
	FlowPath       string              `json:"flow_path,omitempty"`       // maestro: a .yaml, a workspace dir, or a flow inside one
	Env            map[string]string   `json:"env,omitempty"`             // maestro: -e KEY=VALUE for the flows
	IncludeTags    string              `json:"include_tags,omitempty"`    // maestro: --include-tags (comma-separated)
	ExcludeTags    string              `json:"exclude_tags,omitempty"`    // maestro: --exclude-tags (comma-separated)
	Command        string              `json:"command,omitempty"`         // command run type
	TimeoutSeconds int                 `json:"timeout_seconds,omitempty"` // per-device cap (default 1200)
	CallbackURL    string              `json:"callback_url,omitempty"`    // POSTed the run JSON on finish
}

// RunDevice is one device's slice of a run.
type RunDevice struct {
	RunID      string     `json:"-"`
	DeviceID   string     `json:"device_id"`
	Platform   Platform   `json:"platform"`
	Status     RunStatus  `json:"status"`
	Detail     string     `json:"detail,omitempty"`
	Package    string     `json:"package,omitempty"`
	Artifacts  []string   `json:"artifacts"`
	StartedAt  *time.Time `json:"started_at,omitempty"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Session is a browser login: a server-side record keyed by a random token,
// revocable and expiring. The token itself lives only in the client cookie;
// the store keeps its hash.
type Session struct {
	ID        int64     `json:"id"`
	User      string    `json:"user"`
	CreatedAt time.Time `json:"created_at"`
	LastSeen  time.Time `json:"last_seen"`
	ExpiresAt time.Time `json:"expires_at"`
	IP        string    `json:"ip"`
	UserAgent string    `json:"user_agent"`
	Revoked   bool      `json:"revoked"`
}

// Redacted replaces server-side file paths in a run's spec with base names before
// it goes over the wire.
func (run Run) Redacted() Run {
	if run.Spec.FlowPath != "" {
		run.Spec.FlowPath = filepath.Base(run.Spec.FlowPath)
	}
	// env often carries test credentials — show which keys were set, not values
	if len(run.Spec.Env) > 0 {
		e := make(map[string]string, len(run.Spec.Env))
		for k := range run.Spec.Env {
			e[k] = "***"
		}
		run.Spec.Env = e
	}
	if len(run.Spec.Artifacts) > 0 {
		a := make(map[Platform]string, len(run.Spec.Artifacts))
		for p, v := range run.Spec.Artifacts {
			a[p] = filepath.Base(v)
		}
		run.Spec.Artifacts = a
	}
	if len(run.Spec.TestArtifacts) > 0 {
		a := make(map[Platform]string, len(run.Spec.TestArtifacts))
		for p, v := range run.Spec.TestArtifacts {
			a[p] = filepath.Base(v)
		}
		run.Spec.TestArtifacts = a
	}
	return run
}

// Build is one app file that has been installed onto a farm phone. Identical
// files (same sha256) are one Build, however many times they were uploaded.
type Build struct {
	ID         int64      `json:"id"`
	SHA256     string     `json:"sha256"`
	Filename   string     `json:"filename"`
	Platform   Platform   `json:"platform"`
	Format     string     `json:"format"`
	Size       int64      `json:"size"`
	Package    string     `json:"package,omitempty"`
	AppName    string     `json:"app_name,omitempty"`
	Version    string     `json:"version,omitempty"`
	BuildCode  string     `json:"build_code,omitempty"`
	MinOS      string     `json:"min_os,omitempty"`
	Path       string     `json:"-"`
	UploadedBy string     `json:"uploaded_by"`
	UploadedAt time.Time  `json:"uploaded_at"`
	Via        string     `json:"via,omitempty"`
	SourceURL  string     `json:"source_url,omitempty"`
	Note       string     `json:"note,omitempty"`
	DeletedAt  *time.Time `json:"deleted_at,omitempty"`

	// summary of build_installs, filled by list queries
	Installs      int        `json:"installs"`
	InstallsOK    int        `json:"installs_ok"`
	LastInstalled *time.Time `json:"last_installed_at,omitempty"`
	Devices       int        `json:"devices"`
}

// BuildInstall is one install of a Build onto a device.
type BuildInstall struct {
	ID          int64     `json:"id"`
	BuildID     int64     `json:"build_id"`
	DeviceID    string    `json:"device_id"`
	DeviceModel string    `json:"device_model,omitempty"`
	OSVersion   string    `json:"os_version,omitempty"`
	User        string    `json:"user"`
	Via         string    `json:"via,omitempty"`
	Ref         string    `json:"ref,omitempty"`
	Status      string    `json:"status"`
	Detail      string    `json:"detail,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}
