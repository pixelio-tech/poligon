package runner

import (
	"bytes"
	"context"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/procgroup"
)

// runMaestro installs the build (if one was supplied) and runs a Maestro flow
// against the device. Maestro auto-detects the platform from --device, so the
// same flow drives Android and iOS.
func (r *Runner) runMaestro(ctx context.Context, run model.Run, dev model.Device, rd *model.RunDevice, devDir string) {
	if run.Spec.FlowPath == "" {
		rd.Status, rd.Detail = model.RunError, "no flow file"
		return
	}

	if why := maestroUnsupported(dev); why != "" {
		rd.Status, rd.Detail = model.RunError, why
		return
	}

	_ = r.st.SetDeviceStatus(dev.ID, model.StatusRunningTest, time.Now())
	defer r.st.SetDeviceStatus(dev.ID, model.StatusReserved, time.Now())

	if art := run.Spec.Artifacts[dev.Platform]; art != "" {
		_ = r.cap.ClearLogs(ctx, dev)
		if _, ierr := r.inst.Run(ctx, dev, art, runOrigin(run)); ierr != nil {
			rd.Status, rd.Detail = model.RunError, "install failed: "+ierr.Error()
			r.saveLog(ctx, dev, rd, devDir)
			return
		}
	}

	target := dev.Serial
	if dev.Platform == model.IOS {
		target = dev.UDID
		if err := wakeCoreDevice(ctx, dev.UDID); err != nil {
			r.log.Warn("maestro: could not bring up the CoreDevice tunnel", "device", dev.ID, "err", err)
		}
	}
	report := filepath.Join(devDir, "report.xml")
	debug := filepath.Join(devDir, "maestro")

	var buf bytes.Buffer
	var runErr error
	for attempt := 1; ; attempt++ {
		cmd := exec.CommandContext(ctx, r.maestro, maestroArgs(run.Spec, dev, target, report, debug)...)
		cmd.Env = append(os.Environ(), "MAESTRO_CLI_NO_ANALYTICS=1", "CI=true")
		procgroup.Bind(cmd) // a cancel/timeout takes the whole tree down, not only the wrapper
		start := buf.Len()
		cmd.Stdout, cmd.Stderr = &buf, &buf
		runErr = cmd.Run()
		// MIUI answers the install of Maestro's on-device driver with a
		// "install via USB?" prompt that it sometimes declines on its own —
		// the same run passes when simply started again
		if runErr == nil || attempt == 2 || ctx.Err() != nil ||
			!driverInstallRefused(buf.Bytes()[start:]) {
			break
		}
		r.log.Info("maestro: driver install refused by the phone, retrying", "run", run.ID, "device", dev.ID)
		fmt.Fprintf(&buf, "\n--- poligon: the phone refused to install Maestro's driver (INSTALL_FAILED_USER_RESTRICTED); retrying once ---\n\n")
	}

	if os.WriteFile(filepath.Join(devDir, "maestro.log"), buf.Bytes(), 0o644) == nil {
		rd.Artifacts = append(rd.Artifacts, "maestro.log")
	}
	if _, err := os.Stat(report); err == nil {
		rd.Artifacts = append(rd.Artifacts, "report.xml")
	}
	// pull in whatever maestro wrote (recording.mp4, screenshots, ...)
	_ = filepath.WalkDir(debug, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if rel, e := filepath.Rel(devDir, p); e == nil {
			rd.Artifacts = append(rd.Artifacts, filepath.ToSlash(rel))
		}
		return nil
	})

	if img, mime, err := r.cap.Screenshot(ctx, dev); err == nil {
		name := "screenshot.png"
		if mime == "image/jpeg" {
			name = "screenshot.jpg"
		}
		if os.WriteFile(filepath.Join(devDir, name), img, 0o644) == nil {
			rd.Artifacts = append(rd.Artifacts, name)
		}
	}
	r.saveLog(ctx, dev, rd, devDir)

	switch {
	case ctx.Err() != nil:
		rd.Status, rd.Detail = model.RunCanceled, "canceled"
	case runErr != nil:
		// a driver install refused by the phone ends in a long stack trace;
		// name the cause instead of showing its last 400 characters
		if h := adb.InstallHint(buf.String()); h != "" {
			rd.Status, rd.Detail = model.RunError, "maestro could not install its driver: "+h
		} else {
			rd.Status, rd.Detail = model.RunFailed, "maestro: "+tail(buf.String(), 400)
		}
	default:
		rd.Status, rd.Detail = model.RunPassed, ""
	}
}

// maestroArgs builds the `maestro test` command line. The flows always get
// POLIGON_DEVICE_ID / POLIGON_PLATFORM, so one suite run on several phones can
// pick per-device data (a test account each, say); caller env cannot override
// them.
func maestroArgs(spec model.RunSpec, dev model.Device, target, report, debug string) []string {
	args := []string{"--device", target,
		"test", "--format", "junit", "--output", report,
		"--debug-output", debug}
	keys := make([]string, 0, len(spec.Env))
	for k := range spec.Env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		args = append(args, "-e", k+"="+spec.Env[k])
	}
	args = append(args,
		"-e", "POLIGON_DEVICE_ID="+dev.ID,
		"-e", "POLIGON_PLATFORM="+string(dev.Platform))
	if spec.IncludeTags != "" {
		args = append(args, "--include-tags", spec.IncludeTags)
	}
	if spec.ExcludeTags != "" {
		args = append(args, "--exclude-tags", spec.ExcludeTags)
	}
	return append(args, spec.FlowPath)
}

// resolveMaestro finds the maestro binary: PATH first, then the default
// install location (~/.maestro/bin), else the bare name so the error is clear.
func resolveMaestro() string {
	if p, err := exec.LookPath("maestro"); err == nil {
		return p
	}
	if home, err := os.UserHomeDir(); err == nil {
		p := filepath.Join(home, ".maestro", "bin", "maestro")
		if _, err := os.Stat(p); err == nil {
			return p
		}
	}
	return "maestro"
}

// tail returns the last n characters of s, trimmed, prefixed with an ellipsis
// when it was cut.
func tail(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return "…" + s[len(s)-n:]
}

// driverInstallRefused reports a MIUI refusal to install Maestro's driver apk.
func driverInstallRefused(out []byte) bool {
	return bytes.Contains(out, []byte("INSTALL_FAILED_USER_RESTRICTED"))
}

// maestroUnsupported explains up front why Maestro cannot drive a device,
// instead of its own "Device … was requested, but it is not connected".
// Maestro finds physical iPhones through CoreDevice (`xcrun devicectl`),
// which only knows iOS 17 and later.
func maestroUnsupported(dev model.Device) string {
	if dev.Platform != model.IOS {
		return ""
	}
	major, _ := strconv.Atoi(strings.SplitN(dev.Specs.OSVersion, ".", 2)[0])
	if major > 0 && major < 17 {
		return fmt.Sprintf("Maestro cannot run on this iPhone: it finds physical iPhones through CoreDevice (xcrun devicectl), which supports iOS 17+, and %s is on iOS %s. Drive it through the MCP tools (tap, type_text, ui_tree) or use an iPhone on iOS 17+.",
			dev.ID, dev.Specs.OSVersion)
	}
	return ""
}

// wakeCoreDevice brings up the device's CoreDevice tunnel. Maestro lists
// physical iPhones with `xcrun devicectl` and treats one whose tunnel is idle
// ("disconnected" — the normal state between uses) as not connected at all;
// asking devicectl for the device's details connects the tunnel.
func wakeCoreDevice(ctx context.Context, udid string) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xcrun", "devicectl", "device", "info", "details", "--device", udid).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, tail(string(out), 300))
	}
	return nil
}
