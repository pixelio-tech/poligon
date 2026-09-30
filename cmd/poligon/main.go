// Command poligon runs the phone-farm control plane: device polling, the JSON
// API and the dashboard, plus a small CLI for user management.
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/pancir/poligon/internal/adb"
	"github.com/pancir/poligon/internal/api"
	"github.com/pancir/poligon/internal/auth"
	"github.com/pancir/poligon/internal/builds"
	"github.com/pancir/poligon/internal/capture"
	"github.com/pancir/poligon/internal/config"
	"github.com/pancir/poligon/internal/devices"
	"github.com/pancir/poligon/internal/install"
	"github.com/pancir/poligon/internal/ios"
	"github.com/pancir/poligon/internal/iosscreen"
	"github.com/pancir/poligon/internal/live"
	"github.com/pancir/poligon/internal/procgroup"
	"github.com/pancir/poligon/internal/provision"
	"github.com/pancir/poligon/internal/reserve"
	"github.com/pancir/poligon/internal/runner"
	"github.com/pancir/poligon/internal/store"
	"github.com/pancir/poligon/internal/webui"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	cfgPath := envOr("POLIGON_CONFIG", "config/devices.yaml")

	switch os.Args[1] {
	case "serve":
		devFlag := false
		for _, a := range os.Args[2:] {
			if a == "--dev" {
				devFlag = true
			}
		}
		if err := serve(log, cfgPath, devFlag); err != nil {
			log.Error("serve", "err", err)
			os.Exit(1)
		}
	case "user":
		if err := userCmd(log, cfgPath, os.Args[2:]); err != nil {
			log.Error("user", "err", err)
			os.Exit(1)
		}
	case "token":
		if err := tokenCmd(cfgPath, os.Args[2:]); err != nil {
			log.Error("token", "err", err)
			os.Exit(1)
		}
	default:
		usage()
		os.Exit(2)
	}
}

func serve(log *slog.Logger, cfgPath string, devFlag bool) error {
	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(cfg.StorageDir, 0o755); err != nil {
		return err
	}

	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()
	_ = st.PurgeExpiredSessions()

	res := reserve.New(st, st.DB(), cfg.IdleTimeout, cfg.MaxLease)
	mgr, err := devices.New(cfg, st, res.IsHeld, log)
	if err != nil {
		return err
	}
	inst := install.New(adb.New(cfg.ADBPath), ios.Default(), install.Options{
		BundletoolJar:   os.Getenv("POLIGON_BUNDLETOOL"),
		SigningIdentity: os.Getenv("POLIGON_SIGNING_IDENTITY"),
		ProfileDir:      envOr("POLIGON_PROFILE_DIR", "config/profiles"),
		// Xcode's automatic signing (the WebDriverAgent build) keeps the team
		// wildcard profile there — enough to install any build on farm iPhones
		ExtraProfileDirs: install.XcodeProfileDirs(),
		WorkDir:          os.TempDir(),
	})
	lib := builds.New(st, cfg.StorageDir, log)
	inst.SetRecorder(lib)
	devUser := os.Getenv("POLIGON_DEV_USER")
	a := auth.New(st, auth.Options{
		SessionTTL:  cfg.Auth.SessionTTL,
		SessionIdle: cfg.Auth.SessionIdle,
		DevUser:     devUser,
		DevAllow:    devUser != "" && (devFlag || isLoopbackListen(cfg.Listen)),
		Log:         log,
	})
	lp := live.New(cfg.LiveSidecar, st, res, log)

	// iOS screen endpoints: static config plus any persisted by earlier adopts.
	iosEndpoints := iosscreen.ParseEndpoints(cfg.IOSScreen)
	if rows, err := st.IOSScreens(); err == nil {
		for _, rr := range rows {
			iosEndpoints[rr.DeviceID] = iosscreen.Endpoint{WDA: rr.WDA, MJPEG: rr.MJPEG}
		}
	}
	iosCtl := iosscreen.New(iosEndpoints, iosscreen.Tuning{
		Framerate: cfg.IOSWDA.MJPEGFramerate,
		Quality:   cfg.IOSWDA.MJPEGQuality,
		Scale:     cfg.IOSWDA.MJPEGScale,
	})
	prov := provision.New(cfg, st, adb.New(cfg.ADBPath), ios.Default(), iosCtl, log)

	capt := capture.New(adb.New(cfg.ADBPath), iosCtl, ios.Default())
	run := runner.New(st, res, inst, capt, adb.New(cfg.ADBPath), iosCtl,
		filepath.Join(cfg.StorageDir, "runs"), os.Getenv("POLIGON_MAESTRO"), log)
	run.SetScreenRestarter(func(ctx context.Context, id string) error {
		if _, err := prov.RestartScreen(id); err != nil {
			return err
		}
		for {
			j, ok := prov.Get(id)
			if !ok || j.State != "running" {
				if ok && j.State == "failed" {
					return fmt.Errorf("screen restart failed: %s", j.Err)
				}
				return nil
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(2 * time.Second):
			}
		}
	})
	srv := api.New(cfg, st, res, inst, lp, iosCtl, prov, capt, run, http.FS(webui.FS()), log)
	srv.SetLibrary(lib)
	handler := srv.Handler(a)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// with the adb server run by launchd, poligon must never start one itself
	if newADBGuard(cfg.ADBPath, cfg.ADBServerAddr, log).managed() {
		adb.UseManagedServer(cfg.ADBServerAddr)
		log.Info("adb server is managed by " + adbLabel)
	} else {
		log.Warn(adbLabel + " is not installed — adb clients may start stray servers; run scripts/install-all.sh")
	}

	// clear leftovers from a previous poligon before we respawn iOS screens
	prov.ReapOrphans()
	// re-open every active reservation's adb tunnel — a restart drops the
	// in-memory listeners, and nobody's going to reserve/heartbeat again
	// just to fix that mid-debug-session
	srv.SyncADBTunnels()

	go mgr.Run(ctx)
	go reapLoop(ctx, res, st, srv, log)
	go prov.Resume(ctx)
	go depsWatchdog(ctx, cfg, st, prov, iosCtl, log)
	runDone := make(chan struct{})
	go func() { run.Run(ctx); close(runDone) }()

	httpSrv := &http.Server{Addr: cfg.Listen, Handler: handler}
	go func() {
		<-ctx.Done()
		log.Info("shutting down")
		// an in-flight run's context is already canceled: its maestro/command
		// process group gets SIGTERM, then SIGKILL after procgroup.Grace — wait
		// for that, so launchd never has to kill poligon with children alive
		select {
		case <-runDone:
		case <-time.After(procgroup.Grace + 3*time.Second):
			log.Warn("shutdown: runner still busy, moving on")
		}
		prov.Shutdown() // kill WebDriverAgent runners + forwards — no orphans
		capt.Shutdown() // kill any background idevicesyslog captures
		sh, cancel := context.WithTimeout(context.Background(), 8*time.Second)
		defer cancel()
		_ = httpSrv.Shutdown(sh)
	}()

	tlsOn := cfg.TLS.Cert != "" && cfg.TLS.Key != ""
	log.Info("poligon listening", "addr", cfg.Listen, "tls", tlsOn, "devices", len(cfg.Devices))
	var serveErr error
	if tlsOn {
		serveErr = httpSrv.ListenAndServeTLS(cfg.TLS.Cert, cfg.TLS.Key)
	} else {
		serveErr = httpSrv.ListenAndServe()
	}
	if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
		return serveErr
	}
	return nil
}

// isLoopbackListen reports whether a "host:port" listen address is bound to
// localhost only — the one place the POLIGON_DEV_USER bypass is safe without --dev.
func isLoopbackListen(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil || host == "" {
		return false // ":8080" binds every interface
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func reapLoop(ctx context.Context, res *reserve.Manager, st *store.Store, srv *api.Server, log *slog.Logger) {
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	purgeEvery := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if n, err := res.ReapExpired(); err != nil {
				log.Warn("reap", "err", err)
			} else if n > 0 {
				log.Info("reaped stale reservations", "count", n)
			}
			srv.SyncADBTunnels()
			// sweep dead sessions roughly hourly
			if purgeEvery%60 == 0 {
				if err := st.PurgeExpiredSessions(); err != nil {
					log.Warn("purge sessions", "err", err)
				}
			}
			purgeEvery++
		}
	}
}

func userCmd(log *slog.Logger, cfgPath string, args []string) error {
	if len(args) == 0 {
		return errors.New("usage: poligon user <add|list|disable|enable|reset-password> ...")
	}

	cfg, err := config.Load(cfgPath)
	if err != nil {
		return err
	}
	st, err := store.Open(cfg.DBPath)
	if err != nil {
		return err
	}
	defer st.Close()

	a := auth.New(st, auth.Options{Log: log})

	switch args[0] {
	case "add":
		if len(args) < 2 {
			return errors.New("usage: poligon user add <email>")
		}
		email := args[1]
		tok, err := a.CreateUser(email)
		if err != nil {
			return err
		}
		fmt.Printf("user %q created\n\nsend this one-time set-password link (valid 72h):\n  %s\n",
			email, setupURL(cfg, tok))
		return nil

	case "reset-password":
		if len(args) < 2 {
			return errors.New("usage: poligon user reset-password <email>")
		}
		if err := st.ClearPassword(args[1]); err != nil {
			return err
		}
		if err := st.RevokeUserSessions(args[1]); err != nil {
			return err
		}
		tok, err := a.NewSetupToken(args[1])
		if err != nil {
			return err
		}
		fmt.Printf("password reset for %q. New set-password link (valid 72h):\n  %s\n", args[1], setupURL(cfg, tok))
		return nil

	case "disable", "enable":
		if len(args) < 2 {
			return fmt.Errorf("usage: poligon user %s <email>", args[0])
		}
		if err := st.SetUserDisabled(args[1], args[0] == "disable"); err != nil {
			return err
		}
		fmt.Printf("user %q %sd\n", args[1], args[0])
		return nil

	case "list":
		users, err := st.Users()
		if err != nil {
			return err
		}
		for _, u := range users {
			flags := []string{}
			if u.Disabled {
				flags = append(flags, "disabled")
			}
			if !u.PassSet {
				flags = append(flags, "password-pending")
			}
			fmt.Printf("%-32s %s\n", u.Name, strings.Join(flags, ", "))
		}
		return nil

	default:
		return fmt.Errorf("unknown user subcommand %q", args[0])
	}
}

// setupURL builds the set-password link from PublicURL, falling back to a
// localhost URL derived from the listen address.
func setupURL(cfg config.Config, token string) string {
	base := strings.TrimRight(cfg.Auth.PublicURL, "/")
	if base == "" {
		_, port, _ := net.SplitHostPort(cfg.Listen)
		if port == "" {
			port = "8080"
		}
		base = "http://localhost:" + port
	}
	return base + "/auth/setup?t=" + token
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func usage() {
	fmt.Fprint(os.Stderr, `poligon — phone farm control plane

usage:
  poligon serve [--dev]              run the API + dashboard + device poller
  poligon user add <email>           pre-create an account, print a set-password link
  poligon user list                  list accounts and their state
  poligon user disable <email>       block an account and kill its sessions
  poligon user enable <email>        unblock an account
  poligon user reset-password <email> clear the password, issue a new link
  poligon token create <email> [name] mint a personal API token (shown once)
  poligon token list <email>          list a user's API tokens
  poligon token revoke <email> <pfx>  revoke a token by hash prefix

Registration is open: anyone who can reach the dashboard signs up with an
email + password. The CLI is for moderation (disable / reset) from the host.

env:
  POLIGON_CONFIG            config path (default config/devices.yaml)
  POLIGON_DEV_USER          bypass auth as this user; honored only on a loopback
                           listen or with "serve --dev"
  POLIGON_BUNDLETOOL        path to bundletool.jar (aab installs)
  POLIGON_SIGNING_IDENTITY  codesign identity for iOS re-signing
  POLIGON_PROFILE_DIR       farm .mobileprovision dir (default config/profiles)
`)
}
