package api

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png" // screenshots arrive as PNG
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"golang.org/x/image/draw"

	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/model"
)

// The MCP endpoint (POST /mcp) hands the farm to a coding agent — Claude Code,
// Codex — as tools: reserve a phone, install a build, look at the screen, tap
// and type, read logs, run Maestro. It is stateless streamable HTTP: every
// request authenticates with a personal API token and nothing survives
// between calls except the reservation itself, which each device tool renews.
//
// Coordinates: agents see screenshots scaled so the long edge is at most
// agentMaxEdge, and every x/y an agent sends or receives (tap, swipe,
// ui_tree) is in that same image space. The server maps it to the device's
// native touch space (pixels on Android, points on iOS).

const agentMaxEdge = 1280

const mcpInstructions = `Poligon is a farm of real Android and iOS phones. Typical session:
1. list_devices → reserve_device (you hold it until release_device; the lease renews while you keep calling tools, and lapses after ~15 min of silence).
2. Get the build onto the farm: install_app with url= (http/https), or upload a local file first with
   curl -sF file=@path/to/app.apk -H "Authorization: Bearer $POLIGON_TOKEN" <farm>/api/uploads
   and pass the returned upload_id. Every build ever installed on the farm stays there: list_builds finds
   one (by app, version, uploader) and install_app build_id= reinstalls it with no upload.
3. launch_app → screenshot / ui_tree to see the screen → tap / type_text / swipe / press_key → wait_for to let the UI settle.
   Prefer tap by text or id (from ui_tree) over raw coordinates. Coordinates are always in the screenshot's image space.
4. get_logs for crashes (logcat on Android, syslog on iOS); shell for adb shell one-liners (Android).
5. For a scripted end-to-end check use start_run type=maestro with flow_yaml (inline Maestro YAML); get_run for the result, get_run_artifact for logs/screenshots.
6. release_device when done — other people share these phones.
Android text input is ASCII-only (no Cyrillic); for non-ASCII text use a Maestro flow's inputText.`

// agentState is the little the MCP surface remembers between calls.
type agentState struct {
	mu    sync.Mutex
	size  map[string][2]int    // device id -> native screen w,h (px Android, pt iOS)
	beats map[string]time.Time // device id -> last heartbeat we sent
}

func (s *Server) mcpHandler(a *auth.Auth) http.Handler {
	s.agent = &agentState{size: map[string][2]int{}, beats: map[string]time.Time{}}
	srv := mcp.NewServer(&mcp.Implementation{Name: "poligon", Title: "Poligon device farm", Version: "1"},
		&mcp.ServerOptions{Instructions: mcpInstructions})
	s.addMCPTools(srv)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv },
		&mcp.StreamableHTTPOptions{
			Stateless:    true,
			JSONResponse: true,
			// the farm sits behind `tailscale serve` / Caddy, which proxy from
			// loopback with the public Host — the SDK's DNS-rebinding guard
			// would refuse all of it. Bearer-only auth is the guard here.
			DisableLocalhostProtection: true,
		})
	return a.BearerMiddleware(h)
}

// agentUser is the token owner of the MCP request that ctx belongs to.
func agentUser(ctx context.Context) (string, error) {
	u, ok := auth.UserFrom(ctx)
	if !ok || u.Name == "" {
		return "", errors.New("unauthenticated")
	}
	return u.Name, nil
}

// agentDevice resolves a device the caller holds and renews its lease (at
// most every 30s, so a burst of taps is not a burst of writes).
func (s *Server) agentDevice(ctx context.Context, id string) (model.Device, error) {
	user, err := agentUser(ctx)
	if err != nil {
		return model.Device{}, err
	}
	if id == "" {
		return model.Device{}, errors.New("device_id is required")
	}
	dev, err := s.st.Device(id)
	if err != nil {
		return model.Device{}, fmt.Errorf("no device %q (see list_devices)", id)
	}
	res, ok, _ := s.res.Holder(id)
	if !ok || res.User != user {
		if ok {
			return model.Device{}, fmt.Errorf("%s is held by %s — pick another from list_devices", id, res.User)
		}
		return model.Device{}, fmt.Errorf("%s is not reserved by you — call reserve_device first", id)
	}
	s.agentBeat(id, user)
	return dev, nil
}

func (s *Server) agentBeat(id, user string) {
	s.agent.mu.Lock()
	due := time.Since(s.agent.beats[id]) > 30*time.Second
	if due {
		s.agent.beats[id] = time.Now()
	}
	s.agent.mu.Unlock()
	if due {
		_ = s.res.Heartbeat(id, user)
	}
}

// agentScale is image-space units per native unit for a device (≤ 1). It
// depends on the long edge only, so it does not change with rotation.
func (s *Server) agentScale(ctx context.Context, dev model.Device) (float64, error) {
	w, h, err := s.nativeSize(ctx, dev)
	if err != nil {
		return 0, err
	}
	return scaleFor(w, h), nil
}

func scaleFor(w, h int) float64 {
	if edge := max(w, h); edge > agentMaxEdge {
		return float64(agentMaxEdge) / float64(edge)
	}
	return 1
}

// nativeSize is the device's touch space as last seen (orientation included).
func (s *Server) nativeSize(ctx context.Context, dev model.Device) (int, int, error) {
	s.agent.mu.Lock()
	wh, ok := s.agent.size[dev.ID]
	s.agent.mu.Unlock()
	if ok {
		return wh[0], wh[1], nil
	}
	var w, h int
	switch dev.Platform {
	case model.IOS:
		var err error
		if w, h, err = s.capt.ScreenPoints(dev); err != nil {
			return 0, 0, fmt.Errorf("screen size: %w", err)
		}
	default:
		img, _, err := s.capt.Screenshot(ctx, dev)
		if err != nil {
			return 0, 0, err
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(img))
		if err != nil {
			return 0, 0, fmt.Errorf("screenshot: %w", err)
		}
		w, h = cfg.Width, cfg.Height
	}
	if w <= 0 || h <= 0 {
		return 0, 0, errors.New("device reported an empty screen size")
	}
	s.setNativeSize(dev.ID, w, h)
	return w, h, nil
}

func (s *Server) setNativeSize(id string, w, h int) {
	s.agent.mu.Lock()
	s.agent.size[id] = [2]int{w, h}
	s.agent.mu.Unlock()
}

// agentScreenshot grabs the screen and scales it into image space: native
// size capped at agentMaxEdge on the long side — for iOS that also turns
// retina pixels into points. It refreshes the cached native size, so a
// rotation is picked up by the next screenshot.
func (s *Server) agentScreenshot(ctx context.Context, dev model.Device) ([]byte, int, int, error) {
	raw, _, err := s.capt.Screenshot(ctx, dev)
	if err != nil {
		return nil, 0, 0, err
	}
	src, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return nil, 0, 0, fmt.Errorf("decode screenshot: %w", err)
	}
	b := src.Bounds()
	var nw, nh int
	if dev.Platform == model.Android {
		nw, nh = b.Dx(), b.Dy()
	} else if pw, ph, err := s.capt.ScreenPoints(dev); err == nil {
		// points follow the image's orientation, whatever WDA reported
		nw, nh = min(pw, ph), max(pw, ph)
		if b.Dx() > b.Dy() {
			nw, nh = nh, nw
		}
	} else {
		return nil, 0, 0, fmt.Errorf("screen size: %w", err)
	}
	s.setNativeSize(dev.ID, nw, nh)
	f := scaleFor(nw, nh)
	w, h := int(float64(nw)*f+0.5), int(float64(nh)*f+0.5)
	dst := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(dst, dst.Bounds(), src, b, draw.Src, nil)
	var out bytes.Buffer
	if err := jpeg.Encode(&out, dst, &jpeg.Options{Quality: 80}); err != nil {
		return nil, 0, 0, err
	}
	return out.Bytes(), w, h, nil
}

// text is a one-block text result.
func text(format string, a ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, a...)}}}
}

// tail keeps the last n lines of s.
func tail(s string, n int) (string, int) {
	lines := strings.Split(strings.TrimRight(s, "\n"), "\n")
	total := len(lines)
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n"), total
}

// clampInt bounds v to [lo, hi], using def when v is 0.
func clampInt(v, def, lo, hi int) int {
	if v == 0 {
		v = def
	}
	return max(lo, min(v, hi))
}

func itoa(n int) string { return strconv.Itoa(n) }
