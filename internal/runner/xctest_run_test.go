package runner

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/pancir/poligon/internal/model"
)

// trimmed from a real xcodebuild run on the farm (iPhone 8, Flutter 3.47)
const xcodebuildLog = `2026-09-30 16:50:37.841744+0500 Runner[56612:6735174] flutter: 00:00 +0: counter increments
2026-09-30 16:50:40.564121+0500 Runner[56612:6735174] flutter: 00:02 +1: deliberately fails
2026-09-30 16:50:40.581412+0500 Runner[56612:6735174] flutter: Expected: exactly one matching candidate
2026-09-30 16:50:40.581684+0500 Runner[56612:6735174] flutter:   Actual: _TextWidgetFinder:<Found 0 widgets with text "42": []>
2026-09-30 16:50:40.582018+0500 Runner[56612:6735174] flutter:    Which: means none were found but one was expected
2026-09-30 16:50:40.582131+0500 Runner[56612:6735174] flutter: 
2026-09-30 16:50:40.582927+0500 Runner[56612:6735174] flutter: #0      fail (package:matcher/src/expect/expect.dart:187)
2026-09-30 16:50:40.583558+0500 Runner[56612:6735174] flutter: <asynchronous suspension>
2026-09-30 16:50:40.584784+0500 Runner[56612:6735174] flutter: 00:02 +1: deliberately fails [E]
2026-09-30 16:50:40.584957+0500 Runner[56612:6735174] flutter:   Test failed. See exception logs above.
2026-09-30 16:50:40.585446+0500 Runner[56612:6735174] flutter: 00:02 +1 -1: (tearDownAll)
2026-09-30 16:50:40.587212+0500 Runner[56612:6735174] flutter: 00:02 +2 -1: Some tests failed.
`

func TestIOSVerdictQuotesFlutterFailure(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "xcodebuild.log")
	if err := os.WriteFile(logPath, []byte(xcodebuildLog), 0o644); err != nil {
		t.Fatal(err)
	}
	var s xcSummary
	_ = json.Unmarshal([]byte(`{"result":"Failed","totalTestCount":2,"passedTests":1,"failedTests":1,
		"testFailures":[{"testIdentifierString":"RunnerTests/testDeliberatelyFails","testName":"testDeliberatelyFails",
		"failureText":"((success) is true) failed - Instance of 'FlutterErrorDetails'"}]}`), &s)
	st, detail := iosVerdict(s, logPath)
	if st != model.RunFailed {
		t.Fatalf("status %s", st)
	}
	for _, want := range []string{"1 passed, 1 failed", "RunnerTests/testDeliberatelyFails", "Expected: exactly one matching candidate", `Found 0 widgets with text "42"`} {
		if !strings.Contains(detail, want) {
			t.Errorf("detail lacks %q:\n%s", want, detail)
		}
	}
	if strings.Contains(detail, "FlutterErrorDetails") || strings.Contains(detail, "expect.dart") {
		t.Errorf("detail should quote the matcher, not the XCTest wrapper or the stack:\n%s", detail)
	}
}

func TestIOSVerdictPassed(t *testing.T) {
	st, detail := iosVerdict(xcSummary{Result: "Passed", TotalTestCount: 3, PassedTests: 3}, "/nonexistent")
	if st != model.RunPassed || detail != "3 passed, 0 failed" {
		t.Fatalf("%s %q", st, detail)
	}
}

func TestFlutterTestKey(t *testing.T) {
	if a, b := flutterTestKey("testDeliberatelyFails"), flutterTestKey("deliberately fails"); a != b {
		t.Fatalf("%q != %q", a, b)
	}
}

func TestXcodebuildWhy(t *testing.T) {
	out := "noise\nTesting failed:\n\tRunner (56530) encountered an error (Test runner never began executing tests after launching)\n** TEST EXECUTE FAILED **\n"
	if why := xcodebuildWhy(out); !strings.Contains(why, "never began executing tests") {
		t.Fatalf("%q", why)
	}
}
