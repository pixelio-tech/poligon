package runner

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// Maestro's defaults reinstall its two on-device apps (the driver and its
// instrumentation server) at the start of every `maestro test` and uninstall
// them again at the end — two installs per run, each one a chance for MIUI to
// pop "install via USB?" and refuse. With --no-reinstall-driver Maestro
// installs them only when they are missing and leaves them in place.
//
// The one thing that flag gets wrong is an upgrade: a driver left behind by
// an older Maestro stays. So the farm remembers, per phone, which Maestro
// version put the driver there, and clears it itself when that changes.

// maestroDriverPackages are the apps Maestro installs on an Android phone.
var maestroDriverPackages = []string{"dev.mobile.maestro", "dev.mobile.maestro.test"}

type driverCache struct {
	path string

	mu      sync.Mutex
	loaded  bool
	version string            // installed maestro CLI, read once
	devices map[string]string // adb serial -> maestro version whose driver is on it
}

func newDriverCache(path string) *driverCache {
	return &driverCache{path: path, devices: map[string]string{}}
}

func (c *driverCache) load() {
	if c.loaded {
		return
	}
	c.loaded = true
	if b, err := os.ReadFile(c.path); err == nil {
		_ = json.Unmarshal(b, &c.devices)
	}
}

func (c *driverCache) save() {
	b, _ := json.MarshalIndent(c.devices, "", "  ")
	_ = os.MkdirAll(filepath.Dir(c.path), 0o755)
	tmp := c.path + ".tmp"
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, c.path)
	}
}

// current reports whether the phone carries the driver of this Maestro.
func (c *driverCache) current(serial, version string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	return version != "" && c.devices[serial] == version
}

// mark records that a run on the phone got its driver working with this Maestro.
func (c *driverCache) mark(serial, version string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	if version == "" || c.devices[serial] == version {
		return
	}
	c.devices[serial] = version
	c.save()
}

// forget drops what is known about the phone's driver.
func (c *driverCache) forget(serial string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.load()
	if _, ok := c.devices[serial]; !ok {
		return
	}
	delete(c.devices, serial)
	c.save()
}

// maestroVersion is the installed CLI's version, asked once. "" when it
// cannot be read — the driver is then never trusted and always cleared first.
func (r *Runner) maestroVersion(ctx context.Context) string {
	c := r.drivers
	c.mu.Lock()
	v := c.version
	c.mu.Unlock()
	if v != "" {
		return v
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, r.maestro, "--version").Output()
	if err != nil {
		return ""
	}
	// the version is the last line; earlier ones can be update notices
	lines := strings.Split(strings.TrimSpace(string(out)), "\n")
	v = strings.TrimSpace(lines[len(lines)-1])
	c.mu.Lock()
	c.version = v
	c.mu.Unlock()
	return v
}

// clearMaestroDriver uninstalls Maestro's apps from the phone, so the next
// `maestro test --no-reinstall-driver` installs the current ones.
func (r *Runner) clearMaestroDriver(ctx context.Context, serial string) {
	for _, pkg := range maestroDriverPackages {
		_, _ = r.adb.Uninstall(ctx, serial, pkg) // not installed is fine
	}
	r.drivers.forget(serial)
}
