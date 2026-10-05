// Package capture pulls diagnostics off a device during a manual session or a
// test run: a screenshot, the log buffer, (later) a screen recording. It is the
// shared foundation the Phase 3 test runner builds on.
package capture

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"time"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/ios"
	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/model"
)

// Capturer grabs diagnostics from a device.
type Capturer struct {
	adb      *adb.ADB
	ios      *iosscreen.Controller
	iosTools ios.Tools

	mu     sync.Mutex
	syslog map[string]*syslogCapture // device id -> its running idevicesyslog
}

// syslogCapture is one device's continuous idevicesyslog, writing to a local
// file that Logcat/ClearLogs read and truncate — approximating logcat's own
// ring-buffer semantics on a platform that has no such buffer to query.
type syslogCapture struct {
	cmd  *exec.Cmd
	path string
}

// syslogPattern names the capture files in os.TempDir. New removes leftovers
// from a previous poligon process that died without Shutdown.
const syslogPattern = "poligon-ios-syslog-*.log"

// syslogMax caps a capture file. Everything before a run's ClearLogs is thrown
// away anyway, so an idle device's syslog is emptied once it grows past this.
const syslogMax = 256 << 20

// New builds a Capturer. ic may be nil on an Android-only farm.
func New(a *adb.ADB, ic *iosscreen.Controller, it ios.Tools) *Capturer {
	old, _ := filepath.Glob(filepath.Join(os.TempDir(), syslogPattern))
	for _, p := range old {
		_ = os.Remove(p)
	}
	return &Capturer{adb: a, ios: ic, iosTools: it, syslog: map[string]*syslogCapture{}}
}

// Shutdown kills every background idevicesyslog process. Call it once, on
// poligon exit — otherwise they'd outlive the parent and leak.
func (c *Capturer) Shutdown() {
	c.mu.Lock()
	defer c.mu.Unlock()
	for id, sc := range c.syslog {
		_ = sc.cmd.Process.Kill()
		_ = os.Remove(sc.path)
		delete(c.syslog, id)
	}
}

// ensureSyslog returns the device's running syslog capture, starting one if
// none is active yet (or the previous one has died).
func (c *Capturer) ensureSyslog(dev model.Device) (*syslogCapture, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if sc, ok := c.syslog[dev.ID]; ok && sc.cmd.ProcessState == nil {
		return sc, nil
	}
	tmp, err := os.CreateTemp("", syslogPattern)
	if err != nil {
		return nil, err
	}
	path := tmp.Name()
	tmp.Close()
	// O_APPEND so a Truncate really resets the writer: without it idevicesyslog
	// keeps writing at its old offset and the file fills up with NUL bytes.
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		os.Remove(path)
		return nil, err
	}
	cmd := c.iosTools.SyslogCommand(dev.UDID)
	cmd.Stdout, cmd.Stderr = f, f
	if err := cmd.Start(); err != nil {
		f.Close()
		os.Remove(path)
		return nil, err
	}
	done := make(chan struct{})
	go func() { _ = cmd.Wait(); f.Close(); close(done) }()
	go func() {
		t := time.NewTicker(time.Minute)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if fi, err := os.Stat(path); err == nil && fi.Size() > syslogMax {
					_ = os.Truncate(path, 0)
				}
			}
		}
	}()
	sc := &syslogCapture{cmd: cmd, path: path}
	c.syslog[dev.ID] = sc
	return sc, nil
}

// Screenshot returns image bytes and their MIME type. Android grabs the
// framebuffer directly (PNG); iOS asks WebDriverAgent for a full-resolution
// PNG, falling back to a frame off the live stream (JPEG, scaled down for the
// wall) if that fails. Either way the device's screen must be up.
func (c *Capturer) Screenshot(ctx context.Context, dev model.Device) ([]byte, string, error) {
	switch dev.Platform {
	case model.Android:
		b, err := c.adb.Screenshot(ctx, dev.Serial)
		return b, "image/png", err
	case model.IOS:
		if c.ios == nil || !c.ios.Configured(dev.ID) {
			return nil, "", fmt.Errorf("iOS live screen is not up for %s", dev.ID)
		}
		if b, err := c.ios.Screenshot(dev.ID); err == nil {
			return b, "image/png", nil
		}
		b, err := c.ios.Frame(dev.ID)
		return b, "image/jpeg", err
	default:
		return nil, "", fmt.Errorf("screenshot unsupported on %s", dev.Platform)
	}
}

// Logcat returns the device log buffer since the last call and clears it.
// iOS has no queryable ring buffer, so poligon keeps a continuous
// idevicesyslog running per device (started lazily here, or by ClearLogs)
// and reads + truncates its output file instead.
func (c *Capturer) Logcat(ctx context.Context, dev model.Device) (string, error) {
	switch dev.Platform {
	case model.Android:
		return c.adb.LogcatDump(ctx, dev.Serial)
	case model.IOS:
		sc, err := c.ensureSyslog(dev)
		if err != nil {
			return "", fmt.Errorf("idevicesyslog: %w", err)
		}
		b, err := os.ReadFile(sc.path)
		if err != nil {
			return "", err
		}
		_ = os.Truncate(sc.path, 0)
		return string(b), nil
	default:
		return "", fmt.Errorf("logs unsupported on %s", dev.Platform)
	}
}

// Keyevent presses a hardware/navigation key — the on-screen bar equivalent of
// power/volume/back/home/recents. Android only.
func (c *Capturer) Keyevent(ctx context.Context, dev model.Device, code int) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("hardware keys unsupported on %s", dev.Platform)
	}
	return c.adb.Keyevent(ctx, dev.Serial, code)
}

// ListFiles lists one directory on the device (Android only).
func (c *Capturer) ListFiles(ctx context.Context, dev model.Device, path string) ([]adb.FileEntry, error) {
	if dev.Platform != model.Android {
		return nil, fmt.Errorf("file browser unsupported on %s", dev.Platform)
	}
	return c.adb.ListDir(ctx, dev.Serial, path)
}

// PullFile copies a device file to a local path.
func (c *Capturer) PullFile(ctx context.Context, dev model.Device, remote, local string) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("file browser unsupported on %s", dev.Platform)
	}
	return c.adb.Pull(ctx, dev.Serial, remote, local)
}

// PushFile copies a local file to a device path.
func (c *Capturer) PushFile(ctx context.Context, dev model.Device, local, remote string) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("file browser unsupported on %s", dev.Platform)
	}
	return c.adb.Push(ctx, dev.Serial, local, remote)
}

// RemovePath deletes a file or directory on the device.
func (c *Capturer) RemovePath(ctx context.Context, dev model.Device, path string) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("file browser unsupported on %s", dev.Platform)
	}
	return c.adb.Remove(ctx, dev.Serial, path)
}

// RenamePath moves/renames a device path.
func (c *Capturer) RenamePath(ctx context.Context, dev model.Device, from, to string) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("file browser unsupported on %s", dev.Platform)
	}
	return c.adb.Rename(ctx, dev.Serial, from, to)
}

// ShellCommand returns an unstarted interactive shell process for a caller to
// wire stdio to (Android only — iOS has no equivalent without a jailbreak).
func (c *Capturer) ShellCommand(ctx context.Context, dev model.Device) (*exec.Cmd, error) {
	if dev.Platform != model.Android {
		return nil, fmt.Errorf("shell unsupported on %s", dev.Platform)
	}
	return c.adb.ShellCommand(ctx, dev.Serial), nil
}

// LogcatCommand returns an unstarted continuous log stream process.
func (c *Capturer) LogcatCommand(ctx context.Context, dev model.Device) (*exec.Cmd, error) {
	if dev.Platform != model.Android {
		return nil, fmt.Errorf("logcat unsupported on %s", dev.Platform)
	}
	return c.adb.LogcatCommand(ctx, dev.Serial), nil
}

// ListPackages lists installed apps.
func (c *Capturer) ListPackages(ctx context.Context, dev model.Device, withSystem bool) ([]adb.Package, error) {
	if dev.Platform != model.Android {
		return nil, fmt.Errorf("app manager unsupported on %s", dev.Platform)
	}
	return c.adb.ListPackages(ctx, dev.Serial, withSystem)
}

// ForceStopApp kills every process of a package (terminates an iOS app).
func (c *Capturer) ForceStopApp(ctx context.Context, dev model.Device, pkg string) error {
	if dev.Platform == model.IOS && c.ios != nil && c.ios.Configured(dev.ID) {
		return c.ios.TerminateApp(dev.ID, pkg)
	}
	if dev.Platform != model.Android {
		return fmt.Errorf("app manager unsupported on %s", dev.Platform)
	}
	return c.adb.ForceStop(ctx, dev.Serial, pkg)
}

// ClearAppData wipes a package's data and cache.
func (c *Capturer) ClearAppData(ctx context.Context, dev model.Device, pkg string) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("app manager unsupported on %s", dev.Platform)
	}
	return c.adb.ClearData(ctx, dev.Serial, pkg)
}

// UninstallApp removes a package.
func (c *Capturer) UninstallApp(ctx context.Context, dev model.Device, pkg string) (string, error) {
	if dev.Platform != model.Android {
		return "", fmt.Errorf("app manager unsupported on %s", dev.Platform)
	}
	return c.adb.Uninstall(ctx, dev.Serial, pkg)
}

// LaunchApp starts a package's launcher activity (an iOS bundle id via WDA).
func (c *Capturer) LaunchApp(ctx context.Context, dev model.Device, pkg string) error {
	if dev.Platform == model.IOS && c.ios != nil && c.ios.Configured(dev.ID) {
		return c.ios.LaunchApp(dev.ID, pkg)
	}
	if dev.Platform != model.Android {
		return fmt.Errorf("app manager unsupported on %s", dev.Platform)
	}
	return c.adb.Launch(ctx, dev.Serial, pkg)
}

// StartRecording starts a screen recording (Android only).
func (c *Capturer) StartRecording(dev model.Device) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("screen recording unsupported on %s", dev.Platform)
	}
	return c.adb.StartScreenRecord(dev.Serial)
}

// StopRecording ends the active screen recording so its file can be pulled.
func (c *Capturer) StopRecording(ctx context.Context, dev model.Device) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("screen recording unsupported on %s", dev.Platform)
	}
	return c.adb.StopScreenRecord(ctx, dev.Serial)
}

// UIDump captures the current screen's view hierarchy as XML: uiautomator's
// dump on Android, WebDriverAgent's source on iOS.
func (c *Capturer) UIDump(ctx context.Context, dev model.Device) (string, error) {
	switch dev.Platform {
	case model.Android:
		return c.adb.UIDump(ctx, dev.Serial)
	case model.IOS:
		if c.ios == nil || !c.ios.Configured(dev.ID) {
			return "", fmt.Errorf("iOS live screen is not up for %s", dev.ID)
		}
		return c.ios.Source(ctx, dev.ID)
	default:
		return "", fmt.Errorf("UI dump unsupported on %s", dev.Platform)
	}
}

// OpenSettings jumps the device to one whitelisted system settings screen.
func (c *Capturer) OpenSettings(ctx context.Context, dev model.Device, screen string) error {
	if dev.Platform != model.Android {
		return fmt.Errorf("settings shortcuts unsupported on %s", dev.Platform)
	}
	return c.adb.OpenSettings(ctx, dev.Serial, screen)
}

// ClearLogs empties the device log buffer — call before a test run so a later
// Logcat is scoped to that run. A no-op (nil) on platforms without support.
func (c *Capturer) ClearLogs(ctx context.Context, dev model.Device) error {
	switch dev.Platform {
	case model.Android:
		return c.adb.LogcatClear(ctx, dev.Serial)
	case model.IOS:
		sc, err := c.ensureSyslog(dev)
		if err != nil {
			return fmt.Errorf("idevicesyslog: %w", err)
		}
		return os.Truncate(sc.path, 0)
	}
	return nil
}
