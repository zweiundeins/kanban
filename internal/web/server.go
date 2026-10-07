// Package web is the HTTP side: pages, the live stream, and the actions,
// which only ever answer 204 (what they did reaches the page through its
// stream).
package web

import (
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/andybalholm/brotli"

	"kanban/internal/app"
	"kanban/internal/live"
	"kanban/internal/views"
)

// Server holds everything the handlers need.
type Server struct {
	App      *app.App
	Hub      *live.Hub
	Render   *Renderer
	Static   *Static
	Log      *slog.Logger
	Secure   bool     // cookies only over HTTPS, HSTS
	Origins  []string // allowed Origin values besides the request's own host
	Proxied  bool     // trust X-Forwarded-For from loopback
	LGWin    int      // brotli window of the live streams
	Actions  int      // actions a user may send per 10 s (0: no limit)
	Level    int      // brotli quality of the live streams
	Started  time.Time
	BuildRef string

	csp      string
	loginRL  *limiter
	actionRL *limiter
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

// Handler returns the routes with their middleware.
func (s *Server) Handler() http.Handler {
	s.loginRL = newLimiter(10, time.Minute)
	s.actionRL = newLimiter(s.Actions, 10*time.Second)
	s.csp = s.policy()

	mux := http.NewServeMux()
	mux.Handle("GET /s/{hash}/{path...}", s.Static)
	mux.HandleFunc("GET /_health", s.health)
	mux.HandleFunc("GET /login", s.compress(s.loginPage))
	mux.HandleFunc("POST /login", s.login)
	mux.HandleFunc("POST /logout", s.logout)
	mux.HandleFunc("GET /{$}", s.compress(s.page(s.home)))
	mux.HandleFunc("GET /boards/{b}", s.compress(s.page(s.boardPage)))
	mux.HandleFunc("GET /boards/{b}/cards/{c}", s.compress(s.page(s.boardPage)))
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
	// Commands about this tab's view: no database, 204 as well.
	mux.HandleFunc("POST /boards/{b}/open", s.ui(s.openCard))
	mux.HandleFunc("POST /boards/{b}/toasts/dismiss", s.ui(s.dismissToast))

	return s.recover(s.logged(s.headers(s.session(mux))))
}

// policy is the content security policy: Datastar evaluates expressions
// (unsafe-eval); the two inline scripts are allowed by their hashes.
func (s *Server) policy() string {
	hash := func(body string) string {
		h := sha256.Sum256([]byte(body))
		return "'sha256-" + base64.StdEncoding.EncodeToString(h[:]) + "'"
	}
	return strings.Join([]string{
		"default-src 'self'",
		"script-src 'self' 'unsafe-eval' " + hash(views.ImportMap(s.Static.Prefix)) + " " + hash(views.SpeculationRules),
		"style-src 'self' 'unsafe-inline'",
		"img-src 'self' data:",
		"connect-src 'self'",
		"form-action 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"object-src 'none'",
	}, "; ")
}

func (s *Server) headers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("Content-Security-Policy", s.csp)
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("Referrer-Policy", "same-origin")
		h.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=()")
		if s.Secure {
			h.Set("Strict-Transport-Security", "max-age=63072000; includeSubDomains")
		}
		next.ServeHTTP(w, r)
	})
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

func (s *Server) logged(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, r)
		level := slog.LevelInfo
		if strings.HasPrefix(r.URL.Path, "/s/") || r.URL.Path == "/_health" {
			level = slog.LevelDebug
		}
		if sw.status >= 500 {
			level = slog.LevelError
		}
		s.Log.Log(r.Context(), level, "http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "ms", time.Since(start).Milliseconds())
	})
}

func (s *Server) recover(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler {
					panic(p)
				}
				s.Log.Error("panic", "path", r.URL.Path, "panic", fmt.Sprint(p))
				http.Error(w, "Internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// session puts the logged-in user (if any) into the request's context.
func (s *Server) session(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/s/") {
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

// sameOrigin rejects cross-site writes: the Origin header (or Fetch
// Metadata) must name this site.
func (s *Server) sameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		return r.Header.Get("Sec-Fetch-Site") == "same-origin"
	}
	for _, o := range s.Origins {
		if origin == o {
			return true
		}
	}
	u, err := url.Parse(origin)
	return err == nil && u.Host == r.Host
}

func (s *Server) clientIP(r *http.Request) string {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if s.Proxied && (host == "127.0.0.1" || host == "::1") {
		if f := r.Header.Get("X-Forwarded-For"); f != "" {
			parts := strings.Split(f, ",")
			return strings.TrimSpace(parts[len(parts)-1])
		}
	}
	return host
}

// compress compresses a page (brotli or gzip), unlike the live stream,
// which compresses itself.
func (s *Server) compress(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Add("Vary", "Accept-Encoding")
		switch {
		case accepts(r, "br"):
			w.Header().Set("Content-Encoding", "br")
			bw := brotli.NewWriterLevel(w, 5)
			defer bw.Close()
			h(compressed{w, bw}, r)
		case accepts(r, "gzip"):
			w.Header().Set("Content-Encoding", "gzip")
			gw, _ := gzip.NewWriterLevel(w, 6)
			defer gw.Close()
			h(compressed{w, gw}, r)
		default:
			h(w, r)
		}
	}
}

type compressed struct {
	http.ResponseWriter
	w interface{ Write([]byte) (int, error) }
}

func (c compressed) Write(b []byte) (int, error) { return c.w.Write(b) }

// limiter allows n events per window per key.
type limiter struct {
	mu     sync.Mutex
	n      int
	window time.Duration
	seen   map[string][]time.Time
}

func newLimiter(n int, window time.Duration) *limiter {
	return &limiter{n: n, window: window, seen: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	if l.n <= 0 {
		return true
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	ts := l.seen[key]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > l.window {
		i++
	}
	ts = ts[i:]
	if len(ts) >= l.n {
		l.seen[key] = ts
		return false
	}
	l.seen[key] = append(ts, now)
	if len(l.seen) > 10000 {
		for k, v := range l.seen {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > l.window {
				delete(l.seen, k)
			}
		}
	}
	return true
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	var one int
	err := s.App.DB.Read.QueryRowContext(r.Context(), "SELECT 1").Scan(&one)
	s.Render.cards.mu.Lock()
	cards := map[string]int64{"hits": s.Render.cards.hits, "misses": s.Render.cards.miss, "size": int64(len(s.Render.cards.items))}
	s.Render.cards.mu.Unlock()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	body := map[string]any{
		"ok":         err == nil,
		"memory":     map[string]uint64{"heap_alloc": ms.HeapAlloc, "heap_sys": ms.HeapSys, "sys": ms.Sys, "gc_cycles": uint64(ms.NumGC), "total_alloc": ms.TotalAlloc},
		"process":    process(),
		"goroutines": runtime.NumGoroutine(),
		"build":      s.BuildRef,
		"uptime_s":   int(time.Since(s.Started).Seconds()),
		"writer":     s.App.DB.W.Stats(),
		"live":       s.Hub.Stats(),
		"card_cache": cards,
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
