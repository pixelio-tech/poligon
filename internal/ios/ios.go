// Package ios wraps the libimobiledevice tools and ios-deploy for the iOS side
// of the farm: listing devices, reading info, installing (re-signed) apps.
//
// Install of an .ipa requires the app to be signed with a provisioning profile
// that covers the target device. poligon re-signs incoming builds with the
// farm's ad-hoc profiles before calling Install (see internal/install).
package ios

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/model"
)

// Tools bundles the external binaries used on the iOS side.
type Tools struct {
	IDeviceID     string // "idevice_id"
	IDeviceInfo   string // "ideviceinfo"
	IOSDeploy     string // "ios-deploy"
	IDeviceSyslog string // "idevicesyslog"
}

// Default returns Tools pointing at the standard binary names on PATH.
func Default() Tools {
	return Tools{
		IDeviceID: "idevice_id", IDeviceInfo: "ideviceinfo", IOSDeploy: "ios-deploy",
		IDeviceSyslog: "idevicesyslog",
	}
}

func run(ctx context.Context, bin string, args ...string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		return out.String(), fmt.Errorf("%s %s: %w: %s", bin, strings.Join(args, " "), err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}

// Available reports whether the libimobiledevice tools are installed.
func (t Tools) Available() bool {
	_, err := exec.LookPath(t.IDeviceID)
	return err == nil
}

// OnlineUDIDs returns the UDIDs of USB-connected devices. If the iOS tools are
// not installed it returns an empty set and no error (the farm may be
// Android-only).
func (t Tools) OnlineUDIDs(ctx context.Context) (map[string]bool, error) {
	if !t.Available() {
		return map[string]bool{}, nil
	}
	out, err := run(ctx, t.IDeviceID, "-l")
	if err != nil {
		return nil, err
	}
	set := map[string]bool{}
	for _, l := range strings.Fields(out) {
		if l != "" {
			set[l] = true
		}
	}
	return set, nil
}

// Specs reads device characteristics via ideviceinfo. RAM/SoC are not exposed
// by iOS, so they are filled from a static table keyed on ProductType.
func (t Tools) Specs(ctx context.Context, udid string) (model.Specs, error) {
	ctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	sp := model.Specs{Battery: -1, Manufacturer: "Apple"}
	get := func(domain, key string) string {
		args := []string{"-u", udid}
		if domain != "" {
			args = append(args, "-q", domain)
		}
		args = append(args, "-k", key)
		v, _ := run(ctx, t.IDeviceInfo, args...)
		return strings.TrimSpace(v)
	}
	product := get("", "ProductType") // e.g. "iPhone13,2"
	sp.OSVersion = get("", "ProductVersion")
	sp.Build = get("", "BuildVersion")
	sp.ScreenSize = "" // available via lockdown but noisy; skip for now

	if hw, ok := hardware[product]; ok {
		sp.Model, sp.SoC, sp.RAM = hw.name, hw.soc, hw.ram
	} else {
		sp.Model = product
	}
	if lvl := get("com.apple.mobile.battery", "BatteryCurrentCapacity"); lvl != "" {
		fmt.Sscan(lvl, &sp.Battery)
	}
	if total := get("com.apple.disk_usage", "TotalDiskCapacity"); total != "" {
		var b float64
		fmt.Sscan(total, &b)
		sp.Storage = fmt.Sprintf("%.0f GB", b/1e9)
	}
	return sp, nil
}

// Install deploys a (re-signed) .app bundle to the device and launches it.
//
// iOS 17 moved developer services behind CoreDevice: there is no
// DeveloperDiskImage to mount any more, so ios-deploy fails on those phones
// ("could not find DeveloperDiskImage"). They go through Xcode's devicectl;
// older phones, which devicectl does not serve, keep ios-deploy. When the OS
// version is not known yet, devicectl is tried first and ios-deploy after.
func (t Tools) Install(ctx context.Context, udid, osVersion, bundleID, appBundlePath string) (string, error) {
	out, err := t.install(ctx, udid, osVersion, bundleID, appBundlePath)
	if err == nil || bundleID == "" || !entitlementMismatch(out+err.Error()) {
		return out, err
	}
	// The copy already on the phone was signed differently (older farm builds
	// signed with a wildcard application-identifier); iOS will not upgrade
	// across that. Remove it and install again — its data goes with it.
	if uout, uerr := t.Uninstall(ctx, udid, osVersion, bundleID); uerr != nil {
		return out + uout, fmt.Errorf("%w (and removing the installed copy failed: %v)", err, uerr)
	}
	out2, err2 := t.install(ctx, udid, osVersion, bundleID, appBundlePath)
	return out + "\n--- reinstalled after removing the differently signed copy ---\n" + out2, err2
}

// entitlementMismatch spots iOS refusing to upgrade an app whose signing
// identity changed.
func entitlementMismatch(s string) bool {
	return strings.Contains(s, "does not match that of the installed application") ||
		strings.Contains(s, "MismatchedApplicationIdentifierEntitlement")
}

// Uninstall removes an app by bundle id.
func (t Tools) Uninstall(ctx context.Context, udid, osVersion, bundleID string) (string, error) {
	if osMajor(osVersion) >= 17 {
		return run(ctx, "xcrun", "devicectl", "device", "uninstall", "app", "--device", udid, bundleID)
	}
	return run(ctx, t.IOSDeploy, "--id", udid, "--uninstall_only", "--bundle_id", bundleID, "--no-wifi")
}

func (t Tools) install(ctx context.Context, udid, osVersion, bundleID, appBundlePath string) (string, error) {
	switch major := osMajor(osVersion); {
	case major >= 17:
		return t.installCoreDevice(ctx, udid, bundleID, appBundlePath)
	case major > 0:
		return t.installLegacy(ctx, udid, appBundlePath)
	}
	out, err := t.installCoreDevice(ctx, udid, bundleID, appBundlePath)
	if err == nil {
		return out, nil
	}
	lout, lerr := t.installLegacy(ctx, udid, appBundlePath)
	if lerr != nil {
		return lout, fmt.Errorf("devicectl: %v; ios-deploy: %w", err, lerr)
	}
	return lout, nil
}

func (t Tools) installLegacy(ctx context.Context, udid, appBundlePath string) (string, error) {
	return run(ctx, t.IOSDeploy, "--id", udid, "--bundle", appBundlePath, "--justlaunch", "--no-wifi")
}

// installCoreDevice installs with `xcrun devicectl` and launches the app.
func (t Tools) installCoreDevice(ctx context.Context, udid, bundleID, appBundlePath string) (string, error) {
	// a paired phone often sits with its CoreDevice tunnel down; asking for
	// its details brings the tunnel up before the install needs it
	wctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	_, _ = run(wctx, "xcrun", "devicectl", "device", "info", "details", "--device", udid, "--quiet")
	cancel()

	out, err := run(ctx, "xcrun", "devicectl", "device", "install", "app", "--device", udid, appBundlePath)
	if err != nil {
		return out, err
	}
	if bundleID == "" {
		return out, nil
	}
	lout, lerr := run(ctx, "xcrun", "devicectl", "device", "process", "launch",
		"--device", udid, "--terminate-existing", bundleID)
	if lerr != nil {
		// installed is what was asked for; a locked phone refuses the launch
		return out + lout + "\nlaunch failed (is the phone locked?): " + lerr.Error(), nil
	}
	return out + lout, nil
}

// osMajor is the major number of an iOS version string ("26.3" -> 26), 0 if
// unknown.
func osMajor(v string) int {
	n := 0
	for _, c := range strings.TrimSpace(v) {
		if c < '0' || c > '9' {
			break
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// SyslogCommand builds (but does not start) a continuous `idevicesyslog`.
// Unlike Android's logcat, iOS has no queryable historical ring buffer — this
// is the only way to see device logs, so the caller runs it continuously in
// the background and reads/truncates its output file to approximate logcat's
// "dump since last clear" semantics (see capture.Capturer).
func (t Tools) SyslogCommand(udid string) *exec.Cmd {
	return exec.Command(t.IDeviceSyslog, "-u", udid)
}

type hw struct{ name, soc, ram string }

// hardware maps ProductType -> marketing name / SoC / RAM. Extend as devices
// join the farm.
var hardware = map[string]hw{
	"iPhone12,1": {"iPhone 11", "A13 Bionic", "4 GB"},
	"iPhone12,8": {"iPhone SE (2nd gen)", "A13 Bionic", "3 GB"},
	"iPhone13,1": {"iPhone 12 mini", "A14 Bionic", "4 GB"},
	"iPhone13,2": {"iPhone 12", "A14 Bionic", "4 GB"},
	"iPhone13,3": {"iPhone 12 Pro", "A14 Bionic", "6 GB"},
	"iPhone13,4": {"iPhone 12 Pro Max", "A14 Bionic", "6 GB"},
	"iPhone14,4": {"iPhone 13 mini", "A15 Bionic", "4 GB"},
	"iPhone14,5": {"iPhone 13", "A15 Bionic", "4 GB"},
	"iPhone14,2": {"iPhone 13 Pro", "A15 Bionic", "6 GB"},
	"iPhone14,3": {"iPhone 13 Pro Max", "A15 Bionic", "6 GB"},
	"iPhone14,7": {"iPhone 14", "A15 Bionic", "6 GB"},
	"iPhone15,2": {"iPhone 14 Pro", "A16 Bionic", "6 GB"},
	"iPhone15,4": {"iPhone 15", "A16 Bionic", "6 GB"},
	"iPhone16,1": {"iPhone 15 Pro", "A17 Pro", "8 GB"},
}
