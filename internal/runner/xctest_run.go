package runner

import (
	"archive/zip"
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/procgroup"
)

// runIntegrationTestIOS runs an XCTest build — for Flutter, the RunnerTests
// target that wraps integration_test — on a real iPhone.
//
// The test artifact is the Build/Products folder of `xcodebuild
// build-for-testing`, zipped: the .xctestrun and the Release-iphoneos (or
// Debug-iphoneos) folder beside it. The app under test lives inside it, so no
// separate .ipa is needed. The farm re-signs it for the phone, installs it
// fresh and runs `xcodebuild test-without-building`, which injects XCTest the
// way Xcode does. go-ios cannot stand in here: it launches an app-hosted test
// bundle without injecting XCTest, and the tests never report.
func (r *Runner) runIntegrationTestIOS(ctx context.Context, run model.Run, dev model.Device, rd *model.RunDevice, devDir string) {
	testArt := run.Spec.TestArtifacts[model.IOS]
	if testArt == "" {
		rd.Status, rd.Detail = model.RunSkipped, "integration_test on iOS needs the test build: the zipped Build/Products folder of `xcodebuild build-for-testing` (test_artifact=…zip)"
		return
	}

	_ = r.st.SetDeviceStatus(dev.ID, model.StatusRunningTest, time.Now())
	defer r.st.SetDeviceStatus(dev.ID, model.StatusReserved, time.Now())

	tb, err := r.inst.PrepareIOSTests(ctx, testArt, dev.UDID)
	if err != nil {
		rd.Status, rd.Detail = model.RunError, "test build: "+err.Error()
		return
	}
	defer os.RemoveAll(tb.Dir) // the extracted, re-signed copy
	rd.Package = tb.HostBundleID

	// A test bundle built by a newer Xcode than the farm's never starts:
	// xcodebuild injects the farm's older XCTest, the bundle does not load,
	// and the run hangs until "Test runner never began executing tests".
	if hostName, hostMajor := hostXcode(ctx); hostMajor > 0 && install.XcodeMajor(tb.XcodeBuilt) > hostMajor {
		rd.Status, rd.Detail = model.RunError, fmt.Sprintf(
			"the test build was made with Xcode %s, the farm has Xcode %s — XCTest from an older Xcode cannot load it. "+
				"Build with Xcode %d (or older), or install a newer Xcode on the farm host.",
			install.XcodeName(tb.XcodeBuilt), hostName, hostMajor)
		return
	}

	_ = r.cap.ClearLogs(ctx, dev)
	// start from a clean install, as `flutter test` does; it also clears a
	// copy signed by someone else, which iOS would refuse to upgrade
	r.inst.UninstallIOS(ctx, dev.UDID, dev.Specs.OSVersion, tb.HostBundleID)

	result := filepath.Join(devDir, "result.xcresult")
	logPath := filepath.Join(devDir, "xcodebuild.log")
	logf, err := os.Create(logPath)
	if err != nil {
		rd.Status, rd.Detail = model.RunError, err.Error()
		return
	}
	rd.Artifacts = append(rd.Artifacts, "xcodebuild.log")
	if r.ios != nil {
		r.ios.BeginXCTest(dev.ID)
	}
	cmd := exec.CommandContext(ctx, "xcodebuild", "test-without-building",
		"-xctestrun", tb.XCTestRun,
		"-destination", "id="+dev.UDID,
		"-resultBundlePath", result)
	procgroup.Bind(cmd)
	var tailBuf tailWriter
	cmd.Stdout = io.MultiWriter(logf, &tailBuf)
	cmd.Stderr = cmd.Stdout
	runErr := cmd.Run()
	logf.Close()
	if r.ios != nil {
		r.ios.EndXCTest(dev.ID)
	}

	sum, sumErr := xcresultSummary(ctx, result)
	if sumErr == nil {
		if b, err := json.MarshalIndent(sum, "", "  "); err == nil && os.WriteFile(filepath.Join(devDir, "test-summary.json"), b, 0o644) == nil {
			rd.Artifacts = append(rd.Artifacts, "test-summary.json")
		}
	}
	if _, err := os.Stat(result); err == nil {
		if zipDir(result, filepath.Join(devDir, "result.xcresult.zip")) == nil {
			rd.Artifacts = append(rd.Artifacts, "result.xcresult.zip")
		}
		_ = os.RemoveAll(result)
	}
	r.screenshotAfterXCTest(ctx, dev, rd, devDir)
	r.saveLog(ctx, dev, rd, devDir)

	switch {
	case ctx.Err() != nil:
		rd.Status, rd.Detail = model.RunCanceled, "canceled or timed out"
	case sumErr == nil && sum.TotalTestCount > 0:
		rd.Status, rd.Detail = iosVerdict(sum, logPath)
	case runErr == nil:
		rd.Status, rd.Detail = model.RunError, "xcodebuild finished but reported no tests — see xcodebuild.log"
	default:
		rd.Status, rd.Detail = model.RunError, "xcodebuild could not run the tests: "+xcodebuildWhy(tailBuf.String())
	}
}

// screenshotAfterXCTest takes the closing screenshot. The test's XCTest
// session usually leaves WebDriverAgent (which takes screenshots and runs the
// live screen) dead, so on a failure the screen is restarted first — which
// also gives the next person a working live screen.
func (r *Runner) screenshotAfterXCTest(ctx context.Context, dev model.Device, rd *model.RunDevice, devDir string) {
	shoot := func() bool {
		sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
		defer cancel()
		img, mime, err := r.cap.Screenshot(sctx, dev)
		if err != nil {
			return false
		}
		name := "screenshot.png"
		if mime == "image/jpeg" {
			name = "screenshot.jpg"
		}
		if os.WriteFile(filepath.Join(devDir, name), img, 0o644) == nil {
			rd.Artifacts = append(rd.Artifacts, name)
		}
		return true
	}
	if shoot() || r.restartScreen == nil || ctx.Err() != nil {
		return
	}
	rctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	if err := r.restartScreen(rctx, dev.ID); err != nil {
		r.log.Warn("integration_test: could not bring the live screen back", "device", dev.ID, "err", err)
		return
	}
	shoot()
}

// xcSummary is the part of `xcresulttool get test-results summary` we use.
type xcSummary struct {
	Result         string `json:"result"`
	TotalTestCount int    `json:"totalTestCount"`
	PassedTests    int    `json:"passedTests"`
	FailedTests    int    `json:"failedTests"`
	SkippedTests   int    `json:"skippedTests"`
	TestFailures   []struct {
		TestIdentifierString string `json:"testIdentifierString"`
		TestName             string `json:"testName"`
		FailureText          string `json:"failureText"`
	} `json:"testFailures"`
}

func xcresultSummary(ctx context.Context, path string) (xcSummary, error) {
	var s xcSummary
	if _, err := os.Stat(path); err != nil {
		return s, err
	}
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, "xcrun", "xcresulttool", "get", "test-results", "summary", "--path", path).Output()
	if err != nil {
		return s, err
	}
	err = json.Unmarshal(out, &s)
	return s, err
}

// iosVerdict turns the summary into a run status. Flutter's RunnerTests only
// says "Instance of 'FlutterErrorDetails'" for a failure; the real message is
// in the app's own output, which xcodebuild captured — quote that.
func iosVerdict(s xcSummary, logPath string) (model.RunStatus, string) {
	counts := fmt.Sprintf("%d passed, %d failed", s.PassedTests, s.FailedTests)
	if s.SkippedTests > 0 {
		counts += fmt.Sprintf(", %d skipped", s.SkippedTests)
	}
	if s.FailedTests == 0 && s.Result != "Failed" {
		return model.RunPassed, counts
	}
	var b strings.Builder
	b.WriteString(counts)
	flutter := flutterFailures(logPath)
	for _, f := range s.TestFailures {
		name := f.TestIdentifierString
		if name == "" {
			name = f.TestName
		}
		fmt.Fprintf(&b, "\n✕ %s", name)
		if msg, ok := flutter[flutterTestKey(f.TestName)]; ok {
			fmt.Fprintf(&b, ": %s", msg)
		} else if f.FailureText != "" {
			fmt.Fprintf(&b, ": %s", f.FailureText)
		}
	}
	return model.RunFailed, tail(b.String(), 1500)
}

// flutter test output lines, as the app printed them through xcodebuild:
//
//	Runner[56612:6735174] flutter: 00:02 +1: deliberately fails
//	Runner[56612:6735174] flutter: Expected: exactly one matching candidate
var flutterLine = regexp.MustCompile(`flutter: (.*)$`)
var flutterTestStart = regexp.MustCompile(`^\d\d:\d\d \+\d+(?: -\d+)?: (.+?)(?: \[E\])?$`)

// flutterFailures maps each failed Flutter test (by flutterTestKey) to the
// first lines of its failure message.
func flutterFailures(logPath string) map[string]string {
	out := map[string]string{}
	f, err := os.Open(logPath)
	if err != nil {
		return out
	}
	defer f.Close()
	var cur string
	var msg []string
	flush := func() {
		if cur != "" && len(msg) > 0 {
			if _, seen := out[flutterTestKey(cur)]; !seen {
				out[flutterTestKey(cur)] = strings.Join(msg, " ")
			}
		}
		msg = nil
	}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		m := flutterLine.FindStringSubmatch(sc.Text())
		if m == nil {
			continue
		}
		line := strings.TrimSpace(m[1])
		if t := flutterTestStart.FindStringSubmatch(line); t != nil {
			if strings.HasSuffix(line, "[E]") {
				flush()
			} else {
				msg = nil
			}
			cur = t[1]
			continue
		}
		// keep the matcher's explanation, not the stack trace
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, "<asynchronous") ||
			strings.HasPrefix(line, "══") || strings.HasPrefix(line, "When the exception") ||
			strings.HasPrefix(line, "The test description") || strings.HasPrefix(line, "Test failed.") {
			continue
		}
		if len(msg) < 4 {
			msg = append(msg, line)
		}
	}
	flush()
	return out
}

// flutterTestKey normalises a test's name so the Flutter description
// ("deliberately fails") and the XCTest method RunnerTests generates from it
// ("testDeliberatelyFails") meet.
func flutterTestKey(name string) string {
	name = strings.TrimPrefix(name, "test")
	var b strings.Builder
	for _, c := range strings.ToLower(name) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
		}
	}
	return b.String()
}

// xcodebuildWhy picks the reason out of a failed xcodebuild's output.
func xcodebuildWhy(out string) string {
	if i := strings.LastIndex(out, "Testing failed:"); i >= 0 {
		return strings.TrimSpace(tail(out[i+len("Testing failed:"):], 600))
	}
	for _, key := range []string{"error:", "xcodebuild: error"} {
		if i := strings.LastIndex(out, key); i >= 0 {
			return strings.TrimSpace(tail(out[i:], 600))
		}
	}
	return tail(out, 600)
}

var hostXcodeOnce struct {
	sync.Once
	name  string
	major int
}

// hostXcode is the farm host's Xcode version ("16.4") and its major number.
func hostXcode(ctx context.Context) (string, int) {
	hostXcodeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		out, err := exec.CommandContext(ctx, "xcodebuild", "-version").Output()
		if err != nil {
			return
		}
		first := strings.SplitN(strings.TrimSpace(string(out)), "\n", 2)[0] // "Xcode 16.4"
		v := strings.TrimSpace(strings.TrimPrefix(first, "Xcode"))
		hostXcodeOnce.name = v
		fmt.Sscanf(v, "%d", &hostXcodeOnce.major)
	})
	return hostXcodeOnce.name, hostXcodeOnce.major
}

// tailWriter keeps the last 64 KiB written to it.
type tailWriter struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (t *tailWriter) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf.Write(p)
	if t.buf.Len() > 64<<10 {
		b := t.buf.Bytes()
		keep := append([]byte(nil), b[len(b)-(48<<10):]...)
		t.buf.Reset()
		t.buf.Write(keep)
	}
	return len(p), nil
}

func (t *tailWriter) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.buf.String()
}

// zipDir zips a directory (an .xcresult bundle) so it downloads as one file.
func zipDir(dir, dst string) error {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	zw := zip.NewWriter(f)
	base := filepath.Dir(dir)
	walkErr := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return err
		}
		w, err := zw.Create(filepath.ToSlash(rel))
		if err != nil {
			return err
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		_, err = io.Copy(w, src)
		src.Close()
		return err
	})
	if err := zw.Close(); err != nil && walkErr == nil {
		walkErr = err
	}
	if err := f.Close(); err != nil && walkErr == nil {
		walkErr = err
	}
	if walkErr != nil {
		os.Remove(dst)
	}
	return walkErr
}
