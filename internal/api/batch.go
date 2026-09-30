package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/model"
)

// platformForExt maps an artifact extension to the device platform that can
// install it. Unknown extensions return "".
func platformForExt(name string) model.Platform {
	switch strings.ToLower(filepath.Ext(name)) {
	case ".apk", ".aab", ".apks":
		return model.Android
	case ".ipa":
		return model.IOS
	}
	return ""
}

// saveUpload streams one multipart file into dir, keeping its base name.
func saveUpload(hdr *multipart.FileHeader, dir string) (string, error) {
	src, err := hdr.Open()
	if err != nil {
		return "", err
	}
	defer src.Close()
	path := filepath.Join(dir, filepath.Base(hdr.Filename))
	dst, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer dst.Close()
	if _, err := io.Copy(dst, src); err != nil {
		return "", err
	}
	return path, nil
}

type batchCreateReq struct {
	Devices []string `json:"devices"`
}

func (s *Server) batchCreate(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	var req batchCreateReq
	if err := readJSON(r, &req); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	batch, res, err := s.res.ReserveMany(req.Devices, u.Name)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	s.syncUserTunnel(u.Name)
	writeJSON(w, http.StatusOK, map[string]any{"batch": batch, "reservations": res})
}

// batchList reports every batch the caller still holds so the dashboard can
// offer a way back to (or out of) devices reserved from another browser.
func (s *Server) batchList(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	batches, err := s.res.UserBatches(u.Name)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"batches": batches})
}

func (s *Server) batchGet(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	ids, err := s.res.BatchDevices(r.PathValue("batch"), u.Name)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	out := make([]deviceView, 0, len(ids))
	for _, id := range ids {
		d, err := s.st.Device(id)
		if err != nil {
			continue
		}
		out = append(out, deviceView{Device: d})
	}
	writeJSON(w, http.StatusOK, map[string]any{"batch": r.PathValue("batch"), "devices": out})
}

func (s *Server) batchHeartbeat(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if err := s.res.HeartbeatBatch(r.PathValue("batch"), u.Name); err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}
	s.syncUserTunnel(u.Name) // self-heals the user's tunnel if it died since reserve
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) batchRelease(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if err := s.res.ReleaseBatch(r.PathValue("batch"), u.Name, false); err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}
	s.syncUserTunnel(u.Name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

// batchInstall uploads one artifact per platform (an .apk/.aab for Android, an
// .ipa for iOS) and installs each device in the batch with the artifact that
// matches its platform, in parallel (bounded). A mixed Android+iOS batch needs
// both files; a device whose platform has no artifact is skipped.
func (s *Server) batchInstall(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	batch := r.PathValue("batch")

	ids, err := s.res.BatchDevices(batch, u.Name)
	if err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}

	if err := r.ParseMultipartForm(1 << 30); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	headers := r.MultipartForm.File["artifact"]
	if len(headers) == 0 {
		fail(w, http.StatusBadRequest, errors.New("no artifact uploaded"))
		return
	}

	dir := filepath.Join(s.cfg.StorageDir, "uploads",
		time.Now().Format("20060102-150405")+"-batch-"+batch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}

	// one artifact per platform, keyed by the extension's target platform
	artifacts := map[model.Platform]string{}
	names := map[model.Platform]string{}
	for _, hdr := range headers {
		plat := platformForExt(hdr.Filename)
		if plat == "" {
			fail(w, http.StatusBadRequest, fmt.Errorf("unsupported artifact %q (want .apk/.aab/.apks or .ipa)", hdr.Filename))
			return
		}
		if _, dup := artifacts[plat]; dup {
			fail(w, http.StatusBadRequest, fmt.Errorf("more than one %s artifact uploaded", plat))
			return
		}
		p, err := saveUpload(hdr, dir)
		if err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
		artifacts[plat] = p
		names[plat] = hdr.Filename
	}

	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()

	type deviceResult struct {
		Device  string `json:"device"`
		Status  string `json:"status"`
		Detail  string `json:"detail,omitempty"`
		Package string `json:"package,omitempty"`
	}
	results := make([]deviceResult, len(ids))

	const maxParallel = 4
	sem := make(chan struct{}, maxParallel)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			dev, err := s.st.Device(id)
			if err != nil {
				results[i] = deviceResult{Device: id, Status: "failed", Detail: err.Error()}
				return
			}
			artifactPath, ok := artifacts[dev.Platform]
			if !ok {
				results[i] = deviceResult{Device: id, Status: "skipped",
					Detail: fmt.Sprintf("no %s build uploaded", dev.Platform)}
				return
			}
			_ = s.st.SetDeviceStatus(id, model.StatusBusy, time.Now())
			res, ierr := s.inst.Run(ctx, dev, artifactPath,
				install.Origin{User: u.Name, Via: "batch", Ref: batch, Name: names[dev.Platform]})
			if ierr != nil {
				results[i] = deviceResult{Device: id, Status: "failed", Detail: ierr.Error()}
			} else {
				results[i] = deviceResult{Device: id, Status: "ok", Package: res.Package, Detail: res.Output}
			}
			_ = s.st.SetDeviceStatus(id, model.StatusReserved, time.Now())
		}(i, id)
	}
	wg.Wait()

	ok := 0
	for _, rr := range results {
		if rr.Status == "ok" {
			ok++
		}
	}
	uploaded := make([]string, 0, len(names))
	for _, n := range names {
		uploaded = append(uploaded, n)
	}
	s.log.Info("batch install", "batch", batch, "user", u.Name,
		"artifacts", strings.Join(uploaded, ", "), "ok", ok, "total", len(ids))

	code := http.StatusOK
	if ok < len(ids) {
		code = http.StatusMultiStatus
	}
	writeJSON(w, code, map[string]any{"ok": ok, "total": len(ids), "results": results})
}

// readJSON is a tiny helper mirroring writeJSON.
func readJSON(r *http.Request, v any) error {
	defer r.Body.Close()
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(v); err != nil {
		return errors.New("invalid JSON body")
	}
	return nil
}
