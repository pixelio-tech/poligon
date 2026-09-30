package iosscreen

import (
	"fmt"
	"math"
	"strings"
	"time"
)

// Input is one control action from the dashboard. Coordinates are in points
// (the device's logical screen), the space /size reports.
type Input struct {
	// tap | hold | swipe | path | pinch | text | home | wake | lock |
	// volume_up | volume_down | app_switcher | back | rotate
	Type     string  `json:"type"`
	X        float64 `json:"x"`
	Y        float64 `json:"y"`
	X2       float64 `json:"x2"`
	Y2       float64 `json:"y2"`
	Duration float64 `json:"duration"` // seconds, for swipe / hold
	Text     string  `json:"text"`
	// Points is a drag's whole trail, [x, y, ms since the previous point] each,
	// so a curved or slow drag replays the way the finger moved instead of as a
	// straight line between its ends.
	Points [][3]float64 `json:"points,omitempty"`
	// Scale is a pinch's end distance over its start distance, around X/Y.
	Scale float64 `json:"scale,omitempty"`
}

// Do performs an input action against the device's WDA.
func (c *Controller) Do(deviceID string, in Input) error {
	switch in.Type {
	case "home":
		return c.noSessionPost(deviceID, "/wda/homescreen")
	case "wake":
		// wakes the screen; fully unlocks only if the device has no passcode
		return c.noSessionPost(deviceID, "/wda/unlock")
	case "lock":
		return c.noSessionPost(deviceID, "/wda/lock")
	case "volume_up":
		return c.PressButton(deviceID, "volumeUp")
	case "volume_down":
		return c.PressButton(deviceID, "volumeDown")
	case "text":
		return c.sessionPost(deviceID, "/wda/keys", map[string]any{"value": []rune(in.Text)})
	case "tap":
		return c.sessionPost(deviceID, "/wda/tap", map[string]any{"x": in.X, "y": in.Y})
	case "hold":
		d := in.Duration
		if d <= 0 {
			d = 1
		}
		return c.Hold(deviceID, in.X, in.Y, time.Duration(d*float64(time.Second)))
	case "swipe":
		dur := in.Duration
		if dur == 0 {
			dur = 0.15
		}
		return c.sessionPost(deviceID, "/wda/dragfromtoforduration", map[string]any{
			"fromX": in.X, "fromY": in.Y, "toX": in.X2, "toY": in.Y2, "duration": dur,
		})
	case "path":
		if len(in.Points) < 2 {
			return fmt.Errorf("path needs at least 2 points")
		}
		return c.actions(deviceID, touchPath("f1", in.Points))
	case "pinch":
		return c.pinch(deviceID, in.X, in.Y, in.Scale)
	case "back":
		// iOS has no back button; apps with a navigation stack go back on a
		// swipe in from the left edge
		w, h, err := c.cachedSize(deviceID)
		if err != nil {
			return err
		}
		y := float64(h) / 2
		return c.actions(deviceID, touchPath("f1", [][3]float64{
			{1, y, 0}, {float64(w) * 0.35, y, 120}, {float64(w) * 0.75, y, 120},
		}))
	case "app_switcher":
		return c.appSwitcher(deviceID)
	case "rotate":
		return c.rotate(deviceID)
	default:
		return fmt.Errorf("unknown input type %q", in.Type)
	}
}

// noSessionPost hits a WDA endpoint that lives outside any session.
func (c *Controller) noSessionPost(deviceID, path string) error {
	ep, ok := c.endpoint(deviceID)
	if !ok || ep.WDA == "" {
		return fmt.Errorf("no ios screen endpoint for %q", deviceID)
	}
	return c.post("http://"+ep.WDA+path, nil)
}

// touchPath is one finger's W3C pointer action sequence along points.
func touchPath(id string, pts [][3]float64) map[string]any {
	acts := []map[string]any{
		{"type": "pointerMove", "duration": 0, "x": pts[0][0], "y": pts[0][1]},
		{"type": "pointerDown", "button": 0},
	}
	for _, p := range pts[1:] {
		acts = append(acts, map[string]any{
			"type": "pointerMove", "duration": int(math.Max(0, p[2])), "x": p[0], "y": p[1],
		})
	}
	acts = append(acts, map[string]any{"type": "pointerUp", "button": 0})
	return map[string]any{
		"type": "pointer", "id": id,
		"parameters": map[string]any{"pointerType": "touch"},
		"actions":    acts,
	}
}

func (c *Controller) actions(deviceID string, fingers ...map[string]any) error {
	return c.sessionPost(deviceID, "/actions", map[string]any{"actions": fingers})
}

// pinch moves two fingers apart (scale > 1, zoom in) or together (scale < 1)
// around cx, cy.
func (c *Controller) pinch(deviceID string, cx, cy, scale float64) error {
	if scale <= 0 || scale == 1 {
		return fmt.Errorf("pinch needs a scale other than 1")
	}
	w, _, err := c.cachedSize(deviceID)
	if err != nil {
		return err
	}
	r0 := float64(w) * 0.12
	r1 := math.Min(r0*scale, float64(w)*0.45)
	if scale < 1 {
		r0, r1 = float64(w)*0.35, math.Max(float64(w)*0.35*scale, 12)
	}
	const ms = 350
	return c.actions(deviceID,
		touchPath("f1", [][3]float64{{cx - r0, cy, 0}, {cx - r1, cy, ms}}),
		touchPath("f2", [][3]float64{{cx + r0, cy, 0}, {cx + r1, cy, ms}}),
	)
}

// appSwitcher opens the multitasking view. Phones with a home button get a
// double press; Face ID phones (taller than 16:9) a swipe up from the bottom
// edge that pauses mid-screen.
func (c *Controller) appSwitcher(deviceID string) error {
	w, h, err := c.cachedSize(deviceID)
	if err != nil {
		return err
	}
	if float64(h)/float64(w) < 2 {
		if err := c.PressButton(deviceID, "home"); err != nil {
			return err
		}
		time.Sleep(120 * time.Millisecond)
		return c.PressButton(deviceID, "home")
	}
	x := float64(w) / 2
	return c.actions(deviceID, map[string]any{
		"type": "pointer", "id": "f1",
		"parameters": map[string]any{"pointerType": "touch"},
		"actions": []map[string]any{
			{"type": "pointerMove", "duration": 0, "x": x, "y": float64(h) - 2},
			{"type": "pointerDown", "button": 0},
			{"type": "pointerMove", "duration": 250, "x": x, "y": float64(h) * 0.6},
			{"type": "pause", "duration": 700},
			{"type": "pointerUp", "button": 0},
		},
	})
}

// rotate flips between portrait and landscape. Apps locked to one orientation
// (and the iPhone home screen) ignore it.
func (c *Controller) rotate(deviceID string) error {
	err := c.withSession(deviceID, func(base, sid string) error {
		var cur struct {
			Value string `json:"value"`
		}
		if err := c.get(fmt.Sprintf("%s/session/%s/orientation", base, sid), &cur); err != nil {
			return err
		}
		next := "LANDSCAPE"
		if strings.HasPrefix(strings.ToUpper(cur.Value), "LANDSCAPE") {
			next = "PORTRAIT"
		}
		return c.post(fmt.Sprintf("%s/session/%s/orientation", base, sid),
			map[string]any{"orientation": next})
	})
	c.forgetSize(deviceID)
	return err
}

// cachedSize is Size without the round trip — gestures need the screen size
// and it only changes on rotation.
func (c *Controller) cachedSize(deviceID string) (int, int, error) {
	c.mu.Lock()
	sz, ok := c.sizes[deviceID]
	c.mu.Unlock()
	if ok {
		return sz[0], sz[1], nil
	}
	w, h, err := c.Size(deviceID)
	if err != nil {
		return 0, 0, err
	}
	if w <= 0 || h <= 0 {
		return 0, 0, fmt.Errorf("wda reported screen size %dx%d", w, h)
	}
	c.mu.Lock()
	c.sizes[deviceID] = [2]int{w, h}
	c.mu.Unlock()
	return w, h, nil
}

func (c *Controller) forgetSize(deviceID string) {
	c.mu.Lock()
	delete(c.sizes, deviceID)
	c.mu.Unlock()
}
