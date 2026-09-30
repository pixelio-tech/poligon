package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/runner"
	"github.com/pancir/poligon/internal/store"
)

// createRun starts a test run. Multipart form:
//
//	type          run type (default "install_smoke")
//	device        repeated device ids (the devices to run on)
//	watch_seconds smoke settle window (optional)
//	artifact      one build file per platform (.apk/.aab for Android, .ipa for iOS)
//	flow          maestro: a .yaml, or a .zip of the whole .maestro/ workspace
//	flow_path     maestro: the flow to run inside a zipped workspace (optional)
//	env           maestro: repeated KEY=VALUE, passed as -e
//	include_tags  maestro: --include-tags; exclude_tags likewise
func (s *Server) createRun(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())

	multipart := strings.HasPrefix(r.Header.Get("Content-Type"), "multipart/")
	if multipart {
		if err := r.ParseMultipartForm(1 << 30); err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
	} else if err := r.ParseForm(); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}

	typ := r.FormValue("type")
	if typ == "" {
		typ = "install_smoke"
	}
	if !runner.Types[typ] {
		fail(w, http.StatusBadRequest, errors.New("unknown run type "+typ))
		return
	}

	dir := filepath.Join(s.cfg.StorageDir, "uploads",
		time.Now().Format("20060102-150405")+"-run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	spec := model.RunSpec{Artifacts: map[model.Platform]string{}}

	// --- flow (maestro): uploaded file or flow_url ---
	if multipart {
		if fh := r.MultipartForm.File["flow"]; len(fh) == 1 {
			p, err := saveUpload(fh[0], dir)
			if err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
			spec.FlowPath = p
		} else if len(fh) > 1 {
			fail(w, http.StatusBadRequest, errors.New("attach exactly one Maestro flow"))
			return
		}
	}
	if spec.FlowPath == "" {
		if url := r.FormValue("flow_url"); url != "" {
			p, err := s.downloadArtifact(r.Context(), url, dir, "flow.yaml")
			if err != nil {
				fail(w, http.StatusBadGateway, err)
				return
			}
			spec.FlowPath = p
		}
	}

	// a zipped .maestro workspace (flows + subflows + config.yaml) unpacks
	// next to the upload; flow_path picks one flow inside it, else maestro
	// runs the whole workspace (its config.yaml decides which flows count)
	if spec.FlowPath != "" && runner.IsZip(spec.FlowPath) {
		target, err := runner.ExtractWorkspace(spec.FlowPath, filepath.Join(dir, "workspace"), r.FormValue("flow_path"))
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		spec.FlowPath = target
	} else if r.FormValue("flow_path") != "" {
		fail(w, http.StatusBadRequest, errors.New("flow_path needs a zipped workspace as the flow"))
		return
	}
	env, err := parseEnv(r.Form["env"])
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	spec.Env = env
	spec.IncludeTags = r.FormValue("include_tags")
	spec.ExcludeTags = r.FormValue("exclude_tags")

	// --- build artifact(s): uploaded files or artifact_url ---
	if multipart {
		for _, hdr := range r.MultipartForm.File["artifact"] {
			plat := platformForExt(hdr.Filename)
			if plat == "" {
				fail(w, http.StatusBadRequest, errors.New("unsupported artifact "+hdr.Filename))
				return
			}
			if _, dup := spec.Artifacts[plat]; dup {
				fail(w, http.StatusBadRequest, errors.New("more than one "+string(plat)+" artifact"))
				return
			}
			p, err := saveUpload(hdr, dir)
			if err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
			spec.Artifacts[plat] = p
		}
	}
	if url := r.FormValue("artifact_url"); url != "" {
		base := filepath.Base(url)
		if q := strings.IndexAny(base, "?#"); q >= 0 {
			base = base[:q]
		}
		plat := platformForExt(base)
		if plat == "" {
			fail(w, http.StatusBadRequest, errors.New("artifact_url must end in .apk/.aab/.apks/.ipa"))
			return
		}
		if _, dup := spec.Artifacts[plat]; dup {
			fail(w, http.StatusBadRequest, errors.New("more than one "+string(plat)+" artifact"))
			return
		}
		p, err := s.downloadArtifact(r.Context(), url, dir, base)
		if err != nil {
			fail(w, http.StatusBadGateway, err)
			return
		}
		spec.Artifacts[plat] = p
	}

	// --- test artifact(s) (integration_test): uploaded files or test_artifact_url.
	// Same shape as the app artifact above but a separate map — an
	// androidTest apk installs alongside the app, it doesn't replace it.
	spec.TestArtifacts = map[model.Platform]string{}
	if multipart {
		for _, hdr := range r.MultipartForm.File["test_artifact"] {
			plat := testPlatformForExt(hdr.Filename)
			if plat == "" {
				fail(w, http.StatusBadRequest, errors.New("unsupported test artifact "+hdr.Filename+" (want an androidTest .apk, or for iOS the zipped Build/Products of xcodebuild build-for-testing)"))
				return
			}
			if _, dup := spec.TestArtifacts[plat]; dup {
				fail(w, http.StatusBadRequest, errors.New("more than one "+string(plat)+" test artifact"))
				return
			}
			p, err := saveUpload(hdr, dir)
			if err != nil {
				fail(w, http.StatusInternalServerError, err)
				return
			}
			spec.TestArtifacts[plat] = p
		}
	}
	if url := r.FormValue("test_artifact_url"); url != "" {
		base := filepath.Base(url)
		if q := strings.IndexAny(base, "?#"); q >= 0 {
			base = base[:q]
		}
		plat := testPlatformForExt(base)
		if plat == "" {
			fail(w, http.StatusBadRequest, errors.New("test_artifact_url must end in .apk (androidTest) or .zip (iOS Build/Products)"))
			return
		}
		if _, dup := spec.TestArtifacts[plat]; dup {
			fail(w, http.StatusBadRequest, errors.New("more than one "+string(plat)+" test artifact"))
			return
		}
		p, err := s.downloadArtifact(r.Context(), url, dir, base)
		if err != nil {
			fail(w, http.StatusBadGateway, err)
			return
		}
		spec.TestArtifacts[plat] = p
	}

	spec.Command = r.FormValue("command")

	if err := checkRunSpec(typ, spec); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}

	if v := r.FormValue("watch_seconds"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			spec.WatchSeconds = n
		}
	}
	if v := r.FormValue("timeout_seconds"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			spec.TimeoutSeconds = n
		}
	}
	spec.CallbackURL = r.FormValue("callback_url")

	// --- device set: explicit ids, or a selector (platform/count/tag) ---
	devices := r.Form["device"]
	if len(devices) == 0 {
		sel, err := s.selectDevices(r.FormValue("platform"), r.FormValue("tag"), r.FormValue("count"))
		if err != nil {
			fail(w, http.StatusBadRequest, err)
			return
		}
		devices = sel
	}
	if len(devices) == 0 {
		fail(w, http.StatusBadRequest, errors.New("no devices (pass device= or platform=&count=)"))
		return
	}

	trigger := "ui"
	if !multipart || r.Header.Get("Authorization") != "" {
		trigger = "api"
	}
	run, err := s.run.Submit(u.Name, typ, spec, devices, trigger)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, run.Redacted())
}

// checkRunSpec rejects a run missing the inputs its type needs.
func checkRunSpec(typ string, spec model.RunSpec) error {
	switch typ {
	case "install_smoke":
		if len(spec.Artifacts) == 0 {
			return errors.New("install_smoke needs a build (artifact or artifact_url)")
		}
	case "maestro":
		if spec.FlowPath == "" {
			return errors.New("maestro needs a flow (flow file or flow_url)")
		}
	case "command":
		if spec.Command == "" {
			return errors.New("command run needs command=")
		}
	case "integration_test":
		// Android: the app apk + its androidTest apk. iOS: one zip of
		// build-for-testing products, which carries the app inside it.
		if len(spec.TestArtifacts) == 0 {
			return errors.New("integration_test needs the test build (test_artifact or test_artifact_url): an androidTest .apk, or for iOS the zipped Build/Products of xcodebuild build-for-testing")
		}
		if _, ok := spec.TestArtifacts[model.Android]; ok && spec.Artifacts[model.Android] == "" {
			return errors.New("integration_test on Android needs the app apk too (artifact or artifact_url)")
		}
	}
	return nil
}

// selectDevices resolves a platform/tag/count selector to free device ids.
func (s *Server) selectDevices(platform, tag, countStr string) ([]string, error) {
	count, err := strconv.Atoi(countStr)
	if err != nil || count < 1 {
		return nil, errors.New("count must be a positive integer")
	}
	pool, err := s.st.Devices()
	if err != nil {
		return nil, err
	}
	var picked []string
	for _, d := range pool {
		if !d.Adopted || d.Status != model.StatusFree {
			continue
		}
		if platform != "" && string(d.Platform) != platform {
			continue
		}
		if tag != "" && !contains(d.Tags, tag) {
			continue
		}
		picked = append(picked, d.ID)
		if len(picked) == count {
			return picked, nil
		}
	}
	return nil, fmt.Errorf("only %d free device(s) match (need %d)", len(picked), count)
}

func contains(ss []string, v string) bool {
	for _, s := range ss {
		if s == v {
			return true
		}
	}
	return false
}

// downloadArtifact fetches url into dir. http(s) only, 30-min timeout, 4 GiB cap.
func (s *Server) downloadArtifact(ctx context.Context, url, dir, name string) (string, error) {
	if !strings.HasPrefix(url, "http://") && !strings.HasPrefix(url, "https://") {
		return "", errors.New("url must be http(s)")
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Minute)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("fetch %s: %s", url, resp.Status)
	}
	path := filepath.Join(dir, filepath.Base(name))
	f, err := os.Create(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(resp.Body, 4<<30)); err != nil {
		return "", err
	}
	return path, nil
}

var envKey = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// parseEnv reads repeated env=KEY=VALUE fields for maestro's -e. POLIGON_* is
// reserved: the runner sets those per device.
func parseEnv(pairs []string) (map[string]string, error) {
	if len(pairs) == 0 {
		return nil, nil
	}
	env := make(map[string]string, len(pairs))
	for _, kv := range pairs {
		k, v, ok := strings.Cut(kv, "=")
		if !ok || !envKey.MatchString(k) {
			return nil, fmt.Errorf("env %q: want KEY=VALUE", kv)
		}
		if strings.HasPrefix(k, "POLIGON_") {
			return nil, fmt.Errorf("env %s: POLIGON_* is set by the farm", k)
		}
		env[k] = v
	}
	return env, nil
}

func (s *Server) listRuns(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.RunFilter{
		Type: q.Get("type"),
		User: q.Get("user"),
		Q:    q.Get("q"),
	}
	// status accepts a comma-separated set so the dashboard's "упавшие" filter
	// is one request rather than two
	if st := q.Get("status"); st != "" {
		if strings.Contains(st, ",") {
			for _, v := range strings.Split(st, ",") {
				if v = strings.TrimSpace(v); v != "" {
					f.Statuses = append(f.Statuses, v)
				}
			}
		} else {
			f.Status = st
		}
	}
	if v, err := strconv.Atoi(q.Get("limit")); err == nil {
		f.Limit = v
	}
	if v, err := strconv.Atoi(q.Get("offset")); err == nil {
		f.Offset = v
	}
	runs, total, err := s.st.Runs(f)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	for i := range runs {
		runs[i] = runs[i].Redacted()
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"runs":  runs,
		"total": total,
	})
}

func (s *Server) getRun(w http.ResponseWriter, r *http.Request) {
	run, err := s.st.Run(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, run.Redacted())
}

// rerunRun re-submits a finished run with the same spec and device set.
func (s *Server) rerunRun(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	old, err := s.st.Run(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	// verify the original upload files still exist
	for _, p := range old.Spec.Artifacts {
		if _, err := os.Stat(p); err != nil {
			fail(w, http.StatusGone, errors.New("original artifact no longer on disk — start a fresh run"))
			return
		}
	}
	for _, p := range old.Spec.TestArtifacts {
		if _, err := os.Stat(p); err != nil {
			fail(w, http.StatusGone, errors.New("original test artifact no longer on disk — start a fresh run"))
			return
		}
	}
	if old.Spec.FlowPath != "" {
		if _, err := os.Stat(old.Spec.FlowPath); err != nil {
			fail(w, http.StatusGone, errors.New("original flow no longer on disk — start a fresh run"))
			return
		}
	}
	ids := make([]string, 0, len(old.Devices))
	for _, d := range old.Devices {
		ids = append(ids, d.DeviceID)
	}
	run, err := s.run.Submit(u.Name, old.Type, old.Spec, ids, "ui")
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, run.Redacted())
}

func (s *Server) cancelRun(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	owner, err := s.st.RunOwner(id)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	if owner != u.Name {
		fail(w, http.StatusForbidden, errors.New("not your run"))
		return
	}
	if !s.run.Cancel(id) {
		fail(w, http.StatusConflict, errors.New("run is not active"))
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "canceling"})
}

// runBadge serves a shields-style SVG for a run's status — for embedding in a
// CI dashboard or README. Unauthenticated (run ids are unguessable enough on a
// trusted LAN) and cache-busting.
func (s *Server) runBadge(w http.ResponseWriter, r *http.Request) {
	run, err := s.st.Run(strings.TrimSuffix(r.PathValue("id"), ".svg"))
	label := "unknown"
	color := "#9f9f9f"
	if err == nil {
		label = string(run.Status)
		switch run.Status {
		case model.RunPassed:
			color = "#3fb950"
		case model.RunFailed, model.RunError:
			color = "#e5534b"
		case model.RunRunning, model.RunQueued:
			color = "#d29922"
		case model.RunCanceled:
			color = "#8b949e"
		}
	}
	lw := 44
	vw := 8*len(label) + 20
	svg := fmt.Sprintf(`<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="20" role="img">`+
		`<rect width="%d" height="20" fill="#555"/>`+
		`<rect x="%d" width="%d" height="20" fill="%s"/>`+
		`<g fill="#fff" font-family="Verdana,DejaVu Sans,sans-serif" font-size="11">`+
		`<text x="6" y="14">poligon</text>`+
		`<text x="%d" y="14">%s</text></g></svg>`,
		lw+vw, lw, lw, vw, color, lw+6, label)
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "no-cache, max-age=0")
	_, _ = w.Write([]byte(svg))
}

// runArtifact serves one file written by a run, e.g.
// GET /api/runs/{id}/artifacts/{device}/logcat.txt
// GET /api/runs/{id}/artifacts/{device}/maestro/recording.mp4
func (s *Server) runArtifact(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	device := r.PathValue("device")
	rel := r.PathValue("path")
	if strings.ContainsAny(id, "/\\") || strings.ContainsAny(device, "/\\") ||
		strings.Contains(rel, "..") || strings.Contains(rel, "\\") {
		fail(w, http.StatusBadRequest, errors.New("bad path"))
		return
	}
	if _, err := s.st.Run(id); err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	root := filepath.Join(s.cfg.StorageDir, "runs")
	path := filepath.Join(root, id, device, filepath.Clean("/"+rel))
	if path != root && !strings.HasPrefix(path, root+string(filepath.Separator)) {
		fail(w, http.StatusBadRequest, errors.New("bad path"))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	http.ServeFile(w, r, path)
}
