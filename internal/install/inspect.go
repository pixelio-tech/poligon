package install

import (
	"archive/zip"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/shogo82148/androidbinary/apk"
)

// Meta is what can be read out of a build file without a device.
type Meta struct {
	Package   string
	AppName   string
	Version   string
	BuildCode string
	MinOS     string
}

// Inspect reads an app build's identity: an .apk's manifest, the base split
// of an .apks, an .ipa's Info.plist. An .aab's manifest is protobuf that only
// bundletool reads; it comes back empty and the install result fills it in.
func Inspect(path string) (Meta, error) {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".apk":
		return inspectAPK(path)
	case ".apks":
		return inspectAPKS(path)
	case ".ipa":
		return inspectIPA(path)
	case ".aab":
		return Meta{}, nil
	}
	return Meta{}, fmt.Errorf("unknown build format %q", filepath.Ext(path))
}

func inspectAPK(path string) (Meta, error) {
	k, err := apk.OpenFile(path)
	if err != nil {
		return Meta{}, err
	}
	defer k.Close()
	m := k.Manifest()
	var out Meta
	out.Package, _ = m.Package.String()
	out.Version, _ = m.VersionName.String()
	if v, err := m.VersionCode.Int32(); err == nil && v != 0 {
		out.BuildCode = strconv.Itoa(int(v))
	}
	if v, err := m.SDK.Min.Int32(); err == nil && v != 0 {
		out.MinOS = "API " + strconv.Itoa(int(v))
	}
	out.AppName, _ = k.Label(nil)
	return out, nil
}

// inspectAPKS reads the base split out of a bundletool .apks set.
func inspectAPKS(path string) (Meta, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Meta{}, err
	}
	var member string
	for _, f := range zr.File {
		n := f.Name
		if strings.HasSuffix(n, ".apk") && (strings.Contains(n, "base-master") || n == "universal.apk") {
			member = n
			break
		}
	}
	zr.Close()
	if member == "" {
		return Meta{}, fmt.Errorf("no base apk in %s", filepath.Base(path))
	}
	tmp, err := os.CreateTemp("", "base-*.apk")
	if err != nil {
		return Meta{}, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := unzipOne(path, member, tmp.Name()); err != nil {
		return Meta{}, err
	}
	return inspectAPK(tmp.Name())
}

// inspectIPA reads Payload/<App>.app/Info.plist. plutil turns a binary plist
// into JSON; the host is always a Mac, so it is always there.
func inspectIPA(path string) (Meta, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return Meta{}, err
	}
	var member string
	for _, f := range zr.File {
		parts := strings.Split(f.Name, "/")
		if len(parts) == 3 && parts[0] == "Payload" && strings.HasSuffix(parts[1], ".app") && parts[2] == "Info.plist" {
			member = f.Name
			break
		}
	}
	zr.Close()
	if member == "" {
		return Meta{}, fmt.Errorf("no Info.plist in %s", filepath.Base(path))
	}
	tmp, err := os.CreateTemp("", "Info-*.plist")
	if err != nil {
		return Meta{}, err
	}
	tmp.Close()
	defer os.Remove(tmp.Name())
	if err := unzipOne(path, member, tmp.Name()); err != nil {
		return Meta{}, err
	}
	raw, err := exec.Command("plutil", "-convert", "json", "-o", "-", tmp.Name()).Output()
	if err != nil {
		// no plutil (not a Mac): at least the bundle id
		return Meta{Package: bundleID(tmp.Name())}, nil
	}
	var p map[string]any
	if err := json.Unmarshal(raw, &p); err != nil {
		return Meta{}, err
	}
	str := func(k string) string { s, _ := p[k].(string); return s }
	out := Meta{
		Package:   str("CFBundleIdentifier"),
		AppName:   str("CFBundleDisplayName"),
		Version:   str("CFBundleShortVersionString"),
		BuildCode: str("CFBundleVersion"),
		MinOS:     str("MinimumOSVersion"),
	}
	if out.AppName == "" {
		out.AppName = str("CFBundleName")
	}
	if out.MinOS != "" {
		out.MinOS = "iOS " + out.MinOS
	}
	return out, nil
}
