package iosscreen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

// Source returns the foreground app's accessibility tree as WDA's XML
// (XCUIElementType* nodes with x/y/width/height in points). A busy screen can
// take WDA several seconds to walk, hence the longer timeout than c.client's.
func (c *Controller) Source(ctx context.Context, deviceID string) (string, error) {
	ep, ok := c.endpoint(deviceID)
	if !ok || ep.WDA == "" {
		return "", fmt.Errorf("no ios screen endpoint for %q", deviceID)
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+ep.WDA+"/source?format=xml", nil)
	if err != nil {
		return "", err
	}
	resp, err := c.streamClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return "", fmt.Errorf("wda source: %s: %s", resp.Status, b)
	}
	var out struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(&out); err != nil {
		return "", err
	}
	if out.Value == "" {
		return "", fmt.Errorf("wda returned an empty source")
	}
	return out.Value, nil
}

// Hold presses one point (points) for d.
func (c *Controller) Hold(deviceID string, x, y float64, d time.Duration) error {
	return c.sessionPost(deviceID, "/wda/touchAndHold",
		map[string]any{"x": x, "y": y, "duration": d.Seconds()})
}

// LaunchApp brings an installed app to the foreground (starting it if needed).
func (c *Controller) LaunchApp(deviceID, bundleID string) error {
	return c.sessionPost(deviceID, "/wda/apps/launch", map[string]any{"bundleId": bundleID})
}

// TerminateApp kills an app.
func (c *Controller) TerminateApp(deviceID, bundleID string) error {
	return c.sessionPost(deviceID, "/wda/apps/terminate", map[string]any{"bundleId": bundleID})
}

// OpenURL opens a deep link or web page (Safari for http/https).
func (c *Controller) OpenURL(deviceID, url string) error {
	return c.sessionPost(deviceID, "/url", map[string]any{"url": url})
}

// PressButton presses a hardware button: home, volumeUp or volumeDown.
func (c *Controller) PressButton(deviceID, name string) error {
	return c.sessionPost(deviceID, "/wda/pressButton", map[string]any{"name": name})
}

// sessionPost POSTs to a path under the device's live WDA session.
func (c *Controller) sessionPost(deviceID, path string, payload map[string]any) error {
	return c.withSession(deviceID, func(base, sid string) error {
		return c.post(fmt.Sprintf("%s/session/%s%s", base, sid, path), payload)
	})
}
