// Package builds keeps every app build that has been installed onto a farm
// phone — whoever installed it and however (dashboard, batch, MCP agent, test
// run) — so it can be found, downloaded and installed again later without
// anyone uploading it a second time.
//
// Files are stored once per content (sha256) under <StorageDir>/builds; the
// database holds who uploaded each build, when, what app and version it is,
// and every install of it.
package builds

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/store"
)

// Library implements install.Recorder.
type Library struct {
	st      *store.Store
	storage string // StorageDir; builds live in storage/builds
	log     *slog.Logger

	// a run installs the same file on several devices: hash it once
	mu     sync.Mutex
	hashed map[string]hashEntry
}

type hashEntry struct {
	size  int64
	mtime time.Time
	id    int64
}

// New builds a Library rooted at the farm's storage dir.
func New(st *store.Store, storageDir string, log *slog.Logger) *Library {
	return &Library{st: st, storage: storageDir, log: log, hashed: map[string]hashEntry{}}
}

// Add stores the file (once per content) and returns its build id.
func (l *Library) Add(path string, o install.Origin) (int64, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	l.mu.Lock()
	h, ok := l.hashed[path]
	l.mu.Unlock()
	if ok && h.size == fi.Size() && h.mtime.Equal(fi.ModTime()) {
		return h.id, nil
	}

	id, err := l.add(path, fi, o)
	if err != nil {
		l.log.Warn("build library: could not keep build", "file", filepath.Base(path), "err", err)
		return 0, err
	}
	l.mu.Lock()
	l.hashed[path] = hashEntry{size: fi.Size(), mtime: fi.ModTime(), id: id}
	l.mu.Unlock()
	return id, nil
}

func (l *Library) add(path string, fi os.FileInfo, o install.Origin) (int64, error) {
	ext := strings.ToLower(filepath.Ext(path))
	plat := PlatformFor(ext)
	if plat == "" {
		return 0, fmt.Errorf("not an app build: %s", filepath.Base(path))
	}
	sum, err := hashFile(path)
	if err != nil {
		return 0, err
	}
	rel := filepath.Join("builds", sum[:2], sum+ext)
	dst := filepath.Join(l.storage, rel)
	if _, err := os.Stat(dst); err != nil {
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return 0, err
		}
		if err := linkOrCopy(path, dst); err != nil {
			return 0, err
		}
	}

	name := o.Name
	if name == "" {
		name = filepath.Base(path)
	}
	meta, merr := install.Inspect(dst)
	if merr != nil {
		l.log.Info("build library: could not read build metadata", "file", name, "err", merr)
	}
	b, isNew, err := l.st.AddBuild(model.Build{
		SHA256: sum, Filename: name, Platform: plat, Format: strings.TrimPrefix(ext, "."),
		Size: fi.Size(), Package: meta.Package, AppName: meta.AppName, Version: meta.Version,
		BuildCode: meta.BuildCode, MinOS: meta.MinOS, Path: rel,
		UploadedBy: o.User, UploadedAt: time.Now(), Via: o.Via, SourceURL: o.URL,
	})
	if err != nil {
		return 0, err
	}
	if isNew {
		l.log.Info("build library: new build", "id", b.ID, "file", name, "package", b.Package,
			"version", b.Version, "user", o.User, "via", o.Via)
	}
	return b.ID, nil
}

// Installed logs one install of a build.
func (l *Library) Installed(buildID int64, dev model.Device, o install.Origin, res install.Result, ierr error) {
	status, detail := "ok", ""
	if ierr != nil {
		status, detail = "failed", ierr.Error()
		if len(detail) > 2000 {
			detail = detail[:2000] + "…"
		}
	}
	if res.Package != "" {
		_ = l.st.FillBuildMeta(buildID, res.Package, res.Version)
	}
	modelName := strings.TrimSpace(dev.Specs.Manufacturer + " " + dev.Specs.Model)
	if err := l.st.AddBuildInstall(model.BuildInstall{
		BuildID: buildID, DeviceID: dev.ID, DeviceModel: modelName, OSVersion: dev.Specs.OSVersion,
		User: o.User, Via: o.Via, Ref: o.Ref, Status: status, Detail: detail, CreatedAt: time.Now(),
	}); err != nil {
		l.log.Warn("build library: could not log install", "build", buildID, "device", dev.ID, "err", err)
	}
}

// File returns the on-disk path of a live build.
func (l *Library) File(b model.Build) (string, error) {
	if b.DeletedAt != nil {
		return "", fmt.Errorf("build %d was deleted", b.ID)
	}
	p := filepath.Join(l.storage, b.Path)
	if _, err := os.Stat(p); err != nil {
		return "", fmt.Errorf("build %d: file is gone from the farm", b.ID)
	}
	return p, nil
}

// Delete marks a build deleted and removes its file. Install history stays.
func (l *Library) Delete(b model.Build) error {
	if err := l.st.DeleteBuild(b.ID); err != nil {
		return err
	}
	l.mu.Lock()
	for k, h := range l.hashed {
		if h.id == b.ID {
			delete(l.hashed, k)
		}
	}
	l.mu.Unlock()
	if err := os.Remove(filepath.Join(l.storage, b.Path)); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// PlatformFor maps a build file extension to the platform it installs on.
func PlatformFor(ext string) model.Platform {
	switch strings.ToLower(ext) {
	case ".apk", ".aab", ".apks":
		return model.Android
	case ".ipa":
		return model.IOS
	}
	return ""
}

func hashFile(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// linkOrCopy hard-links src to dst (uploads and builds share a disk, so the
// library costs no extra space), falling back to a copy.
func linkOrCopy(src, dst string) error {
	if err := os.Link(src, dst); err == nil {
		return nil
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	tmp := dst + ".part"
	out, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}
