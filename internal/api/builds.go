package api

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/store"
)

// The build library: every app build that reached a farm phone, shared with
// every user. Builds get here only by being installed — there is no separate
// upload — and any of them can be downloaded or installed again from here.

func (s *Server) buildsOn(w http.ResponseWriter) bool {
	if s.lib == nil {
		fail(w, http.StatusNotImplemented, errors.New("build library is off"))
		return false
	}
	return true
}

func (s *Server) listBuilds(w http.ResponseWriter, r *http.Request) {
	if !s.buildsOn(w) {
		return
	}
	q := r.URL.Query()
	f := store.BuildFilter{Q: strings.TrimSpace(q.Get("q")), Platform: q.Get("platform"), User: q.Get("user")}
	f.Before, _ = strconv.ParseInt(q.Get("before"), 10, 64)
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	list, err := s.st.ListBuilds(f)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	n, size, _ := s.st.BuildsSize()
	writeJSON(w, http.StatusOK, map[string]any{"builds": list, "total": n, "total_bytes": size})
}

// pathBuild loads the {id} build; deleted ones only when allowDeleted.
func (s *Server) pathBuild(w http.ResponseWriter, r *http.Request, allowDeleted bool) (model.Build, bool) {
	id, err := strconv.ParseInt(strings.TrimPrefix(r.PathValue("id"), "b_"), 10, 64)
	if err != nil {
		fail(w, http.StatusBadRequest, errors.New("bad build id"))
		return model.Build{}, false
	}
	b, err := s.st.Build(id)
	if err != nil || (b.DeletedAt != nil && !allowDeleted) {
		fail(w, http.StatusNotFound, fmt.Errorf("build %d not found", id))
		return model.Build{}, false
	}
	return b, true
}

func (s *Server) getBuild(w http.ResponseWriter, r *http.Request) {
	if !s.buildsOn(w) {
		return
	}
	b, ok := s.pathBuild(w, r, true)
	if !ok {
		return
	}
	installs, err := s.st.BuildInstalls(b.ID, 500)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"build": b, "installs": installs})
}

func (s *Server) downloadBuild(w http.ResponseWriter, r *http.Request) {
	if !s.buildsOn(w) {
		return
	}
	b, ok := s.pathBuild(w, r, false)
	if !ok {
		return
	}
	p, err := s.lib.File(b)
	if err != nil {
		fail(w, http.StatusGone, err)
		return
	}
	f, err := os.Open(p)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	defer f.Close()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": b.Filename}))
	http.ServeContent(w, r, b.Filename, b.UploadedAt, f)
}

func (s *Server) patchBuild(w http.ResponseWriter, r *http.Request) {
	if !s.buildsOn(w) {
		return
	}
	b, ok := s.pathBuild(w, r, false)
	if !ok {
		return
	}
	var in struct {
		Note *string `json:"note"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if in.Note != nil {
		note := strings.TrimSpace(*in.Note)
		if len([]rune(note)) > 2000 {
			fail(w, http.StatusBadRequest, errors.New("note is longer than 2000 characters"))
			return
		}
		if err := s.st.SetBuildNote(b.ID, note); err != nil {
			fail(w, http.StatusInternalServerError, err)
			return
		}
	}
	nb, _ := s.st.Build(b.ID)
	writeJSON(w, http.StatusOK, nb)
}

// deleteBuild removes the file; only whoever first uploaded it may.
func (s *Server) deleteBuild(w http.ResponseWriter, r *http.Request) {
	if !s.buildsOn(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	b, ok := s.pathBuild(w, r, false)
	if !ok {
		return
	}
	if b.UploadedBy != u.Name {
		fail(w, http.StatusForbidden, fmt.Errorf("only %s, who uploaded it, can delete this build", b.UploadedBy))
		return
	}
	if err := s.lib.Delete(b); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.log.Info("build deleted", "build", b.ID, "file", b.Filename, "user", u.Name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "deleted"})
}

// installBuild installs a library build onto devices the caller holds.
func (s *Server) installBuild(w http.ResponseWriter, r *http.Request) {
	if !s.buildsOn(w) {
		return
	}
	u, _ := auth.UserFrom(r.Context())
	b, ok := s.pathBuild(w, r, false)
	if !ok {
		return
	}
	var in struct {
		DeviceIDs []string `json:"device_ids"`
	}
	if err := readJSON(r, &in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if len(in.DeviceIDs) == 0 {
		fail(w, http.StatusBadRequest, errors.New("pick at least one device"))
		return
	}
	path, err := s.lib.File(b)
	if err != nil {
		fail(w, http.StatusGone, err)
		return
	}

	type result struct {
		Device  string `json:"device"`
		Status  string `json:"status"`
		Detail  string `json:"detail,omitempty"`
		Package string `json:"package,omitempty"`
	}
	results := make([]result, len(in.DeviceIDs))
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Minute)
	defer cancel()
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, id := range in.DeviceIDs {
		dev, err := s.st.Device(id)
		if err != nil {
			results[i] = result{Device: id, Status: "failed", Detail: "no such device"}
			continue
		}
		if res, ok, _ := s.res.Holder(id); !ok || res.User != u.Name {
			results[i] = result{Device: id, Status: "failed", Detail: "reserve the device first"}
			continue
		}
		if dev.Platform != b.Platform {
			results[i] = result{Device: id, Status: "skipped", Detail: fmt.Sprintf("%s build, %s device", b.Platform, dev.Platform)}
			continue
		}
		wg.Add(1)
		go func(i int, dev model.Device) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			_ = s.st.SetDeviceStatus(dev.ID, model.StatusBusy, time.Now())
			defer s.st.SetDeviceStatus(dev.ID, model.StatusReserved, time.Now())
			res, ierr := s.inst.Run(ctx, dev, path,
				install.Origin{User: u.Name, Via: "library", Ref: "b_" + strconv.FormatInt(b.ID, 10), Name: b.Filename})
			if ierr != nil {
				results[i] = result{Device: dev.ID, Status: "failed", Detail: ierr.Error()}
				return
			}
			results[i] = result{Device: dev.ID, Status: "ok", Package: res.Package, Detail: res.Output}
		}(i, dev)
	}
	wg.Wait()

	okN := 0
	for _, rr := range results {
		if rr.Status == "ok" {
			okN++
		}
	}
	s.log.Info("library install", "build", b.ID, "file", b.Filename, "user", u.Name, "ok", okN, "total", len(results))
	code := http.StatusOK
	if okN < len(results) {
		code = http.StatusMultiStatus
	}
	writeJSON(w, code, map[string]any{"ok": okN, "total": len(results), "results": results})
}
