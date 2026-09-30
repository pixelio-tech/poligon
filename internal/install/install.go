// Package install turns an uploaded build artifact into an app running on a
// device. Android: apk direct, aab via bundletool. iOS: re-sign the ipa with
// the farm's ad-hoc profiles, then deploy.
package install

import (
	"archive/zip"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/shogo82148/androidbinary/apk"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/ios"
	"github.com/pancir/poligon/internal/model"
)

// Options configures an install run.
type Options struct {
	// BundletoolJar is the path to bundletool for .aab expansion.
	BundletoolJar string
	// SigningIdentity is the codesign identity for iOS re-signing: a SHA-1,
	// a name ("Apple Development: Jane Doe (ABCDE12345)") or part of one.
	SigningIdentity string
	// SigningIdentity may be empty: the keychain's only valid identity is used.
	// ProfileDir holds farm *.mobileprovision files (exact bundle id or team
	// wildcard); ExtraProfileDirs are searched too — normally Xcode's own
	// profile folders, where automatic signing keeps the team wildcard profile.
	ProfileDir       string
	ExtraProfileDirs []string
	// WorkDir is a scratch directory for expansion / re-signing.
	WorkDir string
}

// Installer performs installs.
type Installer struct {
	adb  *adb.ADB
	ios  ios.Tools
	opts Options
	rec  Recorder
}

// Origin says who asked for an install and through which door.
type Origin struct {
	User string
	Via  string // web | batch | mcp | run
	Ref  string // run id or batch id, when there is one
	// Name is the file name the user uploaded, when the stored path differs.
	Name string
	URL  string // where the build was fetched from, if it came from a URL
}

// Recorder keeps every build that reaches a phone and every install of it.
// Add runs before the install, so a build that fails to install is kept too.
type Recorder interface {
	Add(path string, o Origin) (buildID int64, err error)
	Installed(buildID int64, dev model.Device, o Origin, res Result, err error)
}

// SetRecorder makes every Run keep the build and log the install.
func (in *Installer) SetRecorder(r Recorder) { in.rec = r }

// New builds an Installer.
func New(a *adb.ADB, it ios.Tools, opts Options) *Installer {
	return &Installer{adb: a, ios: it, opts: opts}
}

// Result reports what happened.
type Result struct {
	Package string
	Version string
	Output  string
}

// Run installs artifactPath (as uploaded, keeping its extension) onto dev,
// then launches the app. With a Recorder set the build is kept in the build
// library and the install logged, whatever the outcome.
func (in *Installer) Run(ctx context.Context, dev model.Device, artifactPath string, o Origin) (Result, error) {
	if in.rec == nil {
		return in.run(ctx, dev, artifactPath)
	}
	id, rerr := in.rec.Add(artifactPath, o)
	res, err := in.run(ctx, dev, artifactPath)
	if rerr == nil {
		in.rec.Installed(id, dev, o, res, err)
	}
	return res, err
}

func (in *Installer) run(ctx context.Context, dev model.Device, artifactPath string) (Result, error) {
	ext := strings.ToLower(filepath.Ext(artifactPath))
	switch {
	case dev.Platform == model.Android && ext == ".apk":
		pkg, ver := apkInfo(artifactPath)
		out, err := in.adb.Install(ctx, dev.Serial, artifactPath, true, true)
		if err == nil && pkg != "" {
			_ = in.adb.Launch(ctx, dev.Serial, pkg)
		}
		return Result{Output: out, Package: pkg, Version: ver}, err

	case dev.Platform == model.Android && (ext == ".aab" || ext == ".apks"):
		apks, err := in.expandAAB(ctx, artifactPath)
		if err != nil {
			return Result{}, err
		}
		pkg, ver := apkInfo(apks[0])
		out, err := in.adb.InstallMultiple(ctx, dev.Serial, apks, true)
		if err == nil && pkg != "" {
			_ = in.adb.Launch(ctx, dev.Serial, pkg)
		}
		return Result{Output: out, Package: pkg, Version: ver}, err

	case dev.Platform == model.IOS && ext == ".ipa":
		appBundle, err := in.resignIPA(ctx, artifactPath, dev.UDID)
		if err != nil {
			return Result{}, err
		}
		pkg := bundleID(filepath.Join(appBundle, "Info.plist"))
		out, err := in.ios.Install(ctx, dev.UDID, dev.Specs.OSVersion, pkg, appBundle)
		return Result{Output: out, Package: pkg}, err

	default:
		return Result{}, fmt.Errorf("cannot install %s on %s device", ext, dev.Platform)
	}
}

// expandAAB produces a universal split set for the connected device.
func (in *Installer) expandAAB(ctx context.Context, aab string) ([]string, error) {
	if in.opts.BundletoolJar == "" {
		return nil, fmt.Errorf("bundletool jar not configured")
	}
	outDir, err := os.MkdirTemp(in.opts.WorkDir, "aab-*")
	if err != nil {
		return nil, err
	}
	apks := filepath.Join(outDir, "out.apks")
	cmd := exec.CommandContext(ctx, "java", "-jar", in.opts.BundletoolJar,
		"build-apks", "--mode=universal", "--bundle="+aab, "--output="+apks)
	if b, err := cmd.CombinedOutput(); err != nil {
		return nil, fmt.Errorf("bundletool: %v: %s", err, b)
	}
	// universal.apk lives inside the .apks zip
	universal := filepath.Join(outDir, "universal.apk")
	if err := unzipOne(apks, "universal.apk", universal); err != nil {
		return nil, err
	}
	return []string{universal}, nil
}

// resignIPA re-signs an ipa with the farm identity + provisioning profiles and
// returns the path to the extracted, re-signed .app bundle for ios-deploy.
//
// Uses codesign directly (no fastlane/ruby). ProfileDir must contain, for every
// signable bundle (the app and each .appex/.framework with its own id), either
// <bundle-id>.mobileprovision or a wildcard profile (app-id ending ".*") whose
// team matches. Frameworks are signed before the app, extensions before the app.
func (in *Installer) resignIPA(ctx context.Context, ipa, udid string) (string, error) {
	ident, err := ResolveIdentity(ctx, in.opts.SigningIdentity)
	if err != nil {
		return "", fmt.Errorf("iOS re-signing: %w", err)
	}
	dirs := append([]string{in.opts.ProfileDir}, in.opts.ExtraProfileDirs...)
	profiles, err := loadProfiles(dirs, ident.Hash, udid, time.Now())
	if err != nil {
		return "", fmt.Errorf("iOS re-signing with %s: %w", ident.Name, err)
	}
	work, err := os.MkdirTemp(in.opts.WorkDir, "resign-*")
	if err != nil {
		return "", err
	}
	if err := unzipDir(ipa, "Payload/", work); err != nil {
		return "", err
	}
	appDir := ""
	if entries, _ := os.ReadDir(filepath.Join(work, "Payload")); true {
		for _, e := range entries {
			if strings.HasSuffix(e.Name(), ".app") {
				appDir = filepath.Join(work, "Payload", e.Name())
			}
		}
	}
	if appDir == "" {
		return "", fmt.Errorf("no .app in ipa")
	}

	// sign deepest-first: frameworks, plugins, then the app
	var targets []string
	for _, sub := range []string{"Frameworks", "PlugIns"} {
		d := filepath.Join(appDir, sub)
		entries, _ := os.ReadDir(d)
		for _, e := range entries {
			if e.IsDir() && (strings.HasSuffix(e.Name(), ".framework") ||
				strings.HasSuffix(e.Name(), ".appex") || strings.HasSuffix(e.Name(), ".dylib")) {
				targets = append(targets, filepath.Join(d, e.Name()))
			}
		}
	}
	targets = append(targets, appDir)

	for _, t := range targets {
		if err := in.codesignBundle(ctx, t, ident.Hash, profiles, work); err != nil {
			return "", err
		}
	}
	return appDir, nil
}

// codesignBundle embeds the right profile (for bundles that need one) and
// re-signs with the farm identity + that profile's entitlements.
func (in *Installer) codesignBundle(ctx context.Context, bundle, identity string, profiles []profile, work string) error {
	id := bundleID(filepath.Join(bundle, "Info.plist"))

	args := []string{"-f", "-s", identity}
	isFramework := strings.HasSuffix(bundle, ".framework") || strings.HasSuffix(bundle, ".dylib")
	if !isFramework {
		p, ok := matchProfile(profiles, id)
		if !ok {
			return fmt.Errorf("no provisioning profile for bundle id %q (need an exact one or the team wildcard)", id)
		}
		if err := copyFile(p.path, filepath.Join(bundle, "embedded.mobileprovision")); err != nil {
			return err
		}
		ents := filepath.Join(work, strings.ReplaceAll(id, "/", "_")+".plist")
		if err := os.WriteFile(ents, p.entitlements, 0o644); err != nil {
			return err
		}
		args = append(args, "--entitlements", ents)
	}
	args = append(args, bundle)

	if b, err := exec.CommandContext(ctx, "codesign", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("codesign %s: %v: %s", filepath.Base(bundle), err, strings.TrimSpace(string(b)))
	}
	return nil
}

// matchProfile prefers an exact app-id match, else a team wildcard.
func matchProfile(profiles []profile, bundleID string) (profile, bool) {
	var wildcard *profile
	for i := range profiles {
		p := &profiles[i]
		id := p.appID
		if i := strings.IndexByte(id, '.'); i >= 0 {
			id = id[i+1:] // strip TEAMID.
		}
		if id == bundleID {
			return *p, true
		}
		if id == "*" {
			wildcard = p
		}
	}
	if wildcard != nil {
		return *wildcard, true
	}
	return profile{}, false
}

// extractEntitlements pulls the <key>Entitlements</key><dict>…</dict> block from
// a decoded mobileprovision into a standalone plist.
func extractEntitlements(xml []byte) []byte {
	s := string(xml)
	i := strings.Index(s, "<key>Entitlements</key>")
	if i < 0 {
		return []byte(`<?xml version="1.0" encoding="UTF-8"?><!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd"><plist version="1.0"><dict/></plist>`)
	}
	rest := s[i:]
	open := strings.Index(rest, "<dict>")
	if open < 0 {
		return nil
	}
	depth, end := 0, -1
	for j := open; j < len(rest); {
		if strings.HasPrefix(rest[j:], "<dict>") {
			depth++
			j += 6
		} else if strings.HasPrefix(rest[j:], "</dict>") {
			depth--
			j += 7
			if depth == 0 {
				end = j
				break
			}
		} else {
			j++
		}
	}
	if end < 0 {
		return nil
	}
	return []byte(`<?xml version="1.0" encoding="UTF-8"?>` +
		`<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">` +
		`<plist version="1.0">` + rest[open:end] + `</plist>`)
}

// bundleID reads CFBundleIdentifier from an Info.plist (binary or xml).
func bundleID(infoPlist string) string {
	b, err := os.ReadFile(infoPlist)
	if err != nil {
		return ""
	}
	if id := plistString(b, "CFBundleIdentifier"); id != "" {
		return id
	}
	// binary plist: fall back to PlistBuddy
	out, _ := exec.Command("/usr/libexec/PlistBuddy", "-c", "Print :CFBundleIdentifier", infoPlist).Output()
	return strings.TrimSpace(string(out))
}

// plistString does a crude extraction of <key>NAME</key><string>VALUE</string>
// from a binary or xml plist. Good enough for CFBundleIdentifier; swap for a
// real plist parser if this proves fragile.
func plistString(data []byte, key string) string {
	s := string(data)
	i := strings.Index(s, key)
	if i < 0 {
		return ""
	}
	rest := s[i+len(key):]
	open := strings.Index(rest, "<string>")
	if open < 0 {
		return ""
	}
	rest = rest[open+len("<string>"):]
	end := strings.Index(rest, "</string>")
	if end < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:end])
}

func unzipOne(zipPath, member, dst string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if f.Name != member {
			continue
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		defer rc.Close()
		out, err := os.Create(dst)
		if err != nil {
			return err
		}
		defer out.Close()
		_, err = out.ReadFrom(rc)
		return err
	}
	return fmt.Errorf("%s not found in %s", member, zipPath)
}

func unzipDir(zipPath, prefix, dstRoot string) error {
	zr, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer zr.Close()
	for _, f := range zr.File {
		if !strings.HasPrefix(f.Name, prefix) {
			continue
		}
		target := filepath.Join(dstRoot, filepath.Clean(f.Name))
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(target, 0o755); err != nil {
				return err
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(target, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, f.Mode())
		if err != nil {
			rc.Close()
			return err
		}
		_, err = out.ReadFrom(rc)
		out.Close()
		rc.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	b, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	return os.WriteFile(dst, b, 0o644)
}

// apkInfo reads the package name and version name from an APK's manifest.
// Returns empty strings if the APK cannot be parsed.
func apkInfo(path string) (pkg, version string) {
	pkgApk, err := apk.OpenFile(path)
	if err != nil {
		return "", ""
	}
	defer pkgApk.Close()
	m := pkgApk.Manifest()
	pkg, _ = m.Package.String()
	version, _ = m.VersionName.String()
	return pkg, version
}
