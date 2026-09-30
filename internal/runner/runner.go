// Package runner executes automated test runs on a set of reserved devices.
// A run holds a reservation batch for its lifetime, fans out across its devices
// (parallel, bounded), and writes per-device artifacts under <dir>/<run>/<device>/.
//
// Run types:
//   - install_smoke: install the build, launch it, wait, then assert it's
//     still running — Android checks pidof + scans logcat for a crash; iOS
//     (no root, so no pidof/crash-signal equivalent) checks it's still the
//     foreground app via WDA instead. Saves a screenshot + the log (Android:
//     logcat; iOS: idevicesyslog, captured since the run started).
//   - integration_test: Android only — install the app apk + its separately
//     built androidTest apk, run `am instrument -w` against the runner class
//     read from the test apk's manifest. Not yet on iOS (needs a .xctestrun
//     bundle + xcodebuild test-without-building, and Xcode on the host).
package runner

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/capture"
	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/reserve"
	"github.com/pancir/poligon/internal/store"
)

// Types is the set of run types the runner understands.
var Types = map[string]bool{"install_smoke": true, "maestro": true, "command": true, "integration_test": true}

// Runner schedules and executes runs. One run executes at a time; devices within
// a run run in parallel.
type Runner struct {
	st   *store.Store
	res  *reserve.Manager
	inst *install.Installer
	cap  *capture.Capturer
	adb  *adb.ADB
	ios  *iosscreen.Controller
	log  *slog.Logger

	dir     string // artifact root: <StorageDir>/runs
	maestro string // maestro binary (path or name on PATH)
	par     int    // max parallel devices per run

	wake    chan struct{}
	mu      sync.Mutex
	cancels map[string]context.CancelFunc

	drivers *driverCache // which Maestro's driver each phone carries

	// restartScreen brings an iPhone's live screen (WebDriverAgent) back and
	// returns once it is up; nil on a farm without iOS provisioning
	restartScreen func(ctx context.Context, deviceID string) error
}

// SetScreenRestarter wires the live-screen restart the iOS integration_test
// run needs after its XCTest session has pushed WebDriverAgent aside.
func (r *Runner) SetScreenRestarter(f func(ctx context.Context, deviceID string) error) {
	r.restartScreen = f
}

// New builds a Runner. dir is created on first use; maestroBin defaults to
// "maestro" when empty. ios may be nil on an Android-only farm (install_smoke
// then just skips iOS devices, same as before).
func New(st *store.Store, res *reserve.Manager, inst *install.Installer, cap *capture.Capturer, a *adb.ADB, ios *iosscreen.Controller, dir, maestroBin string, log *slog.Logger) *Runner {
	if maestroBin == "" {
		maestroBin = resolveMaestro()
	}
	return &Runner{
		st: st, res: res, inst: inst, cap: cap, adb: a, ios: ios, log: log,
		dir: dir, maestro: maestroBin, par: 4,
		wake:    make(chan struct{}, 1),
		cancels: map[string]context.CancelFunc{},
		drivers: newDriverCache(filepath.Join(dir, ".maestro-drivers.json")),
	}
}

// Submit reserves the devices, records a queued run, and wakes the scheduler.
func (r *Runner) Submit(user, typ string, spec model.RunSpec, deviceIDs []string, trigger string) (model.Run, error) {
	if !Types[typ] {
		return model.Run{}, fmt.Errorf("unknown run type %q", typ)
	}
	seen := map[string]bool{}
	plats := map[string]model.Platform{}
	for _, id := range deviceIDs {
		if seen[id] {
			continue
		}
		seen[id] = true
		dev, err := r.st.Device(id)
		if err != nil {
			return model.Run{}, err
		}
		plats[id] = dev.Platform
	}
	if len(plats) == 0 {
		return model.Run{}, fmt.Errorf("no devices")
	}

	ids := make([]string, 0, len(plats))
	for id := range plats {
		ids = append(ids, id)
	}

	// Reuse the caller's existing hold when every device is already theirs
	// (the screen-grid flow); otherwise reserve a fresh batch (the API flow).
	held, free := 0, 0
	for _, id := range ids {
		if res, ok, _ := r.res.Holder(id); ok && res.User == user {
			held++
		} else if ok {
			return model.Run{}, fmt.Errorf("%s is reserved by %s", id, res.User)
		} else {
			free++
		}
	}
	var batch string
	if free > 0 && held == 0 {
		b, _, err := r.res.ReserveMany(ids, user)
		if err != nil {
			return model.Run{}, fmt.Errorf("reserve devices: %w", err)
		}
		batch = b
	} else if free > 0 {
		return model.Run{}, fmt.Errorf("mix of held and free devices — reserve all or none first")
	}
	// batch == "" means the devices are borrowed from the caller's own hold and
	// must not be released when the run ends.

	run := model.Run{
		ID: newID(), User: user, Type: typ, Trigger: trigger,
		Batch: batch, Spec: spec, Status: model.RunQueued,
	}
	if err := r.st.CreateRun(run, plats); err != nil {
		if batch != "" {
			_ = r.res.ReleaseBatch(batch, user, true)
		}
		return model.Run{}, err
	}
	r.signal()
	r.log.Info("run submitted", "run", run.ID, "type", typ, "user", user, "devices", len(ids))
	return r.st.Run(run.ID)
}

// Cancel stops an in-flight run.
func (r *Runner) Cancel(id string) bool {
	r.mu.Lock()
	cancel, ok := r.cancels[id]
	r.mu.Unlock()
	if ok {
		cancel()
	}
	return ok
}

// recover cleans up runs left "running" by a previous process: fail their
// unfinished devices, mark the run errored, release its held batch.
func (r *Runner) recover() {
	orphans, err := r.st.OrphanRuns()
	if err != nil {
		r.log.Warn("runner: scan orphans", "err", err)
		return
	}
	for id, batch := range orphans {
		_ = r.st.FailRunDevicesNotDone(id, "poligon restarted mid-run")
		fin := time.Now()
		_ = r.st.SetRunStatus(id, model.RunError, "poligon restarted mid-run", nil, &fin)
		if owner, e := r.st.RunOwner(id); e == nil && batch != "" {
			_ = r.res.ReleaseBatch(batch, owner, true)
		}
		r.log.Info("runner: recovered orphan run", "run", id)
	}
}

// Run is the scheduler loop. Blocks until ctx is done.
func (r *Runner) Run(ctx context.Context) {
	r.recover()
	t := time.NewTicker(15 * time.Second)
	defer t.Stop()
	for {
		r.drainQueue(ctx)
		select {
		case <-ctx.Done():
			return
		case <-r.wake:
		case <-t.C:
		}
	}
}

func (r *Runner) drainQueue(ctx context.Context) {
	for {
		ids, err := r.st.QueuedRunIDs()
		if err != nil {
			r.log.Warn("runner: list queue", "err", err)
			return
		}
		if len(ids) == 0 || ctx.Err() != nil {
			return
		}
		r.execute(ctx, ids[0])
	}
}

func (r *Runner) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

func (r *Runner) execute(parent context.Context, id string) {
	run, err := r.st.Run(id)
	if err != nil {
		r.log.Warn("runner: load run", "run", id, "err", err)
		return
	}
	if run.Status != model.RunQueued {
		return
	}

	ctx, cancel := context.WithCancel(parent)
	r.mu.Lock()
	r.cancels[id] = cancel
	r.mu.Unlock()
	defer func() {
		r.mu.Lock()
		delete(r.cancels, id)
		r.mu.Unlock()
		cancel()
	}()

	now := time.Now()
	_ = r.st.SetRunStatus(id, model.RunRunning, "", &now, nil)

	// keep the reservation batch alive while the run works (only when the run
	// owns the batch; a borrowed hold is the caller's to renew)
	hbStop := make(chan struct{})
	if run.Batch != "" {
		go func() {
			tk := time.NewTicker(45 * time.Second)
			defer tk.Stop()
			for {
				select {
				case <-hbStop:
					return
				case <-tk.C:
					_ = r.res.HeartbeatBatch(run.Batch, run.User)
				}
			}
		}()
	}

	sem := make(chan struct{}, r.par)
	var wg sync.WaitGroup
	for i := range run.Devices {
		rd := run.Devices[i]
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			r.runDevice(ctx, run, rd)
		}()
	}
	wg.Wait()
	close(hbStop)

	// aggregate final status from the freshly-written device rows
	final, err := r.st.Run(id)
	status, detail := model.RunPassed, ""
	if err == nil {
		var pass, fail, errd, skip int
		for _, d := range final.Devices {
			switch d.Status {
			case model.RunPassed:
				pass++
			case model.RunFailed:
				fail++
			case model.RunError:
				errd++
			case model.RunSkipped:
				skip++
			}
		}
		switch {
		case ctx.Err() != nil:
			status, detail = model.RunCanceled, "canceled"
		case errd > 0:
			status, detail = model.RunError, fmt.Sprintf("%d device(s) errored", errd)
		case fail > 0:
			status, detail = model.RunFailed, fmt.Sprintf("%d device(s) failed", fail)
		case pass == 0 && skip > 0:
			status, detail = model.RunError, "every device skipped this run type"
		}
	}
	fin := time.Now()
	_ = r.st.SetRunStatus(id, status, detail, nil, &fin)
	if run.Batch != "" {
		_ = r.res.ReleaseBatch(run.Batch, run.User, true)
	}
	r.log.Info("run finished", "run", id, "status", status)

	if run.Spec.CallbackURL != "" {
		if done, e := r.st.Run(id); e == nil {
			go r.fireCallback(run.Spec.CallbackURL, done)
		}
	}
}

func (r *Runner) runDevice(ctx context.Context, run model.Run, rd model.RunDevice) {
	start := time.Now()
	rd.RunID = run.ID
	rd.StartedAt = &start
	rd.Status = model.RunRunning
	_ = r.st.SetRunDevice(rd)

	defer func() {
		fin := time.Now()
		rd.FinishedAt = &fin
		if rd.Status == model.RunRunning {
			rd.Status = model.RunError
			rd.Detail = "run ended without a verdict"
		}
		_ = r.st.SetRunDevice(rd)
	}()

	dev, err := r.st.Device(rd.DeviceID)
	if err != nil {
		rd.Status, rd.Detail = model.RunError, err.Error()
		return
	}

	devDir := filepath.Join(r.dir, run.ID, rd.DeviceID)
	if err := os.MkdirAll(devDir, 0o755); err != nil {
		rd.Status, rd.Detail = model.RunError, err.Error()
		return
	}

	timeout := 20 * time.Minute
	if t := run.Spec.TimeoutSeconds; t > 0 {
		timeout = time.Duration(t) * time.Second
	}
	dctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if dev.Platform == model.Android {
		if err := r.adb.WakeUnlock(dctx, dev.Serial); err != nil {
			r.log.Warn("runner: wake/unlock failed", "device", dev.ID, "err", err)
		}
	}

	switch run.Type {
	case "install_smoke":
		r.smoke(dctx, run, dev, &rd, devDir)
	case "maestro":
		r.runMaestro(dctx, run, dev, &rd, devDir)
	case "command":
		r.runCommand(dctx, run, dev, &rd, devDir)
	case "integration_test":
		r.runIntegrationTest(dctx, run, dev, &rd, devDir)
	default:
		rd.Status, rd.Detail = model.RunSkipped, "unknown run type"
	}

	if rd.Status == model.RunRunning && dctx.Err() == context.DeadlineExceeded {
		rd.Status, rd.Detail = model.RunError, "timed out after "+timeout.String()
	}
}

// smoke: install → launch → settle → assert alive + no crash.
// Android checks pidof + scans the logcat window for a crash; iOS has no
// pidof and nothing crash-scans idevicesyslog yet, so it checks WDA's
// activeAppInfo instead — the app must still be the foreground process (not
// bounced back to the springboard) with a live pid.
func (r *Runner) smoke(ctx context.Context, run model.Run, dev model.Device, rd *model.RunDevice, devDir string) {
	if dev.Platform == model.IOS && r.ios == nil {
		rd.Status, rd.Detail = model.RunSkipped, "no iOS screen controller configured"
		return
	}
	art := run.Spec.Artifacts[dev.Platform]
	if art == "" {
		rd.Status, rd.Detail = model.RunSkipped, "no "+string(dev.Platform)+" artifact"
		return
	}

	_ = r.st.SetDeviceStatus(dev.ID, model.StatusRunningTest, time.Now())
	defer r.st.SetDeviceStatus(dev.ID, model.StatusReserved, time.Now())

	_ = r.cap.ClearLogs(ctx, dev)

	res, ierr := r.inst.Run(ctx, dev, art, runOrigin(run))
	rd.Package = res.Package
	if ierr != nil {
		rd.Status, rd.Detail = model.RunError, "install failed: "+ierr.Error()
		r.saveLog(ctx, dev, rd, devDir)
		return
	}

	watch := run.Spec.WatchSeconds
	if watch <= 0 {
		watch = 8
	}
	select {
	case <-ctx.Done():
		rd.Status, rd.Detail = model.RunCanceled, "canceled"
		return
	case <-time.After(time.Duration(watch) * time.Second):
	}

	if img, mime, err := r.cap.Screenshot(ctx, dev); err == nil {
		name := "screenshot.png"
		if mime == "image/jpeg" {
			name = "screenshot.jpg"
		}
		if os.WriteFile(filepath.Join(devDir, name), img, 0o644) == nil {
			rd.Artifacts = append(rd.Artifacts, name)
		}
	}
	logText := r.saveLog(ctx, dev, rd, devDir)

	if dev.Platform == model.IOS {
		r.smokeVerdictIOS(dev, res, rd, watch)
		return
	}

	alive, _ := r.adb.Running(ctx, dev.Serial, res.Package)
	crash := scanCrash(logText, res.Package)
	switch {
	case crash != "":
		rd.Status, rd.Detail = model.RunFailed, "crash in log: "+crash
	case !alive:
		rd.Status, rd.Detail = model.RunFailed, fmt.Sprintf("process not running %ds after launch", watch)
	default:
		rd.Status, rd.Detail = model.RunPassed, ""
	}
}

// smokeVerdictIOS asserts the just-installed app is the foreground process
// with a live pid, via WDA's activeAppInfo — the closest iOS equivalent of
// Android's pidof + logcat crash scan available without a jailbreak.
func (r *Runner) smokeVerdictIOS(dev model.Device, res install.Result, rd *model.RunDevice, watch int) {
	if res.Package == "" {
		rd.Status, rd.Detail = model.RunError, "could not read the bundle id from the ipa"
		return
	}
	active, pid, err := r.ios.ActiveApp(dev.ID)
	switch {
	case err != nil:
		rd.Status, rd.Detail = model.RunError, "could not query WDA: "+err.Error()
	case active != res.Package:
		rd.Status, rd.Detail = model.RunFailed, fmt.Sprintf(
			"foreground app is %q, not %q, %ds after launch (crashed back to the springboard?)",
			active, res.Package, watch)
	case pid == 0:
		rd.Status, rd.Detail = model.RunFailed, fmt.Sprintf("%s has no running process %ds after launch", res.Package, watch)
	default:
		rd.Status, rd.Detail = model.RunPassed, ""
	}
}

func (r *Runner) saveLog(ctx context.Context, dev model.Device, rd *model.RunDevice, devDir string) string {
	text, err := r.cap.Logcat(ctx, dev)
	if err != nil || text == "" {
		return text
	}
	if os.WriteFile(filepath.Join(devDir, "logcat.txt"), []byte(text), 0o644) == nil {
		rd.Artifacts = append(rd.Artifacts, "logcat.txt")
	}
	return text
}

func newID() string {
	b := make([]byte, 4)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// runOrigin tags a run's installs in the build library with who started the
// run and which run it was.
func runOrigin(run model.Run) install.Origin {
	return install.Origin{User: run.User, Via: "run", Ref: run.ID}
}
