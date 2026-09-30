// Package api is poligon's HTTP surface: the JSON API and the embedded dashboard.
package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/adbfilter"
	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/builds"
	"github.com/pancir/poligon/internal/capture"
	"github.com/pancir/poligon/internal/config"
	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/live"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/provision"
	"github.com/pancir/poligon/internal/reserve"
	"github.com/pancir/poligon/internal/runner"
	"github.com/pancir/poligon/internal/store"
	"github.com/pancir/poligon/internal/webui"
)

// Server holds the API dependencies.
type Server struct {
	cfg  config.Config
	st   *store.Store
	res  *reserve.Manager
	inst *install.Installer
	live *live.Proxy
	ios  *iosscreen.Controller
	prov *provision.Manager
	capt *capture.Capturer
	run  *runner.Runner
	auth *auth.Auth
	log  *slog.Logger
	web  http.FileSystem

	lib       *builds.Library // nil: build library off
	adbFilter *adbfilter.Manager
	agent     *agentState // MCP-side caches; set by Handler
}

// New builds the API server.
func New(cfg config.Config, st *store.Store, res *reserve.Manager, inst *install.Installer, lp *live.Proxy, ios *iosscreen.Controller, prov *provision.Manager, capt *capture.Capturer, run *runner.Runner, web http.FileSystem, log *slog.Logger) *Server {
	return &Server{
		cfg: cfg, st: st, res: res, inst: inst, live: lp, ios: ios, prov: prov, capt: capt, run: run, web: web, log: log,
		adbFilter: adbfilter.New(cfg.ADBServerAddr),
	}
}

// SetLibrary turns on the build library routes.
func (s *Server) SetLibrary(l *builds.Library) { s.lib = l }

// Handler returns the root http.Handler with auth applied to /api.
func (s *Server) Handler(a *auth.Auth) http.Handler {
	s.auth = a
	mux := http.NewServeMux()

	// unauthenticated login surface
	mux.HandleFunc("POST /auth/login", s.authLogin)
	mux.HandleFunc("POST /auth/register", s.authRegister)
	mux.HandleFunc("POST /auth/logout", s.authLogout)
	mux.HandleFunc("GET /auth/setup", s.setupPage)
	mux.HandleFunc("GET /auth/setup/check", s.setupCheck)
	mux.HandleFunc("POST /auth/setup", s.setupSubmit)
	mux.HandleFunc("GET /auth/me", s.authMe)
	mux.HandleFunc("GET /healthz", s.healthz)
	mux.HandleFunc("GET /runs/{id}/badge", s.runBadge) // unauth: status SVG for CI
	mux.HandleFunc("GET /runs/{id}/badge.svg", s.runBadge)
	mux.Handle("POST /auth/password", a.Middleware(http.HandlerFunc(s.authChangePassword)))

	api := http.NewServeMux()
	api.HandleFunc("GET /devices", s.listDevices)
	api.HandleFunc("GET /devices/{id}", s.getDevice)
	api.HandleFunc("POST /devices/{id}/reserve", s.reserve)
	api.HandleFunc("POST /devices/{id}/release", s.release)
	api.HandleFunc("POST /devices/{id}/heartbeat", s.heartbeat)
	api.HandleFunc("POST /devices/{id}/install", s.install)
	api.HandleFunc("GET /devices/{id}/screenshot", s.deviceScreenshot)
	api.HandleFunc("GET /devices/{id}/logcat", s.deviceLogcat)
	api.HandleFunc("POST /devices/{id}/keyevent", s.deviceKeyevent)
	api.HandleFunc("GET /devices/{id}/shell", s.deviceShell)
	api.HandleFunc("GET /devices/{id}/files", s.deviceFilesList)
	api.HandleFunc("GET /devices/{id}/files/download", s.deviceFilesDownload)
	api.HandleFunc("POST /devices/{id}/files/upload", s.deviceFilesUpload)
	api.HandleFunc("POST /devices/{id}/files/delete", s.deviceFilesDelete)
	api.HandleFunc("POST /devices/{id}/files/rename", s.deviceFilesRename)
	api.HandleFunc("GET /devices/{id}/logcat/stream", s.deviceLogcatStream)
	api.HandleFunc("GET /devices/{id}/apps", s.deviceAppsList)
	api.HandleFunc("POST /devices/{id}/apps/force-stop", s.deviceAppForceStop)
	api.HandleFunc("POST /devices/{id}/apps/clear-data", s.deviceAppClearData)
	api.HandleFunc("POST /devices/{id}/apps/uninstall", s.deviceAppUninstall)
	api.HandleFunc("POST /devices/{id}/apps/launch", s.deviceAppLaunch)
	api.HandleFunc("GET /devices/{id}/ui-dump", s.deviceUIDump)
	api.HandleFunc("POST /devices/{id}/settings", s.deviceOpenSettings)
	api.HandleFunc("GET /debug-tunnel/info", s.deviceDebugTunnelInfo)
	api.HandleFunc("POST /uploads", s.createUpload)
	api.HandleFunc("GET /builds", s.listBuilds)
	api.HandleFunc("GET /builds/{id}", s.getBuild)
	api.HandleFunc("GET /builds/{id}/download", s.downloadBuild)
	api.HandleFunc("PATCH /builds/{id}", s.patchBuild)
	api.HandleFunc("DELETE /builds/{id}", s.deleteBuild)
	api.HandleFunc("POST /builds/{id}/install", s.installBuild)
	api.HandleFunc("GET /tokens", s.listTokens)
	api.HandleFunc("POST /tokens", s.createToken)
	api.HandleFunc("DELETE /tokens/{prefix}", s.revokeToken)
	api.HandleFunc("POST /devices/{id}/record/start", s.deviceRecordStart)
	api.HandleFunc("POST /devices/{id}/record/stop", s.deviceRecordStop)
	api.HandleFunc("GET /devices/{id}/record/download", s.deviceRecordDownload)
	api.HandleFunc("GET /devices/{id}/screen", s.screenLink)
	api.HandleFunc("POST /devices/{id}/screen/restart", s.restartScreen)
	api.HandleFunc("POST /devices/{id}/adopt", s.adoptDevice)
	api.HandleFunc("GET /devices/{id}/adopt", s.adoptStatus)
	api.HandleFunc("GET /devices/{id}/job", s.adoptStatus)

	// multi-device batches: reserve a set, install once to all, one grid of screens
	api.HandleFunc("POST /batches", s.batchCreate)
	api.HandleFunc("GET /batches", s.batchList)
	api.HandleFunc("GET /batches/{batch}", s.batchGet)
	api.HandleFunc("POST /batches/{batch}/install", s.batchInstall)
	api.HandleFunc("POST /batches/{batch}/heartbeat", s.batchHeartbeat)
	api.HandleFunc("POST /batches/{batch}/release", s.batchRelease)

	// automated test runs
	api.HandleFunc("POST /runs", s.createRun)
	api.HandleFunc("GET /runs", s.listRuns)
	api.HandleFunc("GET /runs/{id}", s.getRun)
	api.HandleFunc("POST /runs/{id}/cancel", s.cancelRun)
	api.HandleFunc("POST /runs/{id}/rerun", s.rerunRun)
	api.HandleFunc("GET /runs/{id}/artifacts/{device}/{path...}", s.runArtifact)

	// iOS live screen (WebDriverAgent-backed): player page + MJPEG + input.
	ios := http.NewServeMux()
	ios.HandleFunc("GET /ios/{id}", s.iosScreenPage)
	ios.HandleFunc("GET /ios/{id}/size", s.iosSize)
	ios.HandleFunc("GET /ios/{id}/mjpeg", s.iosMJPEG)
	ios.HandleFunc("GET /ios/{id}/frame", s.iosFrame)
	ios.HandleFunc("GET /ios/{id}/state", s.iosState)
	ios.HandleFunc("POST /ios/{id}/input", s.iosInput)
	ios.HandleFunc("GET /ios/{id}/control", s.iosControl)
	ios.HandleFunc("POST /ios/{id}/restart", s.iosRestart)
	ios.HandleFunc("GET /ios/{id}/job", s.iosJob)
	ios.HandleFunc("GET /grid", s.screenGrid)

	mux.Handle("/api/", http.StripPrefix("/api", a.Middleware(api)))
	// agent surface: MCP over streamable HTTP, API tokens only
	mux.Handle("/mcp", s.mcpHandler(a))
	mux.Handle("/live/grid", http.StripPrefix("/live", a.Middleware(ios)))
	mux.Handle("/live/ios/", http.StripPrefix("/live", a.Middleware(ios)))
	mux.Handle("/live/", http.StripPrefix("/live", a.Middleware(s.live.Handler())))
	// embedded assets change on every deploy and are tiny — always revalidate so
	// a redeploy shows immediately (no stale app.css in an iframe).
	fs := http.FileServer(s.web)
	mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache, must-revalidate")
		fs.ServeHTTP(w, r)
	}))
	return logging(s.log, mux)
}

// --- device views ---

type deviceView struct {
	model.Device
	Reservation *model.Reservation `json:"reservation,omitempty"`
	Job         *provision.Job     `json:"job,omitempty"` // active/last adopt job, candidates only
}

func (s *Server) fillView(d model.Device) deviceView {
	dv := deviceView{Device: d}
	if res, ok, _ := s.res.Holder(d.ID); ok {
		dv.Reservation = &res
	}
	if !d.Adopted {
		if j, ok := s.prov.Get(d.ID); ok {
			dv.Job = &j
		}
	}
	return dv
}

func (s *Server) listDevices(w http.ResponseWriter, r *http.Request) {
	pool, err := s.st.Devices()
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	out := make([]deviceView, 0, len(pool))
	for _, d := range pool {
		out = append(out, s.fillView(d))
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) getDevice(w http.ResponseWriter, r *http.Request) {
	d, err := s.st.Device(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	writeJSON(w, http.StatusOK, s.fillView(d))
}

// adoptDevice starts (or re-attaches to) a candidate device's preparation job.
func (s *Server) adoptDevice(w http.ResponseWriter, r *http.Request) {
	job, err := s.prov.Start(r.PathValue("id"))
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// adoptStatus returns the current preparation job for a device.
func (s *Server) adoptStatus(w http.ResponseWriter, r *http.Request) {
	job, ok := s.prov.Get(r.PathValue("id"))
	if !ok {
		fail(w, http.StatusNotFound, errors.New("no preparation job for this device"))
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// --- reservations ---

func (s *Server) reserve(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	res, err := s.res.Reserve(r.PathValue("id"), u.Name)
	switch {
	case errors.Is(err, reserve.ErrTaken):
		fail(w, http.StatusConflict, err)
	case errors.Is(err, reserve.ErrUnavailable):
		fail(w, http.StatusConflict, err)
	case err != nil:
		fail(w, http.StatusInternalServerError, err)
	default:
		s.syncUserTunnel(u.Name)
		writeJSON(w, http.StatusOK, res)
	}
}

func (s *Server) release(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	err := s.res.Release(r.PathValue("id"), u.Name, false)
	if errors.Is(err, reserve.ErrNotHolder) {
		fail(w, http.StatusForbidden, err)
		return
	}
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	s.syncUserTunnel(u.Name)
	writeJSON(w, http.StatusOK, map[string]string{"status": "released"})
}

func (s *Server) heartbeat(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	if err := s.res.Heartbeat(r.PathValue("id"), u.Name); err != nil {
		fail(w, http.StatusForbidden, err)
		return
	}
	s.syncUserTunnel(u.Name) // self-heals the user's tunnel if it died since reserve
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// --- iOS live screen ---

// iosHolder checks the caller holds the device's reservation and iOS screen is
// configured; it writes the error response and returns false on failure.
func (s *Server) iosHolder(w http.ResponseWriter, r *http.Request) (string, bool) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	if !s.ios.Configured(id) {
		fail(w, http.StatusNotImplemented, errors.New("iOS screen not configured for this device"))
		return "", false
	}
	if res, ok, _ := s.res.Holder(id); !ok || res.User != u.Name {
		fail(w, http.StatusForbidden, errors.New("reserve the device first"))
		return "", false
	}
	return id, true
}

func (s *Server) screenGrid(w http.ResponseWriter, r *http.Request) {
	page, err := webui.File("grid.html")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (s *Server) iosScreenPage(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.iosHolder(w, r); !ok {
		return
	}
	page, err := webui.File("ios-screen.html")
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(page)
}

func (s *Server) iosSize(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	wpx, hpx, err := s.ios.Size(id)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"w": wpx, "h": hpx})
}

// iosMJPEG serves the device's screen as multipart/x-mixed-replace, which an
// <img> renders natively — no JavaScript runs per frame and no request is made
// per frame. Frames come from the one shared reader per device, so ten viewers
// still cost the device exactly one mjpeg connection.
func (s *Server) iosMJPEG(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	sub, err := s.ios.Subscribe(id)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	defer sub.Close()

	fl, canFlush := w.(http.Flusher)
	if !canFlush {
		fail(w, http.StatusInternalServerError, errors.New("streaming unsupported"))
		return
	}

	const boundary = "poligonframe"
	w.Header().Set("Content-Type", "multipart/x-mixed-replace; boundary="+boundary)
	w.Header().Set("Cache-Control", "no-store, no-transform")
	w.Header().Set("Connection", "close")
	w.WriteHeader(http.StatusOK)
	fl.Flush()

	for {
		frame, err := sub.Next(r.Context())
		if err != nil {
			return
		}
		if _, err := fmt.Fprintf(w,
			"\r\n--%s\r\nContent-Type: image/jpeg\r\nContent-Length: %d\r\n\r\n",
			boundary, len(frame)); err != nil {
			return
		}
		if _, err := w.Write(frame); err != nil {
			return
		}
		fl.Flush()
	}
}

// iosState reports how fresh the screen is, so the page can say "frozen"
// instead of silently showing a stale frame.
func (s *Server) iosState(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	age, err := s.ios.FrameAge(id)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"live": false, "detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"live":   age < 5*time.Second,
		"age_ms": age.Milliseconds(),
	})
}

// iosFrame returns one JPEG from the device — the polling fallback for browsers
// that will not render a multipart <img> (Safari).
func (s *Server) iosFrame(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	jpg, err := s.ios.Frame(id)
	if err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(jpg)
}

func (s *Server) iosInput(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	var in iosscreen.Input
	if err := json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&in); err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	if err := s.ios.Do(id, in); err != nil {
		fail(w, http.StatusBadGateway, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

// iosRestart tears down and re-creates the device's WebDriverAgent screen.
func (s *Server) iosRestart(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	job, err := s.prov.RestartScreen(id)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// iosJob returns the current provision/restart job for the device.
func (s *Server) iosJob(w http.ResponseWriter, r *http.Request) {
	id, ok := s.iosHolder(w, r)
	if !ok {
		return
	}
	job, jok := s.prov.Get(id)
	if !jok {
		writeJSON(w, http.StatusOK, map[string]string{"state": "none"})
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// restartScreen (bearer-auth, used from the grid) restarts a device's live
// screen: WebDriverAgent for iOS, the scrcpy server for Android.
func (s *Server) restartScreen(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	if res, ok, _ := s.res.Holder(id); !ok || res.User != u.Name {
		fail(w, http.StatusForbidden, errors.New("reserve the device first"))
		return
	}
	job, err := s.prov.RestartScreen(id)
	if err != nil {
		fail(w, http.StatusConflict, err)
		return
	}
	writeJSON(w, http.StatusOK, job)
}

// screenLink returns the live-screen URL for a device the caller may control.
func (s *Server) screenLink(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")
	d, err := s.st.Device(id)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	if res, ok, _ := s.res.Holder(id); !ok || res.User != u.Name {
		fail(w, http.StatusForbidden, errors.New("reserve the device first"))
		return
	}
	if d.Platform == model.IOS {
		if !s.ios.Configured(id) {
			fail(w, http.StatusNotImplemented, errors.New("iOS screen not configured for this device"))
			return
		}
		writeJSON(w, http.StatusOK, map[string]string{"url": "/live/ios/" + id})
		return
	}
	secure := r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https")
	writeJSON(w, http.StatusOK, map[string]string{"url": live.StreamPath(d, r.Host, secure)})
}

// --- install ---

func (s *Server) install(w http.ResponseWriter, r *http.Request) {
	u, _ := auth.UserFrom(r.Context())
	id := r.PathValue("id")

	dev, err := s.st.Device(id)
	if err != nil {
		fail(w, http.StatusNotFound, err)
		return
	}
	// only the holder may install
	if res, ok, _ := s.res.Holder(id); !ok || res.User != u.Name {
		fail(w, http.StatusForbidden, errors.New("reserve the device first"))
		return
	}

	if err := r.ParseMultipartForm(512 << 20); err != nil { // 512 MiB
		fail(w, http.StatusBadRequest, err)
		return
	}
	file, hdr, err := r.FormFile("artifact")
	if err != nil {
		fail(w, http.StatusBadRequest, err)
		return
	}
	defer file.Close()

	dir := filepath.Join(s.cfg.StorageDir, "uploads", time.Now().Format("20060102-150405")+"-"+id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	artifactPath := filepath.Join(dir, filepath.Base(hdr.Filename))
	dst, err := os.Create(artifactPath)
	if err != nil {
		fail(w, http.StatusInternalServerError, err)
		return
	}
	if _, err := io.Copy(dst, file); err != nil {
		dst.Close()
		fail(w, http.StatusInternalServerError, err)
		return
	}
	dst.Close()

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	_ = s.st.SetDeviceStatus(id, model.StatusBusy, time.Now())
	result, ierr := s.inst.Run(ctx, dev, artifactPath,
		install.Origin{User: u.Name, Via: "web", Name: hdr.Filename})

	status, detail := "ok", result.Output
	code := http.StatusOK
	if ierr != nil {
		status, detail = "failed", ierr.Error()
		code = http.StatusBadGateway
	}
	s.log.Info("install", "device", id, "user", u.Name, "artifact", hdr.Filename, "status", status)

	writeJSON(w, code, map[string]any{
		"status":  status,
		"detail":  detail,
		"package": result.Package,
	})
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func fail(w http.ResponseWriter, code int, err error) {
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func logging(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		next.ServeHTTP(sw, r)
		log.Info("http", "method", r.Method, "path", r.URL.Path, "code", sw.code, "dur", time.Since(start).String())
	})
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusWriter) WriteHeader(c int) {
	s.code = c
	s.ResponseWriter.WriteHeader(c)
}

// Hijack lets the WebSocket reverse proxy (/live/) take over the connection.
func (s *statusWriter) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	h, ok := s.ResponseWriter.(http.Hijacker)
	if !ok {
		return nil, nil, errors.New("underlying ResponseWriter is not a Hijacker")
	}
	return h.Hijack()
}

// Flush supports streaming responses.
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
