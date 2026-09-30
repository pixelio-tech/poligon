package main

import (
	"context"
	"log/slog"
	"net/http"
	"os/exec"
	"time"

	"github.com/pancir/poligon/internal/config"
	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/provision"
	"github.com/pancir/poligon/internal/store"
)

// depsWatchdog periodically logs the health of poligon's out-of-process
// dependencies (ws-scrcpy sidecar, go-ios tunnel, adb), keeps the farm on one
// healthy adb server (adbGuard) and self-heals iOS screens whose
// WebDriverAgent has stopped answering.
func depsWatchdog(ctx context.Context, cfg config.Config, st *store.Store, prov *provision.Manager, screens *iosscreen.Controller, log *slog.Logger) {
	t := time.NewTicker(30 * time.Second)
	defer t.Stop()

	type healState struct {
		fails       int
		healCount   int
		windowStart time.Time
	}
	heal := map[string]*healState{}
	guard := newADBGuard(cfg.ADBPath, cfg.ADBServerAddr, log)

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}

		sidecar := reachable(cfg.LiveSidecar)
		tunnel := exec.Command("ios", "tunnel", "ls").Run() == nil
		adbN := guard.check(ctx)
		if !sidecar || !tunnel {
			log.Warn("deps", "ws_scrcpy", sidecar, "go_ios_tunnel", tunnel, "adb_devices", adbN)
		} else {
			log.Info("deps ok", "ws_scrcpy", sidecar, "go_ios_tunnel", tunnel, "adb_devices", adbN)
		}

		rows, err := st.IOSScreens()
		if err != nil {
			continue
		}
		for _, r := range rows {
			if r.WDA == "" {
				continue
			}
			if screens.InXCTest(r.DeviceID) {
				// a test run owns the phone's XCTest session; the runner
				// brings the screen back when it is done
				delete(heal, r.DeviceID)
				continue
			}
			hs := heal[r.DeviceID]
			if hs == nil {
				hs = &healState{windowStart: time.Now()}
				heal[r.DeviceID] = hs
			}
			if time.Since(hs.windowStart) > 10*time.Minute {
				hs.healCount, hs.windowStart = 0, time.Now()
			}

			// /status alone is not enough: WDA can answer it while its mjpeg
			// server has stopped sending, and the wall then shows a frozen frame
			if probeStatus("http://"+r.WDA+"/status") && !screens.StreamDead(r.DeviceID, 45*time.Second) {
				hs.fails = 0
				continue
			}
			hs.fails++
			if hs.fails < 2 {
				continue
			}
			// don't fight a restart that's already running
			if j, ok := prov.Get(r.DeviceID); ok && j.State == "running" {
				continue
			}
			if hs.healCount >= 3 {
				log.Warn("watchdog: iOS screen still down, giving up for this window", "device", r.DeviceID)
				continue
			}
			hs.healCount++
			hs.fails = 0
			log.Warn("watchdog: iOS screen unresponsive — restarting", "device", r.DeviceID, "attempt", hs.healCount)
			_, _ = prov.RestartScreen(r.DeviceID)
		}
	}
}

func reachable(url string) bool {
	if url == "" {
		return false
	}
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Head(url)
	if err != nil {
		// some servers 405 HEAD — a GET that connects still counts
		resp, err = c.Get(url)
		if err != nil {
			return false
		}
	}
	resp.Body.Close()
	return true
}

func probeStatus(url string) bool {
	c := &http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}
