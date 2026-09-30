package runner

import (
	"archive/zip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/pancir/poligon/internal/model"
)

func writeZip(t *testing.T, files map[string]string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "flows.zip")
	f, err := os.Create(p)
	if err != nil {
		t.Fatal(err)
	}
	zw := zip.NewWriter(f)
	for name, body := range files {
		w, err := zw.Create(name)
		if err != nil {
			t.Fatal(err)
		}
		w.Write([]byte(body))
	}
	zw.Close()
	f.Close()
	return p
}

// A zipped ".maestro" folder: the lone top dir is the root, subflows resolve.
func TestExtractWorkspaceLoneTopDir(t *testing.T) {
	z := writeZip(t, map[string]string{
		".maestro/config.yaml":         "flows:\n  - flows/*\n",
		".maestro/flows/offline.yaml":  "appId: x\n---\n- runFlow: ../subflows/login.yaml\n",
		".maestro/subflows/login.yaml": "appId: x\n---\n- back\n",
		"__MACOSX/.maestro/._config":   "junk",
	})
	if !IsZip(z) {
		t.Fatal("IsZip false for a zip")
	}
	dest := filepath.Join(t.TempDir(), "ws")

	root, err := ExtractWorkspace(z, dest, "")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(root) != ".maestro" {
		t.Fatalf("root = %s, want the .maestro dir", root)
	}
	if _, err := os.Stat(filepath.Join(root, "subflows", "login.yaml")); err != nil {
		t.Fatal("subflow not extracted")
	}

	flow, err := ExtractWorkspace(z, filepath.Join(t.TempDir(), "ws2"), "flows/offline.yaml")
	if err != nil || !strings.HasSuffix(flow, filepath.Join(".maestro", "flows", "offline.yaml")) {
		t.Fatalf("flow_path: %s, %v", flow, err)
	}

	_, err = ExtractWorkspace(z, filepath.Join(t.TempDir(), "ws3"), "flows/nope.yaml")
	if err == nil || !strings.Contains(err.Error(), "flows/offline.yaml") {
		t.Fatalf("missing flow_path should list the flows, got %v", err)
	}
}

func TestExtractWorkspaceRejectsEscapes(t *testing.T) {
	for _, name := range []string{"../evil.yaml", "a/../../evil.yaml"} {
		z := writeZip(t, map[string]string{name: "x"})
		if _, err := ExtractWorkspace(z, filepath.Join(t.TempDir(), "ws"), ""); err == nil {
			t.Fatalf("%s: extracted outside the workspace", name)
		}
	}
	z := writeZip(t, map[string]string{"flows/a.yaml": "appId: x\n"})
	if _, err := ExtractWorkspace(z, filepath.Join(t.TempDir(), "ws"), "../../etc/passwd"); err == nil {
		t.Fatal("flow_path escaped the workspace")
	}
}

func TestExtractWorkspaceNeedsFlows(t *testing.T) {
	z := writeZip(t, map[string]string{"readme.txt": "no flows"})
	if _, err := ExtractWorkspace(z, filepath.Join(t.TempDir(), "ws"), ""); err == nil {
		t.Fatal("workspace without .yaml accepted")
	}
}

func TestIsZipOnYAML(t *testing.T) {
	p := filepath.Join(t.TempDir(), "flow.yaml")
	os.WriteFile(p, []byte("appId: x\n"), 0o644)
	if IsZip(p) {
		t.Fatal("yaml detected as zip")
	}
}

func TestMaestroArgs(t *testing.T) {
	spec := model.RunSpec{
		FlowPath:    "/ws/.maestro",
		Env:         map[string]string{"PHONE": "123", "CODE": "0000"},
		IncludeTags: "offline",
	}
	dev := model.Device{ID: "samsung-1", Platform: model.Android}
	got := maestroArgs(spec, dev, "SERIAL", "/r.xml", "/dbg")
	want := []string{"--device", "SERIAL", "test", "--format", "junit", "--output", "/r.xml",
		"--debug-output", "/dbg",
		"-e", "CODE=0000", "-e", "PHONE=123",
		"-e", "POLIGON_DEVICE_ID=samsung-1", "-e", "POLIGON_PLATFORM=android",
		"--include-tags", "offline", "/ws/.maestro"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("args\n got %q\nwant %q", got, want)
	}
}

func TestMaestroRefusesPhysicalIPhones(t *testing.T) {
	for _, v := range []string{"15.8.5", "26.3", ""} {
		d := model.Device{ID: "iphone", Platform: model.IOS, Specs: model.Specs{OSVersion: v}}
		if why := maestroUnsupported(d); !strings.Contains(why, "physical iPhone") {
			t.Fatalf("iOS %q: %q", v, why)
		}
	}
	if why := maestroUnsupported(model.Device{Platform: model.Android, Specs: model.Specs{OSVersion: "10"}}); why != "" {
		t.Fatalf("android: %q", why)
	}
}
