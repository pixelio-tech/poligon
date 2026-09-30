package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/image/draw"

	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/model"
	"github.com/pancir/poligon/internal/runner"
	"github.com/pancir/poligon/internal/store"
	"github.com/pancir/poligon/internal/uitree"
)

// androidKeys are the keys press_key accepts; iOS maps the few it has.
var androidKeys = map[string]int{
	"back": 4, "home": 3, "enter": 66, "recents": 187, "power": 26,
	"volume_up": 24, "volume_down": 25, "delete": 67, "tab": 61,
	"escape": 111, "menu": 82, "search": 84, "wake": 224,
}

// --- tool inputs (json tag without omitempty = required) ---

type devArg struct {
	DeviceID string `json:"device_id" jsonschema:"device id from list_devices"`
}

type listDevicesIn struct {
	Platform string `json:"platform,omitempty" jsonschema:"android or ios; empty = both"`
	OnlyFree bool   `json:"only_free,omitempty" jsonschema:"only devices you can reserve right now"`
}

type reserveIn struct {
	DeviceID string `json:"device_id,omitempty" jsonschema:"device to reserve; empty = the first free one matching platform/tag"`
	Platform string `json:"platform,omitempty" jsonschema:"android or ios (when device_id is empty)"`
	Tag      string `json:"tag,omitempty" jsonschema:"device tag filter (when device_id is empty)"`
}

type installIn struct {
	DeviceID string `json:"device_id" jsonschema:"a device you hold"`
	UploadID string `json:"upload_id,omitempty" jsonschema:"id from POST /api/uploads (a local .apk/.aab/.apks/.ipa you uploaded)"`
	URL      string `json:"url,omitempty" jsonschema:"http(s) URL of the build; used when upload_id is empty"`
	BuildID  string `json:"build_id,omitempty" jsonschema:"id from list_builds (b_123): a build already on the farm, no upload needed"`
}

type listBuildsIn struct {
	Query    string `json:"query,omitempty" jsonschema:"match file name, package, app name, version, uploader or note"`
	Platform string `json:"platform,omitempty" jsonschema:"android or ios"`
	Limit    int    `json:"limit,omitempty" jsonschema:"max rows (default 20)"`
}

type pkgIn struct {
	DeviceID string `json:"device_id" jsonschema:"a device you hold"`
	Package  string `json:"package" jsonschema:"Android package name or iOS bundle id"`
}

type listAppsIn struct {
	DeviceID      string `json:"device_id" jsonschema:"a device you hold"`
	IncludeSystem bool   `json:"include_system,omitempty" jsonschema:"also list system/OEM packages"`
}

type uiTreeIn struct {
	DeviceID string `json:"device_id" jsonschema:"a device you hold"`
	All      bool   `json:"all,omitempty" jsonschema:"include every node, not only ones with text/id or that are tappable/scrollable/editable"`
	Filter   string `json:"filter,omitempty" jsonschema:"only elements whose text/desc/id/value contains this (case-insensitive)"`
}

type selector struct {
	Text  string `json:"text,omitempty" jsonschema:"match element text, content description or value (exact beats substring; case-insensitive)"`
	ID    string `json:"id,omitempty" jsonschema:"match Android resource-id (without package prefix) or iOS accessibility identifier"`
	Index *int   `json:"index,omitempty" jsonschema:"element index as printed by ui_tree"`
}

func (q selector) query() uitree.Query { return uitree.Query{Index: q.Index, Text: q.Text, ID: q.ID} }

type tapIn struct {
	DeviceID string `json:"device_id" jsonschema:"a device you hold"`
	selector
	X      *float64 `json:"x,omitempty" jsonschema:"x in screenshot image space (when no text/id/index)"`
	Y      *float64 `json:"y,omitempty" jsonschema:"y in screenshot image space"`
	HoldMs int      `json:"hold_ms,omitempty" jsonschema:"hold this long for a long press (e.g. 800)"`
}

type swipeIn struct {
	DeviceID   string   `json:"device_id" jsonschema:"a device you hold"`
	Direction  string   `json:"direction,omitempty" jsonschema:"up|down|left|right — the finger's direction across the middle of the screen: up scrolls content toward the end of a list"`
	X1         *float64 `json:"x1,omitempty" jsonschema:"start x (image space), when no direction"`
	Y1         *float64 `json:"y1,omitempty"`
	X2         *float64 `json:"x2,omitempty"`
	Y2         *float64 `json:"y2,omitempty"`
	DurationMs int      `json:"duration_ms,omitempty" jsonschema:"default 300; longer = slower drag, shorter = fling"`
}

type typeIn struct {
	DeviceID string `json:"device_id" jsonschema:"a device you hold"`
	Text     string `json:"text" jsonschema:"text to type; \\n presses Enter. Android: printable ASCII only"`
	selector
	Clear  bool `json:"clear,omitempty" jsonschema:"delete the field's current contents first"`
	Submit bool `json:"submit,omitempty" jsonschema:"press Enter after typing"`
}

type keyIn struct {
	DeviceID string `json:"device_id" jsonschema:"a device you hold"`
	Key      string `json:"key" jsonschema:"back|home|enter|recents|power|volume_up|volume_down|delete|tab|escape|menu|search|wake (iOS: home, enter, volume_up, volume_down, wake, back = swipe from the left edge, recents = app switcher, power = lock)"`
}

type urlIn struct {
	DeviceID string `json:"device_id" jsonschema:"a device you hold"`
	URL      string `json:"url" jsonschema:"deep link (myapp://…) or web URL"`
}

type waitIn struct {
	DeviceID string `json:"device_id" jsonschema:"a device you hold"`
	selector
	Gone     bool `json:"gone,omitempty" jsonschema:"wait for the element to disappear instead"`
	TimeoutS int  `json:"timeout_s,omitempty" jsonschema:"default 10, max 60"`
}

type logsIn struct {
	DeviceID string `json:"device_id" jsonschema:"a device you hold"`
	Grep     string `json:"grep,omitempty" jsonschema:"case-insensitive regexp; keep only matching lines (e.g. 'FATAL|AndroidRuntime|com.my.app')"`
	Lines    int    `json:"lines,omitempty" jsonschema:"last N lines, default 200, max 2000"`
}

type shellIn struct {
	DeviceID string `json:"device_id" jsonschema:"an Android device you hold"`
	Command  string `json:"command" jsonschema:"one adb shell command line, e.g. 'dumpsys activity activities | grep mResumed'"`
}

type startRunIn struct {
	Type         string            `json:"type" jsonschema:"maestro | install_smoke | integration_test | command"`
	DeviceIDs    []string          `json:"device_ids,omitempty" jsonschema:"devices to run on (free, or held by you); empty = pick by platform/count/tag"`
	Platform     string            `json:"platform,omitempty" jsonschema:"android or ios, when device_ids is empty"`
	Count        int               `json:"count,omitempty" jsonschema:"how many free devices to pick, default 1"`
	Tag          string            `json:"tag,omitempty"`
	App          []string          `json:"app,omitempty" jsonschema:"app build(s): upload ids (u_…) or http(s) URLs, one per platform"`
	TestApp      []string          `json:"test_app,omitempty" jsonschema:"integration_test: the androidTest apk, and/or for iOS a .zip of the Build/Products folder from xcodebuild build-for-testing (the app is inside it); upload id or URL"`
	FlowYAML     string            `json:"flow_yaml,omitempty" jsonschema:"maestro: the flow itself, inline YAML (appId header, ---, commands)"`
	Flow         string            `json:"flow,omitempty" jsonschema:"maestro: upload id or URL of a .yaml or a zipped .maestro workspace (instead of flow_yaml)"`
	FlowPath     string            `json:"flow_path,omitempty" jsonschema:"maestro: flow to run inside a zipped workspace"`
	Env          map[string]string `json:"env,omitempty" jsonschema:"maestro: -e KEY=VALUE"`
	IncludeTags  string            `json:"include_tags,omitempty"`
	ExcludeTags  string            `json:"exclude_tags,omitempty"`
	Command      string            `json:"command,omitempty" jsonschema:"command run: host command line run once per device"`
	WatchSeconds int               `json:"watch_seconds,omitempty" jsonschema:"install_smoke: how long the app must stay up"`
	TimeoutS     int               `json:"timeout_seconds,omitempty" jsonschema:"per-device cap, default 1200"`
	WaitSeconds  int               `json:"wait_seconds,omitempty" jsonschema:"block up to this long for the result (default 45, max 600; 0 still waits 45 — poll get_run after)"`
}

type runIn struct {
	RunID       string `json:"run_id"`
	WaitSeconds int    `json:"wait_seconds,omitempty" jsonschema:"block until the run finishes or this many seconds pass (max 600)"`
}

type artifactIn struct {
	RunID    string `json:"run_id"`
	DeviceID string `json:"device_id"`
	Path     string `json:"path" jsonschema:"artifact name as listed by get_run, e.g. logcat.txt or maestro/screenshot-1.png"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"text: return at most this many bytes from the end, default 20000"`
}

type listRunsIn struct {
	Limit int `json:"limit,omitempty" jsonschema:"default 10"`
}

// --- registration ---

func (s *Server) addMCPTools(srv *mcp.Server) {
	add := func(name, desc string) *mcp.Tool { return &mcp.Tool{Name: name, Description: desc} }
	ro := func(t *mcp.Tool) *mcp.Tool {
		t.Annotations = &mcp.ToolAnnotations{ReadOnlyHint: true}
		return t
	}

	mcp.AddTool(srv, ro(add("list_devices", "List farm phones with platform, model, OS, screen, status and who holds them.")), s.toolListDevices)
	mcp.AddTool(srv, add("reserve_device", "Reserve a phone for yourself (a specific one, or the first free by platform/tag). Required before any other device tool. Wakes the screen."), s.toolReserve)
	mcp.AddTool(srv, add("release_device", "Give a reserved phone back to the pool. Always do this when finished."), s.toolRelease)

	mcp.AddTool(srv, add("install_app", "Install a build (.apk/.aab/.apks on Android, .ipa on iOS) on a phone you hold, by build_id (from list_builds), upload_id or URL. Can take a few minutes."), s.toolInstall)
	mcp.AddTool(srv, ro(add("list_builds", "Builds already installed on the farm by anyone, newest first: id, app, version, who uploaded it and when. Reinstall one with install_app build_id.")), s.toolListBuilds)
	mcp.AddTool(srv, ro(add("list_apps", "List installed packages (Android).")), s.toolListApps)
	mcp.AddTool(srv, add("launch_app", "Start an app by Android package or iOS bundle id."), s.toolLaunch)
	mcp.AddTool(srv, add("stop_app", "Force-stop (Android) / terminate (iOS) an app."), s.toolStop)
	mcp.AddTool(srv, add("clear_app_data", "Wipe an Android app's data and cache — a fresh-install state without reinstalling."), s.toolClearData)
	mcp.AddTool(srv, add("uninstall_app", "Uninstall an Android app."), s.toolUninstall)

	mcp.AddTool(srv, ro(add("screenshot", "Capture the screen as an image. All x/y coordinates in other tools are in this image's pixel space.")), s.toolScreenshot)
	mcp.AddTool(srv, ro(add("ui_tree", "List on-screen elements: [index] class \"text\" id=… @(center x,y) size flags. Cheaper and more exact than a screenshot for finding what to tap.")), s.toolUITree)
	mcp.AddTool(srv, add("tap", "Tap an element found by text / id / index (preferred), or a point x,y in screenshot space. hold_ms makes it a long press."), s.toolTap)
	mcp.AddTool(srv, add("swipe", "Swipe/scroll: a direction across the screen middle, or explicit x1,y1 → x2,y2 in screenshot space."), s.toolSwipe)
	mcp.AddTool(srv, add("type_text", "Type into the focused field, or first tap the field given by text/id/index. clear empties it first, submit presses Enter."), s.toolType)
	mcp.AddTool(srv, add("press_key", "Press a hardware/navigation key: back, home, enter, recents, …"), s.toolKey)
	mcp.AddTool(srv, add("open_url", "Open a deep link or web URL on the device."), s.toolOpenURL)
	mcp.AddTool(srv, ro(add("wait_for", "Wait until an element (text/id) appears — or disappears with gone=true. Use after actions that load or animate.")), s.toolWaitFor)

	mcp.AddTool(srv, ro(add("get_logs", "Device log since the previous get_logs call (logcat on Android, syslog on iOS), filtered and tailed.")), s.toolLogs)
	mcp.AddTool(srv, add("shell", "Run one adb shell command on an Android phone you hold (30s limit)."), s.toolShell)

	mcp.AddTool(srv, add("start_run", "Start an automated test run (maestro flow, install smoke test, integration_test, host command) and wait up to wait_seconds for its result."), s.toolStartRun)
	mcp.AddTool(srv, ro(add("get_run", "Status and per-device results of a run, with artifact names; wait_seconds blocks until it finishes.")), s.toolGetRun)
	mcp.AddTool(srv, ro(add("get_run_artifact", "Read one run artifact: text files (log tail) or images.")), s.toolRunArtifact)
	mcp.AddTool(srv, add("cancel_run", "Cancel one of your active runs."), s.toolCancelRun)
	mcp.AddTool(srv, ro(add("list_runs", "Your most recent runs.")), s.toolListRuns)
}

// --- devices & reservations ---

func (s *Server) toolListDevices(ctx context.Context, _ *mcp.CallToolRequest, in listDevicesIn) (*mcp.CallToolResult, any, error) {
	user, err := agentUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	pool, err := s.st.Devices()
	if err != nil {
		return nil, nil, err
	}
	var b strings.Builder
	n := 0
	for _, d := range pool {
		if !d.Adopted || (in.Platform != "" && string(d.Platform) != in.Platform) {
			continue
		}
		state := string(d.Status)
		if res, ok, _ := s.res.Holder(d.ID); ok {
			if res.User == user {
				state = "held by you"
			} else {
				state = "held by " + res.User
			}
		}
		if in.OnlyFree && d.Status != model.StatusFree {
			continue
		}
		n++
		osv := d.Specs.OSVersion
		if d.Specs.APILevel != "" {
			osv += " (API " + d.Specs.APILevel + ")"
		}
		fmt.Fprintf(&b, "%s  %s  %s %s  %s  %s  %s", d.ID, d.Platform,
			d.Specs.Manufacturer, d.Specs.Model, osv, d.Specs.ScreenSize, state)
		if len(d.Tags) > 0 {
			fmt.Fprintf(&b, "  tags=%s", strings.Join(d.Tags, ","))
		}
		b.WriteByte('\n')
	}
	if n == 0 {
		return text("no devices match"), nil, nil
	}
	return text("%s", b.String()), nil, nil
}

func (s *Server) toolReserve(ctx context.Context, _ *mcp.CallToolRequest, in reserveIn) (*mcp.CallToolResult, any, error) {
	user, err := agentUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	id := in.DeviceID
	if id == "" {
		ids, err := s.selectDevices(in.Platform, in.Tag, "1")
		if err != nil {
			return nil, nil, err
		}
		id = ids[0]
	}
	if _, err := s.res.Reserve(id, user); err != nil {
		return nil, nil, fmt.Errorf("reserve %s: %w", id, err)
	}
	s.syncUserTunnel(user)
	dev, err := s.st.Device(id)
	if err != nil {
		return nil, nil, err
	}
	note := ""
	wctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	if err := s.capt.Wake(wctx, dev); err != nil {
		note = "\nwarning: could not wake the screen: " + err.Error()
	}
	return text("reserved %s (%s %s %s, %s). Keep calling tools to hold it; release_device when done.%s",
		dev.ID, dev.Platform, dev.Specs.Manufacturer, dev.Specs.Model, dev.Specs.OSVersion, note), nil, nil
}

func (s *Server) toolRelease(ctx context.Context, _ *mcp.CallToolRequest, in devArg) (*mcp.CallToolResult, any, error) {
	user, err := agentUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	if err := s.res.Release(in.DeviceID, user, false); err != nil {
		return nil, nil, fmt.Errorf("release %s: %w", in.DeviceID, err)
	}
	s.syncUserTunnel(user)
	s.agent.mu.Lock()
	delete(s.agent.beats, in.DeviceID)
	s.agent.mu.Unlock()
	return text("released %s", in.DeviceID), nil, nil
}

// --- apps ---

// resolveBuild turns an upload id or URL into a local file under dir.
func (s *Server) resolveBuild(ctx context.Context, user, ref, dir string) (string, error) {
	if strings.HasPrefix(ref, "u_") {
		return s.stagedUpload(user, ref)
	}
	if strings.HasPrefix(ref, "http://") || strings.HasPrefix(ref, "https://") {
		base := filepath.Base(ref)
		if q := strings.IndexAny(base, "?#"); q >= 0 {
			base = base[:q]
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		return s.downloadArtifact(ctx, ref, dir, base)
	}
	return "", fmt.Errorf("%q is neither an upload id (u_…) nor an http(s) URL", ref)
}

func (s *Server) newUploadDir(suffix string) string {
	return filepath.Join(s.cfg.StorageDir, "uploads", time.Now().Format("20060102-150405")+"-"+suffix)
}

func (s *Server) toolInstall(ctx context.Context, _ *mcp.CallToolRequest, in installIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	user, _ := agentUser(ctx)
	origin := install.Origin{User: user, Via: "mcp", URL: in.URL}
	var path string
	if in.BuildID != "" {
		b, err := s.libraryBuild(in.BuildID)
		if err != nil {
			return nil, nil, err
		}
		if path, err = s.lib.File(b); err != nil {
			return nil, nil, err
		}
		origin.URL, origin.Ref, origin.Name = "", "b_"+strconv.FormatInt(b.ID, 10), b.Filename
	} else {
		ref := in.UploadID
		if ref == "" {
			ref = in.URL
		}
		if ref == "" {
			return nil, nil, errors.New("pass build_id, upload_id or url")
		}
		if path, err = s.resolveBuild(ctx, user, ref, s.newUploadDir(dev.ID)); err != nil {
			return nil, nil, err
		}
	}
	if p := platformForExt(path); p != dev.Platform {
		return nil, nil, fmt.Errorf("%s is not a %s build (.apk/.aab/.apks for Android, .ipa for iOS)", filepath.Base(path), dev.Platform)
	}
	ictx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	_ = s.st.SetDeviceStatus(dev.ID, model.StatusBusy, time.Now())
	res, err := s.inst.Run(ictx, dev, path, origin)
	s.log.Info("install", "device", dev.ID, "user", user, "artifact", filepath.Base(path), "via", "mcp", "ok", err == nil)
	s.agentBeat(dev.ID, user)
	if err != nil {
		return nil, nil, fmt.Errorf("install failed: %w", err)
	}
	return text("installed %s %s on %s (the installer also launched it)", res.Package, res.Version, dev.ID), nil, nil
}

func (s *Server) toolListApps(ctx context.Context, _ *mcp.CallToolRequest, in listAppsIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	pkgs, err := s.capt.ListPackages(ctx, dev, in.IncludeSystem)
	if err != nil {
		return nil, nil, err
	}
	names := make([]string, 0, len(pkgs))
	for _, p := range pkgs {
		names = append(names, p.Name)
	}
	slices.Sort(names)
	return text("%d packages:\n%s", len(names), strings.Join(names, "\n")), nil, nil
}

func (s *Server) pkgTool(verb string, fn func(context.Context, model.Device, string) error) mcp.ToolHandlerFor[pkgIn, any] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in pkgIn) (*mcp.CallToolResult, any, error) {
		dev, err := s.agentDevice(ctx, in.DeviceID)
		if err != nil {
			return nil, nil, err
		}
		if in.Package == "" {
			return nil, nil, errors.New("package is required")
		}
		cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
		if err := fn(cctx, dev, in.Package); err != nil {
			return nil, nil, err
		}
		return text("%s %s on %s", verb, in.Package, dev.ID), nil, nil
	}
}

func (s *Server) toolLaunch(ctx context.Context, r *mcp.CallToolRequest, in pkgIn) (*mcp.CallToolResult, any, error) {
	return s.pkgTool("launched", s.capt.LaunchApp)(ctx, r, in)
}

func (s *Server) toolStop(ctx context.Context, r *mcp.CallToolRequest, in pkgIn) (*mcp.CallToolResult, any, error) {
	return s.pkgTool("stopped", s.capt.ForceStopApp)(ctx, r, in)
}

func (s *Server) toolClearData(ctx context.Context, r *mcp.CallToolRequest, in pkgIn) (*mcp.CallToolResult, any, error) {
	return s.pkgTool("cleared data of", s.capt.ClearAppData)(ctx, r, in)
}

func (s *Server) toolUninstall(ctx context.Context, r *mcp.CallToolRequest, in pkgIn) (*mcp.CallToolResult, any, error) {
	return s.pkgTool("uninstalled", func(ctx context.Context, d model.Device, p string) error {
		_, err := s.capt.UninstallApp(ctx, d, p)
		return err
	})(ctx, r, in)
}

// --- screen ---

func (s *Server) toolScreenshot(ctx context.Context, _ *mcp.CallToolRequest, in devArg) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	img, w, h, err := s.agentScreenshot(ctx, dev)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: img, MIMEType: "image/jpeg"},
		&mcp.TextContent{Text: fmt.Sprintf("%s screen, image %dx%d — tap/swipe coordinates use this space", dev.ID, w, h)},
	}}, nil, nil
}

// screenElements dumps and parses the current screen.
func (s *Server) screenElements(ctx context.Context, dev model.Device) ([]uitree.Element, error) {
	raw, err := s.capt.UIDump(ctx, dev)
	if err != nil {
		return nil, fmt.Errorf("ui dump: %w", err)
	}
	return uitree.Parse(raw)
}

func (s *Server) toolUITree(ctx context.Context, _ *mcp.CallToolRequest, in uiTreeIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	els, err := s.screenElements(ctx, dev)
	if err != nil {
		return nil, nil, err
	}
	scale, err := s.agentScale(ctx, dev)
	if err != nil {
		return nil, nil, err
	}
	if !in.All {
		sw, sh := 0, 0
		if dev.Platform == model.Android && len(els) > 0 {
			sw, sh = els[0].X+els[0].W, els[0].Y+els[0].H
		}
		els = uitree.Interesting(els, sw, sh)
	}
	f := strings.ToLower(in.Filter)
	var b strings.Builder
	n := 0
	for _, e := range els {
		if f != "" && !strings.Contains(strings.ToLower(e.Text+"\x00"+e.Desc+"\x00"+e.ID+"\x00"+e.Value), f) {
			continue
		}
		n++
		b.WriteString(uitree.Format(e, scale))
		b.WriteByte('\n')
	}
	if n == 0 {
		return text("no matching elements (screen may be a canvas/game/WebView without accessibility, or still loading — try screenshot)"), nil, nil
	}
	return text("%d elements (coordinates in screenshot space):\n%s", n, b.String()), nil, nil
}

// findElement resolves a selector on the current screen, with a useful error.
func (s *Server) findElement(ctx context.Context, dev model.Device, q uitree.Query) (uitree.Element, string, error) {
	els, err := s.screenElements(ctx, dev)
	if err != nil {
		return uitree.Element{}, "", err
	}
	e, n, ok := uitree.Find(els, q)
	if !ok {
		var seen []string
		for _, v := range uitree.Interesting(els, 0, 0) {
			if t := v.Text; t != "" && len(seen) < 25 {
				seen = append(seen, fmt.Sprintf("%q", t))
			}
		}
		return uitree.Element{}, "", fmt.Errorf("no element %s on screen; visible texts: %s", q, strings.Join(seen, ", "))
	}
	note := ""
	if n > 1 {
		note = fmt.Sprintf(" (%d matches — picked the first tappable; pass index= to choose)", n)
	}
	return e, note, nil
}

func (s *Server) toolTap(ctx context.Context, _ *mcp.CallToolRequest, in tapIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	scale, err := s.agentScale(ctx, dev)
	if err != nil {
		return nil, nil, err
	}
	hold := time.Duration(clampInt(in.HoldMs, 0, 0, 5000)) * time.Millisecond
	var nx, ny float64
	what := ""
	switch q := in.query(); {
	case !q.Empty():
		e, note, err := s.findElement(ctx, dev, q)
		if err != nil {
			return nil, nil, err
		}
		cx, cy := e.Center()
		nx, ny = float64(cx), float64(cy)
		what = uitree.Format(e, scale) + note
	case in.X != nil && in.Y != nil:
		nx, ny = *in.X/scale, *in.Y/scale
		what = fmt.Sprintf("(%.0f,%.0f)", *in.X, *in.Y)
	default:
		return nil, nil, errors.New("pass text, id or index — or x and y")
	}
	if err := s.capt.Tap(ctx, dev, nx, ny, hold); err != nil {
		return nil, nil, err
	}
	verb := "tapped"
	if hold > 0 {
		verb = "long-pressed"
	}
	return text("%s %s", verb, what), nil, nil
}

func (s *Server) toolSwipe(ctx context.Context, _ *mcp.CallToolRequest, in swipeIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	scale, err := s.agentScale(ctx, dev)
	if err != nil {
		return nil, nil, err
	}
	d := time.Duration(clampInt(in.DurationMs, 300, 50, 5000)) * time.Millisecond
	var x1, y1, x2, y2 float64
	if in.Direction != "" {
		w, h, err := s.nativeSize(ctx, dev)
		if err != nil {
			return nil, nil, err
		}
		fw, fh := float64(w), float64(h)
		cx, cy := fw/2, fh/2
		switch in.Direction {
		case "up":
			x1, y1, x2, y2 = cx, fh*0.75, cx, fh*0.25
		case "down":
			x1, y1, x2, y2 = cx, fh*0.25, cx, fh*0.75
		case "left":
			x1, y1, x2, y2 = fw*0.85, cy, fw*0.15, cy
		case "right":
			x1, y1, x2, y2 = fw*0.15, cy, fw*0.85, cy
		default:
			return nil, nil, fmt.Errorf("direction must be up, down, left or right")
		}
	} else {
		if in.X1 == nil || in.Y1 == nil || in.X2 == nil || in.Y2 == nil {
			return nil, nil, errors.New("pass direction, or all of x1,y1,x2,y2")
		}
		x1, y1, x2, y2 = *in.X1/scale, *in.Y1/scale, *in.X2/scale, *in.Y2/scale
	}
	if err := s.capt.Swipe(ctx, dev, x1, y1, x2, y2, d); err != nil {
		return nil, nil, err
	}
	return text("swiped (%.0f,%.0f) → (%.0f,%.0f) in %s", x1*scale, y1*scale, x2*scale, y2*scale, d), nil, nil
}

func (s *Server) toolType(ctx context.Context, _ *mcp.CallToolRequest, in typeIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	var done []string
	clearN := 64
	if q := in.query(); !q.Empty() {
		e, _, err := s.findElement(ctx, dev, q)
		if err != nil {
			return nil, nil, err
		}
		cx, cy := e.Center()
		if err := s.capt.Tap(ctx, dev, float64(cx), float64(cy), 0); err != nil {
			return nil, nil, err
		}
		time.Sleep(300 * time.Millisecond) // let the keyboard come up
		clearN = max(len([]rune(e.Text+e.Value))+8, 16)
		done = append(done, "focused "+e.Class)
	}
	if in.Clear {
		if err := s.capt.ClearField(ctx, dev, clearN); err != nil {
			return nil, nil, err
		}
		done = append(done, "cleared")
	}
	t := in.Text
	if in.Submit {
		t += "\n"
	}
	if t != "" {
		if err := s.capt.TypeText(ctx, dev, t); err != nil {
			return nil, nil, err
		}
		done = append(done, fmt.Sprintf("typed %q", in.Text))
		if in.Submit {
			done = append(done, "pressed Enter")
		}
	}
	return text("%s", strings.Join(done, ", ")), nil, nil
}

func (s *Server) toolKey(ctx context.Context, _ *mcp.CallToolRequest, in keyIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	code, ok := androidKeys[in.Key]
	if !ok {
		return nil, nil, fmt.Errorf("unknown key %q", in.Key)
	}
	if err := s.capt.PressButton(ctx, dev, in.Key, code); err != nil {
		return nil, nil, err
	}
	return text("pressed %s", in.Key), nil, nil
}

func (s *Server) toolOpenURL(ctx context.Context, _ *mcp.CallToolRequest, in urlIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	if in.URL == "" {
		return nil, nil, errors.New("url is required")
	}
	if err := s.capt.OpenURL(ctx, dev, in.URL); err != nil {
		return nil, nil, err
	}
	return text("opened %s", in.URL), nil, nil
}

func (s *Server) toolWaitFor(ctx context.Context, _ *mcp.CallToolRequest, in waitIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	q := in.query()
	if q.Empty() {
		return nil, nil, errors.New("pass text, id or index")
	}
	timeout := time.Duration(clampInt(in.TimeoutS, 10, 1, 60)) * time.Second
	deadline := time.Now().Add(timeout)
	start := time.Now()
	var lastErr error
	for {
		els, err := s.screenElements(ctx, dev)
		lastErr = err
		if err == nil {
			_, _, found := uitree.Find(els, q)
			if found != in.Gone {
				state := "appeared"
				if in.Gone {
					state = "is gone"
				}
				return text("%s %s after %s", q, state, time.Since(start).Round(100*time.Millisecond)), nil, nil
			}
		}
		if time.Now().After(deadline) {
			state := "did not appear"
			if in.Gone {
				state = "is still on screen"
			}
			if lastErr != nil {
				return nil, nil, fmt.Errorf("%s %s within %s (last UI dump failed: %v)", q, state, timeout, lastErr)
			}
			return nil, nil, fmt.Errorf("%s %s within %s", q, state, timeout)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(700 * time.Millisecond):
		}
	}
}

// --- diagnostics ---

func (s *Server) toolLogs(ctx context.Context, _ *mcp.CallToolRequest, in logsIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	var re *regexp.Regexp
	if in.Grep != "" {
		if re, err = regexp.Compile("(?i)" + in.Grep); err != nil {
			return nil, nil, fmt.Errorf("grep: %w", err)
		}
	}
	lctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	raw, err := s.capt.Logcat(lctx, dev)
	if err != nil {
		return nil, nil, err
	}
	if re != nil {
		var keep []string
		for _, l := range strings.Split(raw, "\n") {
			if re.MatchString(l) {
				keep = append(keep, l)
			}
		}
		raw = strings.Join(keep, "\n")
	}
	if strings.TrimSpace(raw) == "" {
		return text("no log lines (since the previous get_logs)"), nil, nil
	}
	out, total := tail(raw, clampInt(in.Lines, 200, 1, 2000))
	return text("%d lines, showing the last %d:\n%s", total, min(total, clampInt(in.Lines, 200, 1, 2000)), out), nil, nil
}

func (s *Server) toolShell(ctx context.Context, _ *mcp.CallToolRequest, in shellIn) (*mcp.CallToolResult, any, error) {
	dev, err := s.agentDevice(ctx, in.DeviceID)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(in.Command) == "" {
		return nil, nil, errors.New("command is required")
	}
	sctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	out, err := s.capt.Exec(sctx, dev, in.Command)
	out, _ = tail(out, 400)
	if len(out) > 40000 {
		out = "…" + out[len(out)-40000:]
	}
	if err != nil {
		return nil, nil, fmt.Errorf("%w\n%s", err, out)
	}
	return text("%s", out), nil, nil
}

// --- runs ---

func (s *Server) toolStartRun(ctx context.Context, _ *mcp.CallToolRequest, in startRunIn) (*mcp.CallToolResult, any, error) {
	user, err := agentUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	if !runner.Types[in.Type] {
		return nil, nil, fmt.Errorf("unknown run type %q (maestro, install_smoke, integration_test, command)", in.Type)
	}
	dir := s.newUploadDir("run")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, nil, err
	}
	spec := model.RunSpec{
		Artifacts: map[model.Platform]string{}, TestArtifacts: map[model.Platform]string{},
		IncludeTags: in.IncludeTags, ExcludeTags: in.ExcludeTags, Command: in.Command,
		WatchSeconds: in.WatchSeconds, TimeoutSeconds: in.TimeoutS,
	}
	builds := func(refs []string, into map[model.Platform]string, test bool) error {
		for _, ref := range refs {
			p, err := s.resolveBuild(ctx, user, ref, dir)
			if err != nil {
				return err
			}
			plat := platformForExt(p)
			if test {
				plat = testPlatformForExt(p)
			}
			if plat == "" {
				if test {
					return fmt.Errorf("%s: want an androidTest .apk, or a .zip of iOS Build/Products", filepath.Base(p))
				}
				return fmt.Errorf("%s: not a .apk/.aab/.apks/.ipa", filepath.Base(p))
			}
			if _, dup := into[plat]; dup {
				return fmt.Errorf("more than one %s build", plat)
			}
			into[plat] = p
		}
		return nil
	}
	if err := builds(in.App, spec.Artifacts, false); err != nil {
		return nil, nil, err
	}
	if err := builds(in.TestApp, spec.TestArtifacts, true); err != nil {
		return nil, nil, err
	}

	switch {
	case in.FlowYAML != "":
		spec.FlowPath = filepath.Join(dir, "flow.yaml")
		if err := os.WriteFile(spec.FlowPath, []byte(in.FlowYAML), 0o644); err != nil {
			return nil, nil, err
		}
	case strings.HasPrefix(in.Flow, "u_"):
		if spec.FlowPath, err = s.stagedUpload(user, in.Flow); err != nil {
			return nil, nil, err
		}
	case in.Flow != "":
		if spec.FlowPath, err = s.downloadArtifact(ctx, in.Flow, dir, "flow.yaml"); err != nil {
			return nil, nil, err
		}
	}
	if spec.FlowPath != "" && runner.IsZip(spec.FlowPath) {
		if spec.FlowPath, err = runner.ExtractWorkspace(spec.FlowPath, filepath.Join(dir, "workspace"), in.FlowPath); err != nil {
			return nil, nil, err
		}
	} else if in.FlowPath != "" {
		return nil, nil, errors.New("flow_path needs a zipped workspace as the flow")
	}
	if len(in.Env) > 0 {
		pairs := make([]string, 0, len(in.Env))
		for k, v := range in.Env {
			pairs = append(pairs, k+"="+v)
		}
		if spec.Env, err = parseEnv(pairs); err != nil {
			return nil, nil, err
		}
	}
	if err := checkRunSpec(in.Type, spec); err != nil {
		return nil, nil, err
	}

	devices := in.DeviceIDs
	if len(devices) == 0 {
		if devices, err = s.selectDevices(in.Platform, in.Tag, itoa(clampInt(in.Count, 1, 1, 50))); err != nil {
			return nil, nil, err
		}
	}
	run, err := s.run.Submit(user, in.Type, spec, devices, "mcp")
	if err != nil {
		return nil, nil, err
	}
	return s.waitRun(ctx, user, run.ID, clampInt(in.WaitSeconds, 45, 1, 600))
}

func (s *Server) toolGetRun(ctx context.Context, _ *mcp.CallToolRequest, in runIn) (*mcp.CallToolResult, any, error) {
	user, err := agentUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	return s.waitRun(ctx, user, in.RunID, clampInt(in.WaitSeconds, 0, 0, 600))
}

func runDone(st model.RunStatus) bool {
	return st != model.RunQueued && st != model.RunRunning
}

// waitRun polls a run until it finishes or wait seconds pass, keeping the
// caller's own device holds alive meanwhile, then renders it.
func (s *Server) waitRun(ctx context.Context, user, id string, wait int) (*mcp.CallToolResult, any, error) {
	deadline := time.Now().Add(time.Duration(wait) * time.Second)
	for {
		run, err := s.st.Run(id)
		if err != nil {
			return nil, nil, fmt.Errorf("run %s: %w", id, err)
		}
		for _, d := range run.Devices {
			if res, ok, _ := s.res.Holder(d.DeviceID); ok && res.User == user {
				s.agentBeat(d.DeviceID, user)
			}
		}
		if runDone(run.Status) || !time.Now().Before(deadline) {
			return text("%s", formatRun(run)), nil, nil
		}
		select {
		case <-ctx.Done():
			return text("%s", formatRun(run)), nil, nil
		case <-time.After(2 * time.Second):
		}
	}
}

func formatRun(r model.Run) string {
	var b strings.Builder
	fmt.Fprintf(&b, "run %s  %s  status=%s", r.ID, r.Type, r.Status)
	if r.StartedAt != nil {
		end := time.Now()
		if r.FinishedAt != nil {
			end = *r.FinishedAt
		}
		fmt.Fprintf(&b, "  %s", end.Sub(*r.StartedAt).Round(time.Second))
	}
	if r.Detail != "" {
		fmt.Fprintf(&b, "\n%s", r.Detail)
	}
	for _, d := range r.Devices {
		fmt.Fprintf(&b, "\n- %s (%s): %s", d.DeviceID, d.Platform, d.Status)
		if d.Package != "" {
			fmt.Fprintf(&b, "  package=%s", d.Package)
		}
		if d.Detail != "" {
			fmt.Fprintf(&b, "\n  %s", strings.ReplaceAll(strings.TrimSpace(d.Detail), "\n", "\n  "))
		}
		if len(d.Artifacts) > 0 {
			fmt.Fprintf(&b, "\n  artifacts: %s", strings.Join(d.Artifacts, ", "))
		}
	}
	if !runDone(r.Status) {
		b.WriteString("\n(still going — call get_run with wait_seconds to keep waiting)")
	}
	return b.String()
}

func (s *Server) toolRunArtifact(ctx context.Context, _ *mcp.CallToolRequest, in artifactIn) (*mcp.CallToolResult, any, error) {
	if _, err := agentUser(ctx); err != nil {
		return nil, nil, err
	}
	if strings.ContainsAny(in.RunID, "/\\") || strings.ContainsAny(in.DeviceID, "/\\") ||
		strings.Contains(in.Path, "..") || strings.Contains(in.Path, "\\") || in.Path == "" {
		return nil, nil, errors.New("bad path")
	}
	if _, err := s.st.Run(in.RunID); err != nil {
		return nil, nil, fmt.Errorf("run %s: %w", in.RunID, err)
	}
	root := filepath.Join(s.cfg.StorageDir, "runs")
	path := filepath.Join(root, in.RunID, in.DeviceID, filepath.Clean("/"+in.Path))
	if !strings.HasPrefix(path, root+string(filepath.Separator)) {
		return nil, nil, errors.New("bad path")
	}
	info, err := os.Stat(path)
	if err != nil || info.IsDir() {
		return nil, nil, fmt.Errorf("no artifact %s for %s in run %s", in.Path, in.DeviceID, in.RunID)
	}
	web := fmt.Sprintf("/api/runs/%s/artifacts/%s/%s", in.RunID, in.DeviceID, in.Path)
	switch strings.ToLower(filepath.Ext(path)) {
	case ".png", ".jpg", ".jpeg":
		img, err := shrinkImage(path)
		if err != nil {
			return nil, nil, err
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.ImageContent{Data: img, MIMEType: "image/jpeg"}}}, nil, nil
	case ".mp4", ".zip", ".apk", ".ipa", ".mov":
		return text("%s is binary (%d bytes) — download: GET %s on the farm with your token", in.Path, info.Size(), web), nil, nil
	}
	limit := int64(clampInt(in.MaxBytes, 20000, 1000, 500000))
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, err
	}
	defer f.Close()
	off := max(0, info.Size()-limit)
	buf := make([]byte, info.Size()-off)
	if _, err := f.ReadAt(buf, off); err != nil {
		return nil, nil, err
	}
	head := ""
	if off > 0 {
		head = fmt.Sprintf("[last %d of %d bytes]\n", len(buf), info.Size())
	}
	return text("%s%s", head, buf), nil, nil
}

// shrinkImage re-encodes a stored image as a JPEG no larger than agentMaxEdge.
func shrinkImage(path string) ([]byte, error) {
	f, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	src, _, err := image.Decode(bytes.NewReader(f))
	if err != nil {
		return nil, err
	}
	b := src.Bounds()
	sc := scaleFor(b.Dx(), b.Dy())
	dst := image.NewRGBA(image.Rect(0, 0, int(float64(b.Dx())*sc+0.5), int(float64(b.Dy())*sc+0.5)))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

func (s *Server) toolCancelRun(ctx context.Context, _ *mcp.CallToolRequest, in runIn) (*mcp.CallToolResult, any, error) {
	user, err := agentUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	owner, err := s.st.RunOwner(in.RunID)
	if err != nil {
		return nil, nil, fmt.Errorf("run %s: %w", in.RunID, err)
	}
	if owner != user {
		return nil, nil, errors.New("not your run")
	}
	if !s.run.Cancel(in.RunID) {
		return nil, nil, errors.New("run is not active")
	}
	return text("canceling %s", in.RunID), nil, nil
}

func (s *Server) toolListRuns(ctx context.Context, _ *mcp.CallToolRequest, in listRunsIn) (*mcp.CallToolResult, any, error) {
	user, err := agentUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	runs, _, err := s.st.Runs(store.RunFilter{User: user, Limit: clampInt(in.Limit, 10, 1, 50)})
	if err != nil {
		return nil, nil, err
	}
	if len(runs) == 0 {
		return text("no runs yet"), nil, nil
	}
	var b strings.Builder
	for _, r := range runs {
		devs := make([]string, 0, len(r.Devices))
		for _, d := range r.Devices {
			devs = append(devs, d.DeviceID)
		}
		fmt.Fprintf(&b, "%s  %s  %s  %s  %s\n", r.ID, r.Type, r.Status,
			r.CreatedAt.Format("2006-01-02 15:04"), strings.Join(devs, ","))
	}
	return text("%s", b.String()), nil, nil
}

// libraryBuild resolves a build id ("b_123" or "123") to a live library build.
func (s *Server) libraryBuild(ref string) (model.Build, error) {
	if s.lib == nil {
		return model.Build{}, errors.New("build library is off on this farm")
	}
	id, err := strconv.ParseInt(strings.TrimPrefix(ref, "b_"), 10, 64)
	if err != nil {
		return model.Build{}, fmt.Errorf("bad build_id %q (want b_123, from list_builds)", ref)
	}
	b, err := s.st.Build(id)
	if err != nil || b.DeletedAt != nil {
		return model.Build{}, fmt.Errorf("build %s not found", ref)
	}
	return b, nil
}

func (s *Server) toolListBuilds(ctx context.Context, _ *mcp.CallToolRequest, in listBuildsIn) (*mcp.CallToolResult, any, error) {
	if s.lib == nil {
		return nil, nil, errors.New("build library is off on this farm")
	}
	if in.Limit <= 0 {
		in.Limit = 20
	}
	list, err := s.st.ListBuilds(store.BuildFilter{Q: in.Query, Platform: in.Platform, Limit: in.Limit})
	if err != nil {
		return nil, nil, err
	}
	if len(list) == 0 {
		return text("no builds match"), nil, nil
	}
	var b strings.Builder
	for _, x := range list {
		app := x.Package
		if x.AppName != "" {
			app = x.AppName + " (" + x.Package + ")"
		}
		ver := x.Version
		if x.BuildCode != "" {
			ver += " (" + x.BuildCode + ")"
		}
		fmt.Fprintf(&b, "b_%d  %s  %s  %s  %s — uploaded by %s %s, installed %d×",
			x.ID, x.Platform, x.Filename, app, ver, x.UploadedBy, x.UploadedAt.Format("2006-01-02 15:04"), x.Installs)
		if x.Note != "" {
			fmt.Fprintf(&b, " — note: %s", x.Note)
		}
		b.WriteString("\n")
	}
	return text("%s", b.String()), nil, nil
}
