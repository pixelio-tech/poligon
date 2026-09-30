package runner

import (
	"bytes"
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/procgroup"
)

// runCommand is the generic escape hatch: install the build (if any), then run
// an arbitrary shell command with the device coordinates in the environment and
// the artifact dir as the working directory. Exit 0 = passed.
//
// Env handed to the command:
//
//	POLIGON_DEVICE_ID   the farm device id
//	POLIGON_PLATFORM    android | ios
//	POLIGON_RUN_DIR     the per-device artifact dir (also cwd)
//	ANDROID_SERIAL      adb serial   (android)
//	DEVICE_UDID         device udid  (ios)
func (r *Runner) runCommand(ctx context.Context, run model.Run, dev model.Device, rd *model.RunDevice, devDir string) {
	command := run.Spec.Command
	if command == "" {
		rd.Status, rd.Detail = model.RunError, "no command"
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

	// snapshot existing files so we only attach what the command produces
	before := map[string]bool{}
	_ = filepath.WalkDir(devDir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			before[p] = true
		}
		return nil
	})

	cmd := exec.CommandContext(ctx, "/bin/sh", "-c", command)
	cmd.Dir = devDir
	procgroup.Bind(cmd) // a cancel/timeout takes the whole tree down, not only the wrapper
	cmd.Env = append(os.Environ(),
		"POLIGON_DEVICE_ID="+dev.ID,
		"POLIGON_PLATFORM="+string(dev.Platform),
		"POLIGON_RUN_DIR="+devDir,
	)
	if dev.Platform == model.Android {
		cmd.Env = append(cmd.Env, "ANDROID_SERIAL="+dev.Serial)
	} else {
		cmd.Env = append(cmd.Env, "DEVICE_UDID="+dev.UDID)
	}
	var buf bytes.Buffer
	cmd.Stdout, cmd.Stderr = &buf, &buf
	runErr := cmd.Run()

	if os.WriteFile(filepath.Join(devDir, "command.log"), buf.Bytes(), 0o644) == nil {
		rd.Artifacts = append(rd.Artifacts, "command.log")
	}
	_ = filepath.WalkDir(devDir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() || before[p] || filepath.Base(p) == "command.log" {
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
		rd.Status, rd.Detail = model.RunCanceled, "canceled or timed out"
	case runErr != nil:
		rd.Status, rd.Detail = model.RunFailed, "command exited non-zero: "+tail(buf.String(), 400)
	default:
		rd.Status, rd.Detail = model.RunPassed, ""
	}
}
