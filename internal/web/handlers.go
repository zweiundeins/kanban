package web

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/a-h/templ"

	"kanban/internal/app"
	"kanban/internal/live"
	"kanban/internal/views"
)

// One file per component (Starbase's cmd/dist): one request each, no
// waves of imports.
var boardModules = []string{
	"vendor/starbase/kanban-board.js",
	"vendor/starbase/inline-edit.js",
	"vendor/starbase/modal.js",
	"vendor/starbase/toast.js",
}

func (s *Server) pageOf(r *http.Request, title string) views.Page {
	return views.Page{Title: title, Assets: views.Assets{Prefix: s.Static.Prefix}, User: userOf(r)}
}

func (s *Server) html(w http.ResponseWriter, r *http.Request, status int, c templ.Component) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	if err := c.Render(r.Context(), w); err != nil {
		s.Log.Error("render page", "path", r.URL.Path, "err", err)
	}
}

// page requires a login: without one, the login page.
func (s *Server) page(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if userOf(r) == nil {
			next := "/login"
			if r.URL.Path != "/" {
				next += "?next=" + urlQueryEscape(r.URL.Path)
			}
			http.Redirect(w, r, next, http.StatusSeeOther)
			return
		}
		h(w, r)
	}
}

func urlQueryEscape(s string) string {
	return strings.NewReplacer("%", "%25", "&", "%26", "?", "%3F", "#", "%23", "+", "%2B", " ", "%20").Replace(s)
}

func safeNext(next string) string {
	if !strings.HasPrefix(next, "/") || strings.HasPrefix(next, "//") || strings.HasPrefix(next, "/\\") {
		return "/"
	}
	return next
}

func (s *Server) home(w http.ResponseWriter, r *http.Request) {
	projects, err := s.App.Projects(r.Context(), userOf(r).ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	s.html(w, r, http.StatusOK, views.Home(s.pageOf(r, "Your boards"), projects))
}

func (s *Server) fail(w http.ResponseWriter, r *http.Request, err error) {
	s.Log.Error("request failed", "path", r.URL.Path, "err", err)
	http.Error(w, "Something went wrong. Try again.", http.StatusInternalServerError)
}

func (s *Server) notFound(w http.ResponseWriter, r *http.Request, what string) {
	s.html(w, r, http.StatusNotFound, views.NotFound(s.pageOf(r, "Not found"), what))
}

// boardPage renders the board (and the card in the URL, if any) exactly as
// the stream will, then the stream takes over.
func (s *Server) boardPage(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	board, card := pathID(r, "b"), pathID(r, "c")
	if r.PathValue("c") != "" && card == 0 {
		s.notFound(w, r, "There is no such card.")
		return
	}
	ok, err := s.App.IsMember(r.Context(), board, u.ID)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	if !ok {
		s.notFound(w, r, "There is no such board, or you aren't a member of it.")
		return
	}
	b, err := s.App.LoadBoard(r.Context(), board)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	var viewers []app.User
	if card != 0 {
		viewers = []app.User{*u}
	}
	detail, err := s.Render.Detail(r.Context(), board, card, viewers)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	tab := randomID(16)
	s.Hub.Tab(tab, u.ID, board, card)
	present := s.Hub.Present(board)
	if !slices.Contains(present, u.ID) {
		present = append(present, u.ID)
	}
	p := s.pageOf(r, b.Name)
	p.Modules = boardModules
	p.Class = "board-page"
	s.html(w, r, http.StatusOK, views.BoardPage(views.BoardShell{
		Page: p, Board: b, Tab: tab, Open: card,
		Presence: s.Render.Presence(b, present),
		Lanes:    renderBytes(views.Board(b, s.Render.Lanes(b).Full())),
		Detail:   detail,
	}))
}

// sink sends the hub's patches over a Datastar stream. Its first frame
// also tells the page it is live again.
type sink struct {
	es    *eventStream
	live  bool
	pings int
}

var online = live.Signals([]byte(`{"_offline":false}`))

func (k *sink) Send(e *live.Event) error {
	if err := k.es.write(e.Wire()); err != nil {
		return err
	}
	if !k.live && !e.IsSignals() {
		k.live = true
		return k.es.write(online.Wire())
	}
	return nil
}

// Ping keeps proxies from closing a quiet stream, and the page's watchdog
// from replacing it (the page stamps each ping with its own clock).
func (k *sink) Ping() error {
	k.pings++
	return k.es.write(live.Signals([]byte(`{"_ping":` + strconv.Itoa(k.pings) + `}`)).Wire())
}

// stream is the page's live view: the whole board after every change.
func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	u := userOf(r)
	if u == nil {
		http.Error(w, "Log in again.", http.StatusUnauthorized)
		return
	}
	board := pathID(r, "b")
	tabID := r.URL.Query().Get("tab")
	if !tabRe.MatchString(tabID) {
		http.Error(w, "Bad tab.", http.StatusBadRequest)
		return
	}
	if ok, err := s.App.IsMember(r.Context(), board, u.ID); err != nil || !ok {
		http.Error(w, "Not a member of this board.", http.StatusForbidden)
		return
	}
	t := s.Hub.Tab(tabID, u.ID, board, 0)
	_ = http.NewResponseController(w).SetWriteDeadline(time.Time{})
	es, err := openEventStream(w, r, s.Level, s.LGWin)
	if err != nil {
		return
	}
	defer es.close()
	if err := s.Hub.Serve(r.Context(), t, &sink{es: es}, 20*time.Second); err != nil && r.Context().Err() == nil {
		s.Log.Debug("stream ended", "board", board, "err", err)
	}
}

// flexInt reads a number sent as a number or as a string ("" is 0).
type flexInt int64

func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.Trim(string(b), `"`)
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	n, err := strconv.ParseInt(s, 10, 64)
	*f = flexInt(n)
	return err
}

// payload is every field an action may send.
type payload struct {
	Tab     string  `json:"tab"`
	Op      string  `json:"op"`
	Name    string  `json:"name"`
	Title   string  `json:"title"`
	Text    string  `json:"text"`
	Body    string  `json:"body"`
	Day     string  `json:"day"`
	Color   string  `json:"color"`
	ID      string  `json:"id"`
	List    flexInt `json:"list"`
	Col     flexInt `json:"col"`
	Before  flexInt `json:"before"`
	Seen    flexInt `json:"seen"`
	HasSeen bool    `json:"-"`
	Card    flexInt `json:"card"`
	On      bool    `json:"on"`
	Done    bool    `json:"done"`
}

// actionFunc runs one command; clear names the page's input signals to
// empty once it succeeded.
type actionFunc func(ctx context.Context, r *http.Request, a app.Actor, board int64, p payload) (out app.Outcome, clear map[string]any, err error)

func (s *Server) readPayload(w http.ResponseWriter, r *http.Request) (*app.User, int64, payload, bool) {
	var p payload
	u := userOf(r)
	if u == nil {
		http.Error(w, "Log in again.", http.StatusUnauthorized)
		return nil, 0, p, false
	}
	if r.Header.Get("Datastar-Request") != "true" || !s.sameOrigin(r) {
		http.Error(w, "Cross-site request refused.", http.StatusForbidden)
		return nil, 0, p, false
	}
	board := pathID(r, "b")
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 64<<10))
	var keys map[string]json.RawMessage
	if err == nil && json.Unmarshal(body, &keys) == nil {
		_, p.HasSeen = keys["seen"]
	}
	if err != nil || json.Unmarshal(body, &p) != nil || !tabRe.MatchString(p.Tab) {
		http.Error(w, "Bad request.", http.StatusBadRequest)
		return nil, 0, p, false
	}
	if !s.actionRL.allow("u" + itoa(u.ID)) {
		http.Error(w, "Too many changes at once. Wait a moment.", http.StatusTooManyRequests)
		return nil, 0, p, false
	}
	return u, board, p, true
}

// toast is one message in a tab's sb-toast region; the list is the tab's
// server-owned UI state.
type toast struct {
	ID      string `json:"id"`
	Variant string `json:"variant"`
	Title   string `json:"title,omitempty"`
	Text    string `json:"text"`
}

type tabUI struct {
	toasts []toast
	n      int
}

func toastsOf(t *live.Tab) *tabUI {
	ui, _ := t.State.(*tabUI)
	if ui == nil {
		ui = &tabUI{}
		t.State = ui
	}
	return ui
}

func renderBytes(c templ.Component) []byte {
	var b bytes.Buffer
	_ = c.Render(context.Background(), &b)
	return b.Bytes()
}

func (s *Server) action(do actionFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, board, p, ok := s.readPayload(w, r)
		if !ok {
			return
		}
		out, clear, err := do(r.Context(), r, app.Actor{User: u.ID, Op: p.Op}, board, p)
		if errors.Is(err, errBadRequest) {
			http.Error(w, "Bad request.", http.StatusBadRequest)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		t := s.Hub.Tab(p.Tab, u.ID, board, 0)
		acks := map[string]any{}
		if p.Op != "" {
			result := "ok"
			if !out.OK {
				result = out.Msg
			}
			acks["_acks"] = map[string]string{p.Op: result}
		}
		html := renderBytes(views.Status(out.Notice))
		if out.OK {
			for k, v := range clear {
				acks[k] = v
			}
		} else {
			html = renderBytes(views.Status(out.Msg))
			t.Lock()
			ui := toastsOf(t)
			ui.n++
			ui.toasts = append(ui.toasts, toast{ID: "t" + strconv.Itoa(ui.n), Variant: "warn", Title: "Not saved", Text: out.Msg})
			if len(ui.toasts) > 6 {
				ui.toasts = ui.toasts[len(ui.toasts)-6:]
			}
			js, _ := json.Marshal(ui.toasts)
			t.Unlock()
			html = append(html, renderBytes(views.Toasts(board, string(js)))...)
		}
		sig, _ := json.Marshal(acks)
		s.Hub.Send(t, live.Msg{After: out.Version, Signals: sig, HTML: html})
		w.WriteHeader(http.StatusNoContent)
	}
}

// ui handles a command about the tab's own view.
func (s *Server) ui(do func(t *live.Tab, board int64, p payload) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, board, p, ok := s.readPayload(w, r)
		if !ok {
			return
		}
		if member, err := s.App.IsMember(r.Context(), board, u.ID); err != nil || !member {
			http.Error(w, "Not a member of this board.", http.StatusForbidden)
			return
		}
		if err := do(s.Hub.Tab(p.Tab, u.ID, board, 0), board, p); err != nil {
			http.Error(w, "Bad request.", http.StatusBadRequest)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

func (s *Server) openCard(t *live.Tab, board int64, p payload) error {
	if p.Card < 0 {
		return errBadRequest
	}
	s.Hub.SetOpen(t, int64(p.Card))
	return nil
}

func (s *Server) dismissToast(t *live.Tab, board int64, p payload) error {
	t.Lock()
	ui := toastsOf(t)
	ui.toasts = slices.DeleteFunc(ui.toasts, func(x toast) bool { return x.ID == p.ID })
	js, _ := json.Marshal(ui.toasts)
	t.Unlock()
	s.Hub.Send(t, live.Msg{HTML: renderBytes(views.Toasts(board, string(js)))})
	return nil
}

// The actions.

func (s *Server) createList(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.CreateList(ctx, a, b, p.Name)
	return out, map[string]any{"_in": map[string]any{"list": ""}}, err
}

func (s *Server) renameList(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.RenameList(ctx, a, b, pathID(r, "l"), p.Name)
	return out, nil, err
}

func (s *Server) moveList(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	seen := int64(-1)
	if p.HasSeen {
		seen = int64(p.Seen)
	}
	out, err := s.App.MoveList(ctx, a, b, pathID(r, "l"), int64(p.Before), seen)
	return out, nil, err
}

func (s *Server) archiveList(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.ArchiveList(ctx, a, b, pathID(r, "l"))
	return out, nil, err
}

func (s *Server) createCard(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.CreateCard(ctx, a, b, int64(p.List), p.Title)
	return out, map[string]any{"_in": map[string]any{"l" + itoa(int64(p.List)): ""}}, err
}

func (s *Server) moveCard(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.MoveCard(ctx, a, b, app.Move{Card: pathID(r, "c"), List: int64(p.Col), Before: int64(p.Before), Seen: int64(p.Seen)})
	return out, nil, err
}

func (s *Server) renameCard(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.RenameCard(ctx, a, b, pathID(r, "c"), p.Title)
	return out, nil, err
}

func (s *Server) archiveCard(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.ArchiveCard(ctx, a, b, pathID(r, "c"))
	return out, nil, err
}

func (s *Server) setDescription(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.SetDescription(ctx, a, b, pathID(r, "c"), p.Text)
	return out, nil, err
}

func (s *Server) setDue(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.SetDue(ctx, a, b, pathID(r, "c"), p.Day)
	return out, nil, err
}

func (s *Server) setDueDone(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.SetDueDone(ctx, a, b, pathID(r, "c"), p.Done)
	return out, nil, err
}

func (s *Server) setLabel(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.SetLabel(ctx, a, b, pathID(r, "c"), pathID(r, "label"), p.On)
	return out, nil, err
}

func (s *Server) createLabel(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.CreateLabel(ctx, a, b, pathID(r, "c"), p.Name, p.Color)
	return out, map[string]any{"_in": map[string]any{"ln": ""}}, err
}

func (s *Server) addComment(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	c := pathID(r, "c")
	out, err := s.App.AddComment(ctx, a, b, c, p.Body)
	return out, map[string]any{"_in": map[string]any{"c" + itoa(c): ""}}, err
}

func (s *Server) deleteComment(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, map[string]any, error) {
	out, err := s.App.DeleteComment(ctx, a, b, pathID(r, "id"))
	return out, nil, err
}

// Login and logout are plain forms: they work before there is a stream.

func (s *Server) csrfToken(w http.ResponseWriter, r *http.Request) string {
	if c, err := r.Cookie(csrfCookie); err == nil && len(c.Value) >= 20 {
		return c.Value
	}
	tok := randomID(24)
	http.SetCookie(w, &http.Cookie{Name: csrfCookie, Value: tok, Path: "/", HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode})
	return tok
}

func (s *Server) loginPage(w http.ResponseWriter, r *http.Request) {
	if userOf(r) != nil {
		http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
		return
	}
	s.html(w, r, http.StatusOK, views.Login(s.pageOf(r, "Log in"), "", "", s.csrfToken(w, r)))
}

func (s *Server) login(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, 16<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "Bad request.", http.StatusBadRequest)
		return
	}
	c, err := r.Cookie(csrfCookie)
	if !s.sameOrigin(r) || err != nil || c.Value == "" || r.PostFormValue("csrf") != c.Value {
		http.Error(w, "The form expired. Reload the page and try again.", http.StatusForbidden)
		return
	}
	email := r.PostFormValue("email")
	page := s.pageOf(r, "Log in")
	if !s.loginRL.allow("ip" + s.clientIP(r)) {
		s.html(w, r, http.StatusTooManyRequests, views.Login(page, email, "Too many attempts. Wait a minute and try again.", c.Value))
		return
	}
	token, _, err := s.App.Login(r.Context(), email, r.PostFormValue("password"))
	if errors.Is(err, app.ErrBadLogin) {
		s.html(w, r, http.StatusUnauthorized, views.Login(page, email, "Wrong email or password.", c.Value))
		return
	}
	if err != nil {
		s.fail(w, r, err)
		return
	}
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", HttpOnly: true, Secure: s.Secure,
		SameSite: http.SameSiteLaxMode, MaxAge: int(app.SessionTTL.Seconds()),
	})
	http.Redirect(w, r, safeNext(r.URL.Query().Get("next")), http.StatusSeeOther)
}

func (s *Server) logout(w http.ResponseWriter, r *http.Request) {
	if !s.sameOrigin(r) {
		http.Error(w, "Cross-site request refused.", http.StatusForbidden)
		return
	}
	if c, err := r.Cookie(sessionCookie); err == nil {
		if err := s.App.Logout(r.Context(), c.Value); err != nil {
			s.fail(w, r, err)
			return
		}
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true, Secure: s.Secure, SameSite: http.SameSiteLaxMode})
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
