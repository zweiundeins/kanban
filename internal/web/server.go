// Package web is the HTTP side: pages, the live stream, and the actions,
// which only ever answer 204 (what they did reaches the page through its
// stream).
package web

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"time"

	"github.com/zweiundeins/lith/edge"
	lithlive "github.com/zweiundeins/lith/live"

	"kanban/internal/app"
	"kanban/internal/live"
	"kanban/internal/views"
)

// Server holds everything the handlers need.
type Server struct {
	App      *app.App
	Hub      *lithlive.Hub
	Seats    *live.Seats
	Render   *Renderer
	Static   *edge.Static
	Log      *slog.Logger
	Secure   bool     // cookies only over HTTPS, HSTS
	Origins  []string // allowed Origin values besides the request's own host
	Actions  int      // actions a user may send per 10 s (0: no limit)
	Started  time.Time
	BuildRef string

	loginRL  *edge.Limiter
	actionRL *edge.Limiter
}

const (
	sessionCookie = "kanban_session"
	csrfCookie    = "kanban_csrf"
)

type ctxKey int

const userKey ctxKey = 1

func userOf(r *http.Request) *app.User {
	u, _ := r.Context().Value(userKey).(*app.User)
	return u
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func randomID(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

var tabRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,40}$`)

const staticPrefix = "/static/"

// Handler returns the routes with their middleware.
func (s *Server) Handler() http.Handler {
	s.loginRL = edge.NewLimiter(10.0/60, 10)
	if s.Actions > 0 {
		s.actionRL = edge.NewLimiter(float64(s.Actions)/10, s.Actions)
	}

	mux := http.NewServeMux()
	mux.Handle("GET "+staticPrefix, s.Static)
	mux.HandleFunc("GET /_health", s.health)
	mux.HandleFunc("GET /login", s.loginPage)
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /{$}", s.page(s.home))
	mux.HandleFunc("GET /boards/{b}", s.page(s.boardPage))
	mux.HandleFunc("GET /boards/{b}/cards/{c}", s.page(s.boardPage))
	mux.HandleFunc("GET /boards/{b}/live", s.stream)

	// Commands about the board: written by the single writer, 204.
	act := func(pattern string, do actionFunc) { mux.HandleFunc("POST "+pattern, s.action(do)) }
	act("/boards/{b}/lists", s.createList)
	act("/boards/{b}/lists/{l}/rename", s.renameList)
	act("/boards/{b}/lists/{l}/move", s.moveList)
	act("/boards/{b}/lists/{l}/archive", s.archiveList)
	act("/boards/{b}/cards", s.createCard)
	act("/boards/{b}/cards/{c}/move", s.moveCard)
	act("/boards/{b}/cards/{c}/rename", s.renameCard)
	act("/boards/{b}/cards/{c}/archive", s.archiveCard)
	act("/boards/{b}/cards/{c}/description", s.setDescription)
	act("/boards/{b}/cards/{c}/due", s.setDue)
	act("/boards/{b}/cards/{c}/due-done", s.setDueDone)
	act("/boards/{b}/cards/{c}/labels/new", s.createLabel)
	act("/boards/{b}/cards/{c}/labels/{label}", s.setLabel)
	act("/boards/{b}/cards/{c}/comments", s.addComment)
	act("/boards/{b}/comments/{id}/delete", s.deleteComment)
	// Commands about this tab's view: 204 as well.
	mux.HandleFunc("POST /boards/{b}/open", s.ui(s.openCard))
	mux.HandleFunc("POST /boards/{b}/toasts/dismiss", s.ui(s.dismissToast))

	h := s.session(mux)
	h = edge.CSRF{Origins: s.Origins, Datastar: []string{"POST /boards/"}}.Handler(h)
	h = edge.SecurityHeaders{
		CSP:      s.policy(),
		HSTS:     s.Secure,
		Referrer: "same-origin",
		Extra:    map[string]string{"Permissions-Policy": "camera=(), microphone=(), geolocation=()"},
	}.Handler(h)
	h = edge.Compress(h)
	h = edge.Recover(s.Log, h)
	return s.logged(h)
}

// policy is the content security policy: Datastar evaluates expressions
// (unsafe-eval); the two inline scripts are allowed by their hashes.
func (s *Server) policy() edge.CSP {
	hash := func(body string) string {
		h := sha256.Sum256([]byte(body))
		return "'sha256-" + base64.StdEncoding.EncodeToString(h[:]) + "'"
	}
	return edge.CSP{
		"default-src":     {"'self'"},
		"script-src":      {"'self'", "'unsafe-eval'", hash(views.ImportMap(s.Static.URL(views.DatastarJS))), hash(views.SpeculationRules)},
		"style-src":       {"'self'", "'unsafe-inline'"},
		"img-src":         {"'self'", "data:"},
		"connect-src":     {"'self'"},
		"form-action":     {"'self'"},
		"frame-ancestors": {"'none'"},
		"base-uri":        {"'none'"},
		"object-src":      {"'none'"},
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// logged logs every request; static files and the health check only when
// debugging.
func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		level := slog.LevelInfo
		if strings.HasPrefix(r.URL.Path, staticPrefix) || r.URL.Path == "/_health" {
			level = slog.LevelDebug
		}
		if sw.status >= 500 {
			level = slog.LevelError
		}
		s.Log.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

// session puts the logged-in user (if any) into the request's context.
func (s *Server) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, staticPrefix) {
			next.ServeHTTP(w, r)
			return
		}
		if c, err := r.Cookie(sessionCookie); err == nil {
			if u, err := s.App.Session(r.Context(), c.Value); err == nil {
				r = r.WithContext(context.WithValue(r.Context(), userKey, &u))
			}
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	var one int
	err := s.App.DB.R.QueryRowContext(r.Context(), "SELECT 1").Scan(&one)
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	ws, ls, cards, rs := s.App.DB.W.Stats(), s.Hub.Stats(), s.Render.cards.Stats(), s.App.DB.R.Stats()
	body := map[string]any{
		"ok":         err == nil,
		"memory":     map[string]uint64{"heap_alloc": ms.HeapAlloc, "heap_sys": ms.HeapSys, "sys": ms.Sys, "gc_cycles": uint64(ms.NumGC), "total_alloc": ms.TotalAlloc},
		"process":    process(),
		"goroutines": runtime.NumGoroutine(),
		"build":      s.BuildRef,
		"uptime_s":   int(time.Since(s.Started).Seconds()),
		"writer": map[string]any{
			"commands": ws.Commands, "batches": ws.Batches, "failed": ws.Failures, "max_batch": ws.MaxBatch,
			"queue_depth": ws.Queue, "commit_nanos": ws.Busy.Nanoseconds(), "p99_commit_ms": float64(ws.P99Commit.Microseconds()) / 1000,
		},
		"live": map[string]any{
			"streams": ls.Streams, "views": ls.Views, "ticks": ls.Ticks, "view_renders": ls.Renders, "overruns": ls.Overruns,
			"renders": s.Render.renders.Load(), "render_nanos": s.Render.renderNanos.Load(),
			"frames_sent": ls.FramesSent, "frames_skipped": ls.FramesDropped,
		},
		"card_cache": map[string]any{"hits": cards.Hits, "misses": cards.Misses, "entries": cards.Entries, "bytes": cards.Bytes},
		"read_pool":  map[string]any{"size": rs.MaxOpenConnections, "in_use": rs.InUse, "waits": rs.WaitCount, "wait_nanos": rs.WaitDuration.Nanoseconds()},
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
	}
	json.NewEncoder(w).Encode(body)
}

func pathID(r *http.Request, name string) int64 {
	n, err := strconv.ParseInt(r.PathValue(name), 10, 64)
	if err != nil || n <= 0 {
		return 0
	}
	return n
}

var errBadRequest = errors.New("bad request")
