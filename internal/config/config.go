// Package config loads poligon's static configuration: server settings and the
// device inventory (config/devices.yaml).
package config

import (
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/model"
)

// Config is the merged runtime configuration.
type Config struct {
	Listen        string        `yaml:"listen"`         // e.g. ":8080"
	DBPath        string        `yaml:"db_path"`        // sqlite file
	StorageDir    string        `yaml:"storage_dir"`    // artifacts + history
	PollInterval  time.Duration `yaml:"poll_interval"`  // device health poll
	SpecsInterval time.Duration `yaml:"specs_interval"` // specs refresh
	IdleTimeout   time.Duration `yaml:"idle_timeout"`   // auto-release after no heartbeat
	MaxLease      time.Duration `yaml:"max_lease"`      // hard cap on a reservation
	ADBPath       string        `yaml:"adb_path"`
	AutoDiscover  bool          `yaml:"auto_discover"` // register unknown devices on connect
	LiveSidecar   string        `yaml:"live_sidecar"`  // ws-scrcpy base URL, "" disables Android live screen

	// ADBTunnelPort > 0 enables the VS Code live-debug tunnel feature at all;
	// 0 disables it outright. Each developer gets their OWN listening port
	// (see ADBTunnelPortRangeStart/End) exposing the host's local adb server
	// (ADBServerAddr), filtered to just the Android devices they currently
	// hold — see internal/adbfilter for how the filtering works and why it's
	// needed: adb's wire protocol itself has no per-device ACL, so without
	// this filtering layer every device on the farm host would be reachable
	// through anyone's tunnel, regardless of who holds what in poligon.
	ADBTunnelPort           int    `yaml:"adb_tunnel_port"`
	ADBTunnelPortRangeStart int    `yaml:"adb_tunnel_port_range_start"` // first port handed out to a user
	ADBTunnelPortRangeEnd   int    `yaml:"adb_tunnel_port_range_end"`   // last port handed out to a user
	ADBServerAddr           string `yaml:"adb_server_addr"`             // local adb server the tunnel forwards to

	// IOSScreen maps a device id to its running WebDriverAgent endpoints
	// (see internal/iosscreen). Omitted -> iOS live screen disabled.
	IOSScreen map[string]iosscreen.Endpoint `yaml:"ios_screen"`

	// Auth configures the login layer (sessions + TOTP).
	Auth AuthConfig `yaml:"auth"`

	// TLS, when both fields are set, makes poligon serve HTTPS directly.
	// Otherwise it serves plain HTTP (put it behind `tailscale serve` / Caddy).
	TLS TLSConfig `yaml:"tls"`

	// IOSWDA configures automatic WebDriverAgent provisioning when an iOS
	// device is adopted (see internal/provision). Omitted -> iOS adopt fails
	// with a clear message and the manual scripts/ios-wda.sh is still available.
	IOSWDA IOSWDAConfig `yaml:"ios_wda"`

	Devices []DeviceSpec `yaml:"devices"`
}

// AuthConfig tunes the login layer.
type AuthConfig struct {
	SessionTTL  time.Duration `yaml:"session_ttl"`  // absolute cap on a session
	SessionIdle time.Duration `yaml:"session_idle"` // sliding inactivity window
	// PublicURL is the externally reachable base URL (e.g. https://poligon.corp).
	// Used to build the set-password links printed by `poligon user add`.
	PublicURL string `yaml:"public_url"`
}

// TLSConfig points at a certificate + key for direct HTTPS.
type TLSConfig struct {
	Cert string `yaml:"cert"`
	Key  string `yaml:"key"`
}

// IOSWDAConfig holds the inputs scripts/ios-wda.sh needs, so poligon can run
// the same build/run/forward pipeline itself.
type IOSWDAConfig struct {
	Team          string `yaml:"team"`            // Apple DEVELOPMENT_TEAM (10 chars); $POLIGON_WDA_TEAM overrides
	Src           string `yaml:"src"`             // WebDriverAgent checkout dir (default ~/WebDriverAgent)
	DerivedData   string `yaml:"derived_data"`    // xcodebuild -derivedDataPath — must persist across reboots (default ~/Library/Caches/poligon/wda-dd)
	BundleID      string `yaml:"bundle_id"`       // WDA runner bundle id
	DDIDir        string `yaml:"ddi_dir"`         // Developer Disk Image cache for `ios image auto` (iOS 17+); default ~/.cache/poligon/ddi
	WDAPortBase   int    `yaml:"wda_port_base"`   // first host port for WDA http (default 18100)
	MJPEGPortBase int    `yaml:"mjpeg_port_base"` // first host port for WDA mjpeg (default 19100)

	// How WDA should encode the mjpeg stream. Downscaling and lowering quality
	// on the device is far cheaper than shipping full-resolution frames over
	// USB and re-encoding them on the host — a wall tile is ~300px wide, a
	// modern iPhone screenshot is ~1200px.
	MJPEGFramerate int `yaml:"mjpeg_framerate"` // frames per second WDA emits (default 12)
	MJPEGQuality   int `yaml:"mjpeg_quality"`   // JPEG quality 1-100 (default 35)
	MJPEGScale     int `yaml:"mjpeg_scale"`     // scaling factor 1-100 (default 50)
}

// DeviceSpec is one entry in devices.yaml.
type DeviceSpec struct {
	ID       string         `yaml:"id"`
	Platform model.Platform `yaml:"platform"`
	Serial   string         `yaml:"serial"`
	UDID     string         `yaml:"udid"`
	Tags     []string       `yaml:"tags"`
}

// Default returns config with sane defaults applied.
func Default() Config {
	return Config{
		Listen:                  ":8080",
		DBPath:                  "poligon.db",
		StorageDir:              "storage",
		PollInterval:            10 * time.Second,
		SpecsInterval:           time.Hour,
		IdleTimeout:             15 * time.Minute,
		MaxLease:                4 * time.Hour,
		ADBPath:                 "adb",
		AutoDiscover:            true,
		LiveSidecar:             "http://127.0.0.1:8000",
		ADBTunnelPort:           5038,
		ADBTunnelPortRangeStart: 5040,
		ADBTunnelPortRangeEnd:   5090,
		ADBServerAddr:           "127.0.0.1:5037",
		Auth: AuthConfig{
			SessionTTL:  14 * 24 * time.Hour,
			SessionIdle: 24 * time.Hour,
		},
		IOSWDA: IOSWDAConfig{
			Src:           "~/WebDriverAgent",
			DerivedData:   "~/Library/Caches/poligon/wda-dd",
			BundleID:      "com.poligon.WebDriverAgentRunner",
			DDIDir:        "~/.cache/poligon/ddi",
			WDAPortBase:   18100,
			MJPEGPortBase: 19100,

			MJPEGFramerate: 12,
			MJPEGQuality:   35,
			MJPEGScale:     50,
		},
	}
}

// Load reads a YAML config file and overlays it onto the defaults.
func Load(path string) (Config, error) {
	cfg := Default()
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read config: %w", err)
	}
	if err := yaml.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse config: %w", err)
	}
	if err := cfg.validate(); err != nil {
		return cfg, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	seen := map[string]bool{}
	for _, d := range c.Devices {
		if d.ID == "" {
			return fmt.Errorf("device with empty id")
		}
		if seen[d.ID] {
			return fmt.Errorf("duplicate device id %q", d.ID)
		}
		seen[d.ID] = true
		switch d.Platform {
		case model.Android:
			if d.Serial == "" {
				return fmt.Errorf("device %q: android needs serial", d.ID)
			}
		case model.IOS:
			if d.UDID == "" {
				return fmt.Errorf("device %q: ios needs udid", d.ID)
			}
		default:
			return fmt.Errorf("device %q: bad platform %q", d.ID, d.Platform)
		}
	}
	return nil
}
