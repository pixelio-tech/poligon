package runner

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/shogo82148/androidbinary/apk"

	"github.com/pancir/poligon/internal/model"
)

// runIntegrationTest: install the app under test, install its separately
// built androidTest apk (the one containing the Flutter integration_test
// suite), then run the instrumentation the way `flutter drive`/`gradlew
// connectedAndroidTest` would — the farm doesn't build, so both apks arrive
// pre-built as run artifacts.
//
// iOS takes an XCTest build instead — see runIntegrationTestIOS.
func (r *Runner) runIntegrationTest(ctx context.Context, run model.Run, dev model.Device, rd *model.RunDevice, devDir string) {
	if dev.Platform == model.IOS {
		r.runIntegrationTestIOS(ctx, run, dev, rd, devDir)
		return
	}
	appArt := run.Spec.Artifacts[dev.Platform]
	testArt := run.Spec.TestArtifacts[dev.Platform]
	if appArt == "" || testArt == "" {
		rd.Status, rd.Detail = model.RunSkipped, "integration_test needs both an app artifact and a test artifact for android"
		return
	}
	testPkg, testRunner, err := androidTestInfo(testArt)
	if err != nil {
		rd.Status, rd.Detail = model.RunError, "test apk: "+err.Error()
		return
	}

	_ = r.st.SetDeviceStatus(dev.ID, model.StatusRunningTest, time.Now())
	defer r.st.SetDeviceStatus(dev.ID, model.StatusReserved, time.Now())

	_ = r.cap.ClearLogs(ctx, dev)

	res, ierr := r.inst.Run(ctx, dev, appArt, runOrigin(run))
	rd.Package = res.Package
	if ierr != nil {
		rd.Status, rd.Detail = model.RunError, "app install failed: "+ierr.Error()
		r.saveLog(ctx, dev, rd, devDir)
		return
	}
	if _, ierr := r.adb.Install(ctx, dev.Serial, testArt, true, true); ierr != nil {
		rd.Status, rd.Detail = model.RunError, "test apk install failed: "+ierr.Error()
		r.saveLog(ctx, dev, rd, devDir)
		return
	}

	out, ierr := r.adb.Instrument(ctx, dev.Serial, testPkg, testRunner)
	if os.WriteFile(filepath.Join(devDir, "instrument.log"), []byte(out), 0o644) == nil {
		rd.Artifacts = append(rd.Artifacts, "instrument.log")
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
	r.saveLog(ctx, dev, rd, devDir)

	switch {
	case ctx.Err() != nil:
		rd.Status, rd.Detail = model.RunCanceled, "canceled or timed out"
	case ierr != nil:
		rd.Status, rd.Detail = model.RunError, "instrumentation did not run: "+ierr.Error()
	case strings.Contains(out, "OK ("):
		rd.Status, rd.Detail = model.RunPassed, ""
	case strings.Contains(out, "FAILURES!!!"):
		rd.Status, rd.Detail = model.RunFailed, tail(out, 800)
	default:
		rd.Status, rd.Detail = model.RunError, "instrumentation ended without a clear result — see instrument.log:\n"+tail(out, 400)
	}
}

// androidTestInfo reads the package name and instrumentation runner class
// out of an androidTest apk's manifest — exactly what `am instrument -w
// <package>/<runner>` needs, and what `flutter drive`/gradle would otherwise
// look up for you.
func androidTestInfo(path string) (pkg, runnerClass string, err error) {
	f, err := apk.OpenFile(path)
	if err != nil {
		return "", "", err
	}
	defer f.Close()
	m := f.Manifest()
	pkg, _ = m.Package.String()
	runnerClass, _ = m.Instrument.Name.String()
	if pkg == "" {
		return "", "", fmt.Errorf("could not read the package from the manifest")
	}
	if runnerClass == "" {
		return "", "", fmt.Errorf("manifest has no <instrumentation> element — is this the androidTest apk, not the app apk?")
	}
	return pkg, runnerClass, nil
}
