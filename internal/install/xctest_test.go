package install

import (
	"archive/zip"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestXcodeVersions(t *testing.T) {
	for in, want := range map[string]int{"1640": 16, "2700": 27, "0940": 9, "": 0, "x1": 0} {
		if got := XcodeMajor(in); got != want {
			t.Errorf("XcodeMajor(%q) = %d, want %d", in, got, want)
		}
	}
	for in, want := range map[string]string{"1640": "16.4", "2700": "27.0", "0940": "9.4"} {
		if got := XcodeName(in); got != want {
			t.Errorf("XcodeName(%q) = %q, want %q", in, got, want)
		}
	}
}

// the .xctestrun Xcode 16.4 wrote for a Flutter project, cut to what matters
const xctestrunV1 = `<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
<key>RunnerTests</key><dict>
  <key>IsAppHostedTestBundle</key><true/>
  <key>TestBundlePath</key><string>__TESTHOST__/PlugIns/RunnerTests.xctest</string>
  <key>TestHostBundleIdentifier</key><string>dev.poligon.itestDemo</string>
  <key>TestHostPath</key><string>__TESTROOT__/Release-iphoneos/Runner.app</string>
</dict>
<key>__xctestrun_metadata__</key><dict><key>FormatVersion</key><integer>1</integer></dict>
</dict></plist>`

func TestFindXCTestRun(t *testing.T) {
	root := t.TempDir()
	products := filepath.Join(root, "Products")
	app := filepath.Join(products, "Release-iphoneos", "Runner.app")
	xct := filepath.Join(app, "PlugIns", "RunnerTests.xctest")
	for _, d := range []string{xct} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	plist := func(id string) string {
		return `<?xml version="1.0" encoding="UTF-8"?><plist version="1.0"><dict><key>CFBundleIdentifier</key><string>` + id +
			`</string><key>DTXcode</key><string>1640</string></dict></plist>`
	}
	os.WriteFile(filepath.Join(app, "Info.plist"), []byte(plist("dev.poligon.itestDemo")), 0o644)
	os.WriteFile(filepath.Join(xct, "Info.plist"), []byte(plist("dev.poligon.itestDemo.RunnerTests")), 0o644)
	os.WriteFile(filepath.Join(products, "Runner_iphoneos18.5-arm64.xctestrun"), []byte(xctestrunV1), 0o644)

	tb, err := findXCTestRun(root)
	if err != nil {
		t.Fatal(err)
	}
	if tb.Target != "RunnerTests" || tb.HostApp != app || tb.TestBundle != xct ||
		tb.HostBundleID != "dev.poligon.itestDemo" || tb.TestBundleID != "dev.poligon.itestDemo.RunnerTests" ||
		tb.UITest || tb.XcodeBuilt != "1640" {
		t.Fatalf("%+v", tb)
	}
}

func TestFindXCTestRunNeedsTheProducts(t *testing.T) {
	root := t.TempDir()
	os.WriteFile(filepath.Join(root, "x.xctestrun"), []byte(xctestrunV1), 0o644)
	if _, err := findXCTestRun(root); err == nil || !strings.Contains(err.Error(), "Build/Products") {
		t.Fatalf("want a hint to zip Build/Products, got %v", err)
	}
	if _, err := findXCTestRun(t.TempDir()); err == nil || !strings.Contains(err.Error(), "no .xctestrun") {
		t.Fatalf("got %v", err)
	}
}

func TestUnzipStaysInside(t *testing.T) {
	dir := t.TempDir()
	zp := filepath.Join(dir, "evil.zip")
	f, _ := os.Create(zp)
	zw := zip.NewWriter(f)
	w, _ := zw.Create("../escape.txt")
	w.Write([]byte("x"))
	zw.Close()
	f.Close()
	// a ../ entry is pinned inside the target dir, never written above it
	_ = unzipDir(zp, "", filepath.Join(dir, "out"))
	if _, err := os.Stat(filepath.Join(dir, "escape.txt")); err == nil {
		t.Fatal("file written outside the target dir")
	}
}
