package capture

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/model"
)

// Control input for driving a device from code (the MCP agent surface).
// Coordinates are the platform's native touch space: pixels on Android,
// points on iOS.

// Tap touches one point; hold > 0 makes it a long press.
func (c *Capturer) Tap(ctx context.Context, dev model.Device, x, y float64, hold time.Duration) error {
	switch dev.Platform {
	case model.Android:
		if hold > 0 {
			xi, yi := int(x), int(y)
			return c.adb.Swipe(ctx, dev.Serial, xi, yi, xi, yi, int(hold.Milliseconds()))
		}
		return c.adb.Tap(ctx, dev.Serial, int(x), int(y))
	case model.IOS:
		if err := c.iosUp(dev); err != nil {
			return err
		}
		if hold > 0 {
			return c.ios.Hold(dev.ID, x, y, hold)
		}
		return c.ios.Do(dev.ID, iosscreen.Input{Type: "tap", X: x, Y: y})
	}
	return fmt.Errorf("input unsupported on %s", dev.Platform)
}

// Swipe drags from (x1,y1) to (x2,y2) over d.
func (c *Capturer) Swipe(ctx context.Context, dev model.Device, x1, y1, x2, y2 float64, d time.Duration) error {
	switch dev.Platform {
	case model.Android:
		return c.adb.Swipe(ctx, dev.Serial, int(x1), int(y1), int(x2), int(y2), int(d.Milliseconds()))
	case model.IOS:
		if err := c.iosUp(dev); err != nil {
			return err
		}
		return c.ios.Do(dev.ID, iosscreen.Input{Type: "swipe", X: x1, Y: y1, X2: x2, Y2: y2, Duration: d.Seconds()})
	}
	return fmt.Errorf("input unsupported on %s", dev.Platform)
}

// TypeText types into the focused field; "\n" presses Enter/Return.
func (c *Capturer) TypeText(ctx context.Context, dev model.Device, text string) error {
	switch dev.Platform {
	case model.Android:
		return c.adb.InputText(ctx, dev.Serial, text)
	case model.IOS:
		if err := c.iosUp(dev); err != nil {
			return err
		}
		return c.ios.Do(dev.ID, iosscreen.Input{Type: "text", Text: text})
	}
	return fmt.Errorf("input unsupported on %s", dev.Platform)
}

// OpenURL opens a deep link or web page on the device.
func (c *Capturer) OpenURL(ctx context.Context, dev model.Device, url string) error {
	switch dev.Platform {
	case model.Android:
		return c.adb.OpenURL(ctx, dev.Serial, url)
	case model.IOS:
		if err := c.iosUp(dev); err != nil {
			return err
		}
		return c.ios.OpenURL(dev.ID, url)
	}
	return fmt.Errorf("open url unsupported on %s", dev.Platform)
}

// PressButton presses a named button on either platform. iOS has no back,
// power or recents button, so those become their gestures / WDA equivalents:
// back is a swipe in from the left edge, power locks, recents opens the app
// switcher.
func (c *Capturer) PressButton(ctx context.Context, dev model.Device, name string, androidCode int) error {
	switch dev.Platform {
	case model.Android:
		return c.adb.Keyevent(ctx, dev.Serial, androidCode)
	case model.IOS:
		if err := c.iosUp(dev); err != nil {
			return err
		}
		switch name {
		case "home":
			return c.ios.PressButton(dev.ID, "home")
		case "volume_up":
			return c.ios.PressButton(dev.ID, "volumeUp")
		case "volume_down":
			return c.ios.PressButton(dev.ID, "volumeDown")
		case "enter":
			return c.ios.Do(dev.ID, iosscreen.Input{Type: "text", Text: "\n"})
		case "wake":
			return c.ios.Do(dev.ID, iosscreen.Input{Type: "wake"})
		case "back":
			return c.ios.Do(dev.ID, iosscreen.Input{Type: "back"})
		case "power":
			return c.ios.Do(dev.ID, iosscreen.Input{Type: "lock"})
		case "recents":
			return c.ios.Do(dev.ID, iosscreen.Input{Type: "app_switcher"})
		}
		return fmt.Errorf("key %q has no iOS equivalent", name)
	}
	return fmt.Errorf("keys unsupported on %s", dev.Platform)
}

// Exec runs one shell command line on an Android device.
func (c *Capturer) Exec(ctx context.Context, dev model.Device, command string) (string, error) {
	if dev.Platform != model.Android {
		return "", errors.New("shell is Android only")
	}
	return c.adb.Exec(ctx, dev.Serial, command)
}

// ScreenPoints returns an iOS device's screen size in points (its touch space).
func (c *Capturer) ScreenPoints(dev model.Device) (w, h int, err error) {
	if err := c.iosUp(dev); err != nil {
		return 0, 0, err
	}
	return c.ios.Size(dev.ID)
}

func (c *Capturer) iosUp(dev model.Device) error {
	if c.ios == nil || !c.ios.Configured(dev.ID) {
		return fmt.Errorf("iOS live screen is not up for %s", dev.ID)
	}
	return nil
}

// Wake turns the screen on (and lifts a swipe-only lock) so input lands on
// the app, not on a dark or locked screen.
func (c *Capturer) Wake(ctx context.Context, dev model.Device) error {
	switch dev.Platform {
	case model.Android:
		return c.adb.WakeUnlock(ctx, dev.Serial)
	case model.IOS:
		if err := c.iosUp(dev); err != nil {
			return err
		}
		return c.ios.Do(dev.ID, iosscreen.Input{Type: "wake"})
	}
	return nil
}

// ClearField deletes up to n characters from the focused text field.
func (c *Capturer) ClearField(ctx context.Context, dev model.Device, n int) error {
	switch dev.Platform {
	case model.Android:
		return c.adb.ClearField(ctx, dev.Serial, n)
	case model.IOS:
		if err := c.iosUp(dev); err != nil {
			return err
		}
		return c.ios.Do(dev.ID, iosscreen.Input{Type: "text", Text: strings.Repeat("\b", n)})
	}
	return fmt.Errorf("input unsupported on %s", dev.Platform)
}
