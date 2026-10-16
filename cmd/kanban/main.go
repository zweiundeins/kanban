// Command kanban is the whole application: one binary, one SQLite file.
//
//	kanban serve  [flags]   run the server (default)
//	kanban seed   [flags]   fill an empty database with the demo data
//	kanban reset  [flags]   delete the database and seed it again (server stopped)
//	kanban backup [flags]   copy the database to -to, safe while the server runs
//	kanban export [flags]   print the boards as JSON (to load them into another app)
//
// serve's metrics for Prometheus go out on a loopback listener of their own,
// -metrics (metrics.go).
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

	"github.com/klauspost/compress/zstd"
	"github.com/zweiundeins/lith/edge"
	lithlive "github.com/zweiundeins/lith/live"
	"github.com/zweiundeins/lith/sse"

	"kanban/internal/app"
	"kanban/internal/db"
	"kanban/internal/live"
	"kanban/internal/views"
	"kanban/internal/web"
	webfiles "kanban/web"
)

type config struct {
	addr, metrics, dbPath, sync, origins, adminEmail, adminPassword, resetAt, to, static, zstdLevel string
	secure, debug                                                                                   bool
	throttle                                                                                        time.Duration
	lgwin, level, actions                                                                           int
}

func main() {
	cmd := "serve"
	args := os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	var c config
	flags(cmd, &c).Parse(args)

	level := slog.LevelInfo
	if c.debug {
		level = slog.LevelDebug
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level}))
	slog.SetDefault(log)

	var err error
	switch cmd {
	case "serve":
		err = serve(context.Background(), c, log)
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

// flags defines cmd's flags into c.
func flags(cmd string, c *config) *flag.FlagSet {
	fsf := flag.NewFlagSet(cmd, flag.ExitOnError)
	fsf.StringVar(&c.addr, "addr", env("KANBAN_ADDR", "127.0.0.1:8080"), "listen address")
	fsf.StringVar(&c.metrics, "metrics", env("KANBAN_METRICS", "auto"), "serve: the metrics for Prometheus on this loopback host:port; auto: 127.0.0.1 at -addr's port + 100; off: none")
	fsf.StringVar(&c.dbPath, "db", env("KANBAN_DB", "data/kanban.db"), "SQLite database file")
	fsf.StringVar(&c.sync, "synchronous", env("KANBAN_SYNCHRONOUS", "FULL"), "SQLite synchronous pragma: FULL or NORMAL")
	fsf.StringVar(&c.origins, "origins", env("KANBAN_ORIGINS", ""), "extra allowed Origin values, comma-separated (https://kanban.example.com)")
	fsf.StringVar(&c.adminEmail, "admin-email", env("KANBAN_ADMIN_EMAIL", ""), "seed: an admin account besides the demo users")
	fsf.StringVar(&c.adminPassword, "admin-password", env("KANBAN_ADMIN_PASSWORD", ""), "seed: the admin's password")
	fsf.BoolVar(&c.secure, "secure", env("KANBAN_SECURE", "") == "1", "HTTPS only cookies and HSTS (behind a TLS proxy)")
	fsf.BoolVar(&c.debug, "debug", env("KANBAN_DEBUG", "") == "1", "debug logging")
	fsf.DurationVar(&c.throttle, "throttle", 50*time.Millisecond, "minimum time between two renders of a board")
	// Browsers that accept zstd get it, the others brotli. On the 200-card
	// board with 200 busy streams, brotli quality 3 sends 1.9 kB a frame
	// (0.7 GB RSS), 4 sends 1.0 kB (0.95 GB), 5 sends 0.7 kB (1.2 GB).
	fsf.IntVar(&c.lgwin, "brotli-lgwin", 18, "the largest brotli window a live stream may take (log2 bytes, 10 to 24)")
	fsf.IntVar(&c.level, "brotli-level", 4, "brotli quality of the live streams (1 to 11)")
	fsf.StringVar(&c.zstdLevel, "zstd-level", "fastest", "zstd level of the live streams: fastest, default, better or best")
	fsf.IntVar(&c.actions, "action-limit", 300, "actions one user may send per 10 s (0: no limit, for load tests)")
	fsf.StringVar(&c.resetAt, "reset-at", env("KANBAN_RESET_AT", ""), "serve: put the demo boards back to the seed every day at HH:MM UTC (a public demo)")
	fsf.StringVar(&c.to, "to", "", "backup: the file to write")
	fsf.StringVar(&c.static, "static", "", "serve: static files from this directory instead of the embedded ones (development)")
	return fsf
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

// serve runs the server until ctx ends or the process gets SIGINT or SIGTERM.
func serve(ctx context.Context, c config, log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	ok, zstdLevel := zstd.EncoderLevelFromString(c.zstdLevel)
	if !ok {
		return fmt.Errorf("-zstd-level %q: want fastest, default, better or best", c.zstdLevel)
	}
	maddr, err := metricsListen(c.metrics, c.addr)
	if err != nil {
		return err
	}
	var mln net.Listener
	if maddr != "" {
		if mln, err = net.Listen("tcp", maddr); err != nil {
			return fmt.Errorf("metrics: %w", err)
		}
		defer mln.Close()
	}
	if err := registerDriver(log); err != nil {
		return err
	}

	// The writer tells the hub what each commit changed; the hub reads
	// from the database's pool. No command is sent before hub is set.
	var hub *lithlive.Hub
	d, err := db.Open(ctx, c.dbPath, db.Options{Synchronous: c.sync, Log: log, OnCommit: func(topics []string) { hub.Changed(topics) }, Driver: sqliteDriver})
	if err != nil {
		return err
	}
	hub = lithlive.New(lithlive.Options{
		Read:            d.R,
		Tick:            c.throttle,
		HeartbeatSignal: views.PingSignal,
		Log:             log,
		// The board's frames are about 140 KB, twenty a second: a stream for
		// which the budget has no window that holds a frame is refused with
		// 503, and the page's retry 'error' brings it back once there is room.
		SSE: sse.Options{BrotliQuality: c.level, BrotliWindow: c.lgwin, ZstdLevel: zstdLevel,
			Budget: sse.NewBudget(sse.BudgetOptions{Refuse: true})},
	})
	hubCtx, stopStreams := context.WithCancel(context.Background())
	defer stopStreams()
	hubDone := make(chan error, 1)
	go func() { hubDone <- hub.Run(hubCtx) }()
	a := &app.App{DB: d}

	sub, err := fs.Sub(webfiles.FS, "static")
	if err != nil {
		return err
	}
	if c.static != "" {
		sub = os.DirFS(c.static)
	}
	static, err := edge.NewStatic(sub, "/static/")
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
		App: a, Hub: hub, Seats: live.NewSeats(hub.Changed, 3*time.Second), Render: web.NewRenderer(), Static: static, Log: log,
		Secure: c.secure, Origins: origins, Actions: c.actions, Started: time.Now(), BuildRef: buildRef(),
	}
	srv := &http.Server{
		Addr:              c.addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second, // a stream sets a deadline for each of its writes
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	// The metrics never go on the public listener.
	msrv := &http.Server{Handler: metrics(d.W, hub, log), ReadHeaderTimeout: 5 * time.Second}
	if mln != nil {
		go func() {
			if err := msrv.Serve(mln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("metrics", "err", err)
			}
		}()
	}

	// Housekeeping: old messages for tabs, old op outcomes.
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
				if err := a.Sweep(ctx); err != nil {
					log.Warn("sweep tabs", "err", err)
				}
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

	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", c.addr, "metrics", maddr, "db", c.dbPath, "synchronous", c.sync, "build", s.BuildRef)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		if !errors.Is(err, http.ErrServerClosed) {
			msrv.Close()
			stopStreams()
			<-hubDone
			d.Close()
			return err
		}
	case <-ctx.Done():
	}

	log.Info("shutting down")
	stopStreams() // the pages reconnect to the next process and get the whole board
	<-hubDone
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(sctx); err != nil {
		log.Warn("shutdown", "err", err)
	}
	msrv.Close()
	return d.Close() // runs what is queued, then closes
}
