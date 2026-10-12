package web

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"slices"
	"strconv"
	"strings"

	"github.com/a-h/templ"

	"kanban/internal/app"
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
	var b *app.Board
	var detail []byte
	err = s.App.DB.View(r.Context(), func(tx *sql.Tx) error {
		var err error
		if b, err = app.ReadBoard(r.Context(), tx, board); err != nil {
			return err
		}
		var viewers []app.User
		if card != 0 {
			viewers = []app.User{*u}
		}
		detail, err = s.Render.Detail(r.Context(), tx, board, card, viewers)
		return err
	})
	if err != nil {
		s.fail(w, r, err)
		return
	}
	present := s.Seats.Present(board)
	if !slices.Contains(present, u.ID) {
		present = append(present, u.ID)
	}
	p := s.pageOf(r, b.Name)
	p.Modules = boardModules
	p.Class = "board-page"
	s.html(w, r, http.StatusOK, views.BoardPage(views.BoardShell{
		Page: p, Board: b, Tab: randomID(16), Open: card,
		Presence: s.Render.Presence(b.Members, present),
		Lanes:    renderBytes(views.Board(b, s.Render.Lanes(b).Full())),
		Detail:   detail,
	}))
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

// actionFunc runs one command. It sets a.Clear to the page's input signals
// to empty once the command succeeded.
type actionFunc func(ctx context.Context, r *http.Request, a app.Actor, board int64, p payload) (app.Outcome, error)

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

func renderBytes(c templ.Component) []byte {
	var b bytes.Buffer
	_ = c.Render(context.Background(), &b)
	return b.Bytes()
}

// action answers 204 once the command committed: its outcome reaches the
// page on its stream, written by the command into the tab's outbox.
func (s *Server) action(do actionFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, board, p, ok := s.readPayload(w, r)
		if !ok {
			return
		}
		_, err := do(r.Context(), r, app.Actor{User: u.ID, Op: p.Op, Tab: p.Tab}, board, p)
		if errors.Is(err, errBadRequest) {
			http.Error(w, "Bad request.", http.StatusBadRequest)
			return
		}
		if err != nil {
			s.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// ui handles a command about the tab's own view.
func (s *Server) ui(do func(ctx context.Context, u *app.User, board int64, p payload) error) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		u, board, p, ok := s.readPayload(w, r)
		if !ok {
			return
		}
		if member, err := s.App.IsMember(r.Context(), board, u.ID); err != nil || !member {
			http.Error(w, "Not a member of this board.", http.StatusForbidden)
			return
		}
		if err := do(r.Context(), u, board, p); errors.Is(err, errBadRequest) {
			http.Error(w, "Bad request.", http.StatusBadRequest)
			return
		} else if err != nil {
			s.fail(w, r, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}
}

// openCard shows a card in the tab's dialog. Which card a tab shows lives
// with its stream: without one, the page sends it when it connects.
func (s *Server) openCard(_ context.Context, u *app.User, board int64, p payload) error {
	if p.Card < 0 {
		return errBadRequest
	}
	s.Seats.Open(u.ID, p.Tab, int64(p.Card))
	return nil
}

func (s *Server) dismissToast(ctx context.Context, u *app.User, board int64, p payload) error {
	return s.App.DismissToast(ctx, u.ID, board, p.Tab, p.ID)
}

// The actions.

func (s *Server) createList(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	a.Clear = map[string]any{"_in": map[string]any{"list": ""}}
	return s.App.CreateList(ctx, a, b, p.Name)
}

func (s *Server) renameList(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	return s.App.RenameList(ctx, a, b, pathID(r, "l"), p.Name)
}

func (s *Server) moveList(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	seen := int64(-1)
	if p.HasSeen {
		seen = int64(p.Seen)
	}
	return s.App.MoveList(ctx, a, b, pathID(r, "l"), int64(p.Before), seen)
}

func (s *Server) archiveList(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	return s.App.ArchiveList(ctx, a, b, pathID(r, "l"))
}

func (s *Server) createCard(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	a.Clear = map[string]any{"_in": map[string]any{"l" + itoa(int64(p.List)): ""}}
	return s.App.CreateCard(ctx, a, b, int64(p.List), p.Title)
}

func (s *Server) moveCard(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	return s.App.MoveCard(ctx, a, b, app.Move{Card: pathID(r, "c"), List: int64(p.Col), Before: int64(p.Before), Seen: int64(p.Seen)})
}

func (s *Server) renameCard(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	return s.App.RenameCard(ctx, a, b, pathID(r, "c"), p.Title)
}

func (s *Server) archiveCard(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	return s.App.ArchiveCard(ctx, a, b, pathID(r, "c"))
}

func (s *Server) setDescription(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	return s.App.SetDescription(ctx, a, b, pathID(r, "c"), p.Text)
}

func (s *Server) setDue(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	return s.App.SetDue(ctx, a, b, pathID(r, "c"), p.Day)
}

func (s *Server) setDueDone(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	return s.App.SetDueDone(ctx, a, b, pathID(r, "c"), p.Done)
}

func (s *Server) setLabel(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	return s.App.SetLabel(ctx, a, b, pathID(r, "c"), pathID(r, "label"), p.On)
}

func (s *Server) createLabel(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	a.Clear = map[string]any{"_in": map[string]any{"ln": ""}}
	return s.App.CreateLabel(ctx, a, b, pathID(r, "c"), p.Name, p.Color)
}

func (s *Server) addComment(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	c := pathID(r, "c")
	a.Clear = map[string]any{"_in": map[string]any{"c" + itoa(c): ""}}
	return s.App.AddComment(ctx, a, b, c, p.Body)
}

func (s *Server) deleteComment(ctx context.Context, r *http.Request, a app.Actor, b int64, p payload) (app.Outcome, error) {
	return s.App.DeleteComment(ctx, a, b, pathID(r, "id"))
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
