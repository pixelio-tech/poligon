package builds

import (
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/store"
)

func newLib(t *testing.T) (*Library, *store.Store, string) {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	return New(st, dir, slog.New(slog.NewTextHandler(io.Discard, nil))), st, dir
}

func writeFile(t *testing.T, dir, name, body string) string {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestSameFileIsOneBuild(t *testing.T) {
	lib, st, dir := newLib(t)
	a := writeFile(t, filepath.Join(dir, "uploads", "1"), "app.apk", "same bytes")
	b := writeFile(t, filepath.Join(dir, "uploads", "2"), "app-copy.apk", "same bytes")

	id1, err := lib.Add(a, install.Origin{User: "alice", Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	id2, err := lib.Add(b, install.Origin{User: "bob", Via: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	if id1 != id2 {
		t.Fatalf("identical files got two builds: %d, %d", id1, id2)
	}
	got, err := st.Build(id1)
	if err != nil {
		t.Fatal(err)
	}
	if got.UploadedBy != "alice" || got.Filename != "app.apk" || got.Platform != model.Android || got.Format != "apk" {
		t.Fatalf("first upload should own the build: %+v", got)
	}
	if _, err := os.Stat(filepath.Join(dir, got.Path)); err != nil {
		t.Fatalf("build file not kept: %v", err)
	}
}

func TestInstallsAreLoggedEvenWhenTheyFail(t *testing.T) {
	lib, st, dir := newLib(t)
	p := writeFile(t, filepath.Join(dir, "uploads", "1"), "shop.ipa", "ipa bytes")
	o := install.Origin{User: "alice", Via: "batch", Ref: "batch1"}
	id, err := lib.Add(p, o)
	if err != nil {
		t.Fatal(err)
	}
	dev := model.Device{ID: "iphone-8", Specs: model.Specs{Manufacturer: "Apple", Model: "iPhone8,1", OSVersion: "15.8"}}
	lib.Installed(id, dev, o, install.Result{Package: "com.shop"}, nil)
	lib.Installed(id, dev, o, install.Result{}, errors.New("device locked"))

	ins, err := st.BuildInstalls(id, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(ins) != 2 {
		t.Fatalf("want 2 installs, got %d", len(ins))
	}
	var ok, failed int
	for _, i := range ins {
		if i.Status == "ok" {
			ok++
		} else {
			failed++
			if i.Detail != "device locked" {
				t.Fatalf("failure detail lost: %q", i.Detail)
			}
		}
		if i.DeviceModel != "Apple iPhone8,1" || i.Ref != "batch1" || i.User != "alice" {
			t.Fatalf("install row: %+v", i)
		}
	}
	if ok != 1 || failed != 1 {
		t.Fatalf("ok=%d failed=%d", ok, failed)
	}
	b, _ := st.Build(id)
	if b.Package != "com.shop" {
		t.Fatalf("package from the install result should fill a blank one, got %q", b.Package)
	}
	if b.Installs != 2 || b.InstallsOK != 1 || b.Devices != 1 || b.LastInstalled == nil {
		t.Fatalf("summary: %+v", b)
	}
}

func TestDeletedBuildComesBackWhenInstalledAgain(t *testing.T) {
	lib, st, dir := newLib(t)
	p := writeFile(t, filepath.Join(dir, "uploads", "1"), "app.aab", "aab bytes")
	id, err := lib.Add(p, install.Origin{User: "alice", Via: "web"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := st.Build(id)
	if err := lib.Delete(b); err != nil {
		t.Fatal(err)
	}
	if list, _ := st.ListBuilds(store.BuildFilter{}); len(list) != 0 {
		t.Fatalf("deleted build still listed")
	}
	if _, err := lib.File(b); err == nil {
		t.Fatalf("deleted build still downloadable")
	}

	id2, err := lib.Add(p, install.Origin{User: "bob", Via: "mcp"})
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := st.Build(id2)
	if id2 != id || b2.DeletedAt != nil || b2.UploadedBy != "bob" {
		t.Fatalf("revived build: id %d→%d %+v", id, id2, b2)
	}
	if _, err := lib.File(b2); err != nil {
		t.Fatalf("revived build file: %v", err)
	}
}

func TestListFilters(t *testing.T) {
	lib, st, dir := newLib(t)
	lib.Add(writeFile(t, filepath.Join(dir, "u1"), "alpha.apk", "1"), install.Origin{User: "alice"})
	lib.Add(writeFile(t, filepath.Join(dir, "u2"), "beta.ipa", "2"), install.Origin{User: "bob"})

	if l, _ := st.ListBuilds(store.BuildFilter{Platform: "ios"}); len(l) != 1 || l[0].Filename != "beta.ipa" {
		t.Fatalf("platform filter: %+v", l)
	}
	if l, _ := st.ListBuilds(store.BuildFilter{Q: "ALICE"}); len(l) != 1 || l[0].Filename != "alpha.apk" {
		t.Fatalf("query should match uploader, case-insensitively: %+v", l)
	}
	if n, size, _ := st.BuildsSize(); n != 2 || size != 2 {
		t.Fatalf("size: %d builds, %d bytes", n, size)
	}
}
