// Command kanban is the whole application: one binary, one SQLite file.
//
//	kanban serve  [flags]   run the server (default)
//	kanban seed   [flags]   fill an empty database with the demo data
//	kanban reset  [flags]   delete the database and seed it again (server stopped)
//	kanban backup [flags]   copy the database to -to, safe while the server runs
//	kanban export [flags]   print the boards as JSON (to load them into another app)
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"strings"
	"syscall"
	"time"

	"kanban/internal/app"
	"kanban/internal/db"
	"kanban/internal/live"
	"kanban/internal/web"
	webfiles "kanban/web"
)

type config struct {
	addr, dbPath, sync, origins, adminEmail, adminPassword, resetAt, to, static string
	secure, proxied, debug                                                      bool
	throttle                                                                    time.Duration
	lgwin, level, actions, slots                                                int
}

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var c config
	fsf := flag.NewFlagSet(cmd, flag.ExitOnError)
	fsf.StringVar(&c.addr, "addr", env("KANBAN_ADDR", "127.0.0.1:8080"), "listen address")
	fsf.StringVar(&c.dbPath, "db", env("KANBAN_DB", "data/kanban.db"), "SQLite database file")
	fsf.StringVar(&c.sync, "synchronous", env("KANBAN_SYNCHRONOUS", "FULL"), "SQLite synchronous pragma: FULL or NORMAL")
	fsf.StringVar(&c.origins, "origins", env("KANBAN_ORIGINS", ""), "extra allowed Origin values, comma-separated (https://kanban.example.com)")
	fsf.StringVar(&c.adminEmail, "admin-email", env("KANBAN_ADMIN_EMAIL", ""), "seed: an admin account besides the demo users")
	fsf.StringVar(&c.adminPassword, "admin-password", env("KANBAN_ADMIN_PASSWORD", ""), "seed: the admin's password")
	fsf.BoolVar(&c.secure, "secure", env("KANBAN_SECURE", "") == "1", "HTTPS only cookies and HSTS (behind a TLS proxy)")
	fsf.BoolVar(&c.proxied, "proxied", env("KANBAN_PROXIED", "") == "1", "trust X-Forwarded-For from loopback")
	fsf.BoolVar(&c.debug, "debug", env("KANBAN_DEBUG", "") == "1", "debug logging")
	fsf.DurationVar(&c.throttle, "throttle", 50*time.Millisecond, "minimum time between two renders of a board")
	fsf.IntVar(&c.lgwin, "brotli-lgwin", 18, "brotli window of the live streams (log2 bytes, 10 to 24)")
	// A stream's compressor costs memory as long as the stream is open. On
	// the 200-card board with 200 busy streams: quality 3 sends 1.9 kB a
	// frame (0.7 GB RSS), 4 sends 1.0 kB (0.95 GB), 5 sends 0.7 kB (1.2 GB).
	fsf.IntVar(&c.level, "brotli-level", 4, "brotli quality of the live streams (0 to 11)")
	fsf.IntVar(&c.slots, "send-slots", 0, "streams that may send a frame at once (0: half the CPUs)")
	fsf.IntVar(&c.actions, "action-limit", 300, "actions one user may send per 10 s (0: no limit, for load tests)")
	fsf.StringVar(&c.resetAt, "reset-at", env("KANBAN_RESET_AT", ""), "serve: put the demo boards back to the seed every day at HH:MM UTC (a public demo)")
	fsf.StringVar(&c.to, "to", "", "backup: the file to write")
	fsf.StringVar(&c.static, "static", "", "serve: static files from this directory instead of the embedded ones (development)")
	fsf.Parse(args)

	level := slog.LevelInfo
	if c.debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	var err error
	switch cmd {
	case "serve":
		err = serve(c, log)
	case "seed":
		err = seed(c, false)
	case "reset":
		err = seed(c, true)
	case "export":
		err = export(c)
	case "backup":
		if c.to == "" {
			err = errors.New("backup needs -to <file>")
		} else {
			err = db.Backup(context.Background(), c.dbPath, c.to)
		}
	default:
		err = fmt.Errorf("unknown command %q (serve, seed, reset, backup, export)", cmd)
	}
	if err != nil {
		log.Error(cmd, "err", err)
		os.Exit(1)
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func seed(c config, reset bool) error {
	if reset {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Remove(c.dbPath + suffix); err != nil && !errors.Is(err, fs.ErrNotExist) {
				return err
			}
		}
	}
	ctx := context.Background()
	d, err := db.Open(ctx, c.dbPath, db.Options{Synchronous: c.sync})
	if err != nil {
		return err
	}
	defer d.Close()
	if (c.adminEmail == "") != (c.adminPassword == "") {
		return errors.New("give both -admin-email and -admin-password, or neither")
	}
	return (&app.App{DB: d}).Seed(ctx, c.adminEmail, c.adminPassword)
}

func export(c config) error {
	ctx := context.Background()
	d, err := db.Open(ctx, c.dbPath, db.Options{Synchronous: c.sync})
	if err != nil {
		return err
	}
	defer d.Close()
	out, err := (&app.App{DB: d}).ExportAll(ctx)
	if err != nil {
		return err
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}

func buildRef() string {
	if bi, ok := debug.ReadBuildInfo(); ok {
		for _, s := range bi.Settings {
			if s.Key == "vcs.revision" && len(s.Value) >= 7 {
				return s.Value[:7]
			}
		}
	}
	return "dev"
}

func serve(c config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	hub := live.NewHub(nil)
	hub.Throttle = c.throttle
	if c.slots > 0 {
		hub.Slots = make(chan struct{}, c.slots)
	}
	hub.Log = log
	d, err := db.Open(ctx, c.dbPath, db.Options{Synchronous: c.sync, OnCommit: func(boards []int64) { hub.Changed(boards...) }})
	if err != nil {
		return err
	}
	a := &app.App{DB: d}
	r := web.NewRenderer(a)
	hub.Render = r.Frame

	sub, err := fs.Sub(webfiles.FS, "static")
	if err != nil {
		return err
	}
	if c.static != "" {
		sub = os.DirFS(c.static)
	}
	static, err := web.NewStatic(sub)
	if err != nil {
		return err
	}
	var origins []string
	for _, o := range strings.Split(c.origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			origins = append(origins, o)
		}
	}
	s := &web.Server{
		App: a, Hub: hub, Render: r, Static: static, Log: log,
		Secure: c.secure, Origins: origins, Proxied: c.proxied,
		LGWin: c.lgwin, Level: c.level, Actions: c.actions, Started: time.Now(), BuildRef: buildRef(),
	}

	// Streams run until the server stops: their context ends with this one.
	base, cancelStreams := context.WithCancel(context.Background())
	defer cancelStreams()
	srv := &http.Server{
		Addr:              c.addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // the stream lifts its own deadline
		IdleTimeout:       120 * time.Second,
		BaseContext:       func(net.Listener) context.Context { return base },
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}

	// Housekeeping: forget idle tabs, old op outcomes.
	go func() {
		tabs := time.NewTicker(time.Minute)
		ops := time.NewTicker(time.Hour)
		defer tabs.Stop()
		defer ops.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tabs.C:
				hub.Sweep(30 * time.Minute)
			case <-ops.C:
				if err := a.PruneOps(ctx); err != nil {
					log.Warn("prune ops", "err", err)
				}
			}
		}
	}()

	if c.resetAt != "" {
		at, err := time.Parse("15:04", c.resetAt)
		if err != nil {
			return fmt.Errorf("-reset-at %q: want HH:MM", c.resetAt)
		}
		go func() {
			for {
				now := time.Now().UTC()
				next := time.Date(now.Year(), now.Month(), now.Day(), at.Hour(), at.Minute(), 0, 0, time.UTC)
				if !next.After(now) {
					next = next.Add(24 * time.Hour)
				}
				select {
				case <-ctx.Done():
					return
				case <-time.After(time.Until(next)):
				}
				if boards, err := a.Reset(ctx); err != nil {
					log.Error("reset", "err", err)
				} else {
					log.Info("demo reset", "boards", boards)
				}
			}
		}()
	}

	// Compressing the static files at startup leaves tens of MB of garbage.
	debug.FreeOSMemory()

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", c.addr, "db", c.dbPath, "synchronous", c.sync, "build", s.BuildRef)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			d.Close()
			return err
		}
	case <-ctx.Done():
	}

	log.Info("shutting down")
	cancelStreams() // the pages reconnect to the next process and get the whole board
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	return d.Close() // runs what is queued, then closes
}
