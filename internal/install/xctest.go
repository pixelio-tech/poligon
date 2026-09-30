package install

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
)

// IOSTestBuild is an extracted, re-signed `xcodebuild build-for-testing`
// output: the .xctestrun file and the bundles it points at.
type IOSTestBuild struct {
	Dir          string // scratch dir holding the extracted build; remove when done
	XCTestRun    string // path to the .xctestrun
	Target       string // test target name, e.g. RunnerTests
	HostApp      string // the app the tests run inside (Runner.app)
	HostBundleID string
	TestBundle   string // the .xctest bundle
	TestBundleID string
	UITest       bool     // an XCUITest bundle (drives TargetApps) rather than tests hosted in HostApp
	TargetApps   []string // apps a UI test drives, to install alongside
	// XcodeBuilt is the Xcode that built it, as Info.plist's DTXcode ("1640"
	// is Xcode 16.4); "" if not recorded.
	XcodeBuilt string
}

// XcodeMajor turns a DTXcode value ("1640", "2700") into its major version.
func XcodeMajor(dtXcode string) int {
	if len(dtXcode) < 3 {
		return 0
	}
	n := 0
	for _, c := range dtXcode[:len(dtXcode)-2] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// XcodeName renders a DTXcode value as a version ("2700" -> "27.0").
func XcodeName(dtXcode string) string {
	if len(dtXcode) < 3 {
		return dtXcode
	}
	return strings.TrimLeft(dtXcode[:len(dtXcode)-2], "0") + "." + dtXcode[len(dtXcode)-2:len(dtXcode)-1]
}

// UninstallIOS removes an app from an iPhone, ignoring "not installed".
func (in *Installer) UninstallIOS(ctx context.Context, udid, osVersion, bundleID string) {
	_, _ = in.ios.Uninstall(ctx, udid, osVersion, bundleID)
}

// PrepareIOSTests unpacks a zip of `xcodebuild build-for-testing` products
// (the .xctestrun plus the Release-iphoneos/ or Debug-iphoneos/ folder next to
// it — the Build/Products directory, zipped) and re-signs every app in it for
// the device udid. Signing on the build machine is not needed: the farm
// signs with its own identity, the same as for an .ipa.
func (in *Installer) PrepareIOSTests(ctx context.Context, zipPath, udid string) (IOSTestBuild, error) {
	work, err := os.MkdirTemp(in.opts.WorkDir, "xctest-*")
	if err != nil {
		return IOSTestBuild{}, err
	}
	if err := unzipDir(zipPath, "", work); err != nil {
		os.RemoveAll(work)
		return IOSTestBuild{}, fmt.Errorf("unzip test build: %w", err)
	}
	tb, err := findXCTestRun(work)
	if err != nil {
		os.RemoveAll(work)
		return IOSTestBuild{}, err
	}
	tb.Dir = work
	apps := append([]string{tb.HostApp}, tb.TargetApps...)
	for _, app := range apps {
		if err := in.resignApp(ctx, app, udid, work); err != nil {
			os.RemoveAll(work)
			return IOSTestBuild{}, err
		}
	}
	return tb, nil
}

// findXCTestRun locates the one .xctestrun under root and resolves its first
// test target's bundles.
func findXCTestRun(root string) (IOSTestBuild, error) {
	var runs []string
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() && (strings.HasSuffix(p, ".app") || strings.HasSuffix(p, ".xctest") || strings.HasPrefix(d.Name(), "__MACOSX")) {
			return filepath.SkipDir
		}
		if !d.IsDir() && strings.HasSuffix(p, ".xctestrun") {
			runs = append(runs, p)
		}
		return nil
	})
	if len(runs) == 0 {
		return IOSTestBuild{}, fmt.Errorf("no .xctestrun in the test build — zip the Build/Products folder that `xcodebuild build-for-testing` wrote")
	}
	sort.Strings(runs)
	if len(runs) > 1 {
		// one per SDK/arch; take the iphoneos one if there are several
		for _, r := range runs {
			if strings.Contains(filepath.Base(r), "iphoneos") {
				runs[0] = r
				break
			}
		}
	}
	raw, err := exec.Command("plutil", "-convert", "json", "-o", "-", runs[0]).Output()
	if err != nil {
		return IOSTestBuild{}, fmt.Errorf("read %s: %w", filepath.Base(runs[0]), err)
	}
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return IOSTestBuild{}, err
	}
	name, target, err := firstTestTarget(doc)
	if err != nil {
		return IOSTestBuild{}, fmt.Errorf("%s: %w", filepath.Base(runs[0]), err)
	}
	return resolveTarget(runs[0], name, target)
}

// firstTestTarget returns the first test target of an .xctestrun: format 1
// keeps targets as top-level keys, format 2 lists them under
// TestConfigurations[].TestTargets[].
func firstTestTarget(doc map[string]any) (string, map[string]any, error) {
	if confs, ok := doc["TestConfigurations"].([]any); ok {
		for _, c := range confs {
			cm, _ := c.(map[string]any)
			targets, _ := cm["TestTargets"].([]any)
			for _, t := range targets {
				if tm, ok := t.(map[string]any); ok {
					name, _ := tm["BlueprintName"].(string)
					return name, tm, nil
				}
			}
		}
		return "", nil, fmt.Errorf("no test targets")
	}
	keys := make([]string, 0, len(doc))
	for k := range doc {
		if !strings.HasPrefix(k, "__") {
			keys = append(keys, k)
		}
	}
	sort.Strings(keys)
	for _, k := range keys {
		if tm, ok := doc[k].(map[string]any); ok {
			if _, ok := tm["TestBundlePath"]; ok {
				return k, tm, nil
			}
		}
	}
	return "", nil, fmt.Errorf("no test targets")
}

func resolveTarget(xctestrun, name string, t map[string]any) (IOSTestBuild, error) {
	root := filepath.Dir(xctestrun)
	str := func(k string) string { s, _ := t[k].(string); return s }
	hostPath := strings.ReplaceAll(str("TestHostPath"), "__TESTROOT__", root)
	sub := func(p string) string {
		p = strings.ReplaceAll(p, "__TESTROOT__", root)
		return strings.ReplaceAll(p, "__TESTHOST__", hostPath)
	}
	tb := IOSTestBuild{
		XCTestRun:  xctestrun,
		Target:     name,
		HostApp:    hostPath,
		TestBundle: sub(str("TestBundlePath")),
		UITest:     t["IsUITestBundle"] == true,
	}
	if tb.HostApp == "" || tb.TestBundle == "" {
		return IOSTestBuild{}, fmt.Errorf("test target %q has no TestHostPath / TestBundlePath", name)
	}
	if ui := str("UITargetAppPath"); ui != "" {
		tb.TargetApps = append(tb.TargetApps, sub(ui))
	}
	for _, p := range []string{tb.HostApp, tb.TestBundle} {
		if _, err := os.Stat(p); err != nil {
			return IOSTestBuild{}, fmt.Errorf("the test build is missing %s — zip the whole Build/Products folder", strings.TrimPrefix(p, root+"/"))
		}
	}
	tb.HostBundleID = bundleID(filepath.Join(tb.HostApp, "Info.plist"))
	if out, err := exec.Command("plutil", "-extract", "DTXcode", "raw", filepath.Join(tb.HostApp, "Info.plist")).Output(); err == nil {
		tb.XcodeBuilt = strings.TrimSpace(string(out))
	}
	tb.TestBundleID = bundleID(filepath.Join(tb.TestBundle, "Info.plist"))
	if tb.HostBundleID == "" {
		return IOSTestBuild{}, fmt.Errorf("cannot read the bundle id of %s", filepath.Base(tb.HostApp))
	}
	return tb, nil
}
