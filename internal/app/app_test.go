package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/zweiundeins/lith/writer"

	"kanban/internal/db"
)

func newApp(t *testing.T) *App { return newAppWith(t, nil) }

func newAppWith(t *testing.T, onCommit func([]string)) *App {
	t.Helper()
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "app.db"), db.Options{Synchronous: "NORMAL", OnCommit: onCommit})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { d.Close() })
	a := &App{DB: d}
	if err := a.Seed(context.Background(), "", ""); err != nil {
		t.Fatal(err)
	}
	return a
}

// The small board: "Website", lists To do / Doing / Done with 3 cards each.
func small(t *testing.T, a *App) *Board {
	t.Helper()
	b, err := a.LoadBoard(context.Background(), 1)
	if err != nil {
		t.Fatal(err)
	}
	if b.Name != "Website" || len(b.Lists) != 3 {
		t.Fatalf("seed: %s with %d lists", b.Name, len(b.Lists))
	}
	return b
}

func cardIDs(l List) []int64 {
	var out []int64
	for _, c := range l.Cards {
		out = append(out, c.ID)
	}
	return out
}

func TestSeedBigBoard(t *testing.T) {
	a := newApp(t)
	b, err := a.LoadBoard(context.Background(), 2)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, l := range b.Lists {
		n += len(l.Cards)
	}
	if len(b.Lists) != 8 || n != 200 || len(b.Labels) != 8 || len(b.Members) != 4 {
		t.Fatalf("big board: %d lists, %d cards, %d labels, %d members", len(b.Lists), n, len(b.Labels), len(b.Members))
	}
}

func TestMoveCard(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	b := small(t, a)
	todo, doing := b.Lists[0], b.Lists[1]
	c := todo.Cards[0]
	anna := Actor{User: 1, Op: "op1"}

	// To Doing, before its second card.
	out, err := a.MoveCard(ctx, anna, 1, Move{Card: c.ID, List: doing.ID, Before: doing.Cards[1].ID, Seen: c.MoveVersion})
	if err != nil || !out.OK || !strings.Contains(out.Notice, "Doing") {
		t.Fatalf("move: %+v %v", out, err)
	}
	b2 := small(t, a)
	want := []int64{doing.Cards[0].ID, c.ID, doing.Cards[1].ID, doing.Cards[2].ID}
	if got := cardIDs(b2.Lists[1]); !slices.Equal(got, want) {
		t.Fatalf("doing: %v, want %v", got, want)
	}
	if out.Version != b2.Version {
		t.Fatalf("outcome version %d, board %d", out.Version, b2.Version)
	}

	// The same op again: same answer, nothing moves twice.
	again, err := a.MoveCard(ctx, anna, 1, Move{Card: c.ID, List: todo.ID, Seen: c.MoveVersion})
	if err != nil || !again.Replayed || !again.OK || again.Notice != out.Notice {
		t.Fatalf("replay: %+v %v", again, err)
	}

	// Ben's page still shows the old card: refused, and says who moved it where.
	ben := Actor{User: 2, Op: "op2"}
	out, err = a.MoveCard(ctx, ben, 1, Move{Card: c.ID, List: b.Lists[2].ID, Seen: c.MoveVersion})
	if err != nil || out.OK || !strings.Contains(out.Msg, "Anna moved") || !strings.Contains(out.Msg, "Doing") {
		t.Fatalf("stale move: %+v %v", out, err)
	}
	if got := cardIDs(small(t, a).Lists[1]); !slices.Equal(got, want) {
		t.Fatalf("stale move changed the board: %v", got)
	}

	// An anchor that isn't in the target lane any more.
	c2 := small(t, a).Lists[0].Cards[0]
	out, _ = a.MoveCard(ctx, Actor{User: 2, Op: "op3"}, 1, Move{Card: c2.ID, List: b.Lists[2].ID, Before: doing.Cards[0].ID, Seen: c2.MoveVersion})
	if out.OK || !strings.Contains(out.Msg, "Nothing moved") {
		t.Fatalf("bad anchor: %+v", out)
	}
	// Not a member.
	out, _ = a.MoveCard(ctx, Actor{User: 99, Op: "op4"}, 1, Move{Card: c2.ID, List: b.Lists[2].ID, Seen: c2.MoveVersion})
	if out.OK || !strings.Contains(out.Msg, "not a member") {
		t.Fatalf("stranger: %+v", out)
	}
}

func TestRenumberWhenGapRunsOut(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	b := small(t, a)
	todo, doing := b.Lists[0], b.Lists[1]
	for i := 0; i < 14; i++ {
		if out, err := a.CreateCard(ctx, Actor{User: 1}, 1, doing.ID, "filler"); err != nil || !out.OK {
			t.Fatalf("create: %+v %v", out, err)
		}
	}
	// Each card goes in directly before the previous one: the gap halves
	// every time, until the list has to be renumbered.
	first := todo.Cards[0].ID
	var order []int64
	for _, c := range small(t, a).Lists[1].Cards {
		out, err := a.MoveCard(ctx, Actor{User: 1}, 1, Move{Card: c.ID, List: todo.ID, Before: first, Seen: c.MoveVersion})
		if err != nil || !out.OK {
			t.Fatalf("move: %+v %v", out, err)
		}
		first = c.ID
		order = append([]int64{c.ID}, order...)
	}
	got := cardIDs(small(t, a).Lists[0])
	want := append(order, cardIDs(todo)...)
	if !slices.Equal(got, want) {
		t.Fatalf("order\n got %v\nwant %v", got, want)
	}
}

func TestRejectionsAndOps(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	b := small(t, a)
	out, err := a.CreateCard(ctx, Actor{User: 1, Op: "x1"}, 1, b.Lists[0].ID, "   ")
	if err != nil || out.OK || out.Msg == "" {
		t.Fatalf("empty title: %+v %v", out, err)
	}
	// The refusal is kept for the op too.
	again, _ := a.CreateCard(ctx, Actor{User: 1, Op: "x1"}, 1, b.Lists[0].ID, "now with a title")
	if !again.Replayed || again.OK {
		t.Fatalf("replayed refusal: %+v", again)
	}
	out, _ = a.CreateCard(ctx, Actor{User: 1, Op: "x2"}, 1, b.Lists[0].ID, "bell\u0007")
	if out.OK {
		t.Fatal("control character accepted")
	}
	if _, err := a.CreateCard(ctx, Actor{User: 1, Op: "bad op!"}, 1, b.Lists[0].ID, "x"); err == nil {
		t.Fatal("bad op id accepted")
	}
	// Someone else's comment can't be deleted.
	cd, err := a.LoadCard(ctx, 1, b.Lists[0].Cards[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	a.AddComment(ctx, Actor{User: 2}, 1, cd.ID, "mine")
	cd, _ = a.LoadCard(ctx, 1, cd.ID)
	last := cd.CommentList[len(cd.CommentList)-1]
	out, _ = a.DeleteComment(ctx, Actor{User: 1}, 1, last.ID)
	if out.OK {
		t.Fatal("deleted another user's comment")
	}
	out, _ = a.DeleteComment(ctx, Actor{User: 2}, 1, last.ID)
	if !out.OK {
		t.Fatalf("own comment: %+v", out)
	}
}

func TestLogin(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	if _, _, err := a.Login(ctx, "anna@example.com", "wrong"); err != ErrBadLogin {
		t.Fatalf("wrong password: %v", err)
	}
	if _, _, err := a.Login(ctx, "nobody@example.com", "demo"); err != ErrBadLogin {
		t.Fatalf("unknown email: %v", err)
	}
	tok, u, err := a.Login(ctx, "ANNA@example.com", "demo")
	if err != nil || u.Name != "Anna Keller" {
		t.Fatalf("login: %v %+v", err, u)
	}
	s, err := a.Session(ctx, tok)
	if err != nil || s.ID != u.ID {
		t.Fatalf("session: %v", err)
	}
	a.Logout(ctx, tok)
	if _, err := a.Session(ctx, tok); err != ErrNotFound {
		t.Fatalf("after logout: %v", err)
	}
}

func TestReset(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	b := small(t, a)
	tok, _, err := a.Login(ctx, "anna@example.com", "demo")
	if err != nil {
		t.Fatal(err)
	}
	a.CreateCard(ctx, Actor{User: 1}, 1, b.Lists[0].ID, "gone after the reset")
	before := small(t, a).Version
	boards, err := a.Reset(ctx)
	if err != nil || len(boards) != 2 || boards[0] != 1 {
		t.Fatalf("reset: %v %v", boards, err)
	}
	after := small(t, a)
	if after.Version <= before {
		t.Fatalf("version went back: %d -> %d", before, after.Version)
	}
	for _, l := range after.Lists {
		for _, c := range l.Cards {
			if c.Title == "gone after the reset" {
				t.Fatal("card survived the reset")
			}
		}
	}
	if _, err := a.Session(ctx, tok); err != nil {
		t.Fatalf("session lost: %v", err)
	}
}

func TestMoveListRefusesStale(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	b := small(t, a)
	doing := b.Lists[1]
	out, err := a.MoveList(ctx, Actor{User: 1, Op: "l1"}, 1, doing.ID, 0, doing.MoveVersion)
	if err != nil || !out.OK {
		t.Fatalf("move: %+v %v", out, err)
	}
	// Ben's page still shows the old version.
	out, _ = a.MoveList(ctx, Actor{User: 2, Op: "l2"}, 1, doing.ID, b.Lists[0].ID, doing.MoveVersion)
	if out.OK || !strings.Contains(out.Msg, "Anna moved the list") {
		t.Fatalf("stale: %+v", out)
	}
	// Before a list that is gone: refused, nothing moves.
	a.ArchiveList(ctx, Actor{User: 1}, 1, b.Lists[0].ID)
	cur, _ := a.LoadBoard(ctx, 1)
	out, _ = a.MoveList(ctx, Actor{User: 2, Op: "l3"}, 1, doing.ID, b.Lists[0].ID, cur.Lists[len(cur.Lists)-1].MoveVersion)
	if out.OK {
		t.Fatalf("before an archived list: %+v", out)
	}
}

// topics records what the commits touched.
type topics struct {
	mu   sync.Mutex
	seen []string
}

func (r *topics) add(ts []string) {
	r.mu.Lock()
	r.seen = append(r.seen, ts...)
	r.mu.Unlock()
}

func (r *topics) has(t string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return slices.Contains(r.seen, t)
}

type message struct {
	ID, After     int64
	Signals       string
	Status, Toast *string
}

func outbox(t *testing.T, a *App, tab string) []message {
	t.Helper()
	rows, err := a.DB.R.Query(`SELECT id, after, signals, status, toasts FROM tab_outbox WHERE tab = ? ORDER BY id`, tab)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []message
	for rows.Next() {
		var m message
		if err := rows.Scan(&m.ID, &m.After, &m.Signals, &m.Status, &m.Toast); err != nil {
			t.Fatal(err)
		}
		out = append(out, m)
	}
	return out
}

// A command that changes a board bumps its version and touches its topic;
// the outcome goes to the actor's tab in the same command.
func TestOutcomeQueuedForTheTab(t *testing.T) {
	var seen topics
	a := newAppWith(t, seen.add)
	ctx := context.Background()
	b := small(t, a)
	const tab = "tab-of-anna-0000001"
	anna := Actor{User: 1, Op: "c1", Tab: tab, Clear: map[string]any{"_in": map[string]any{"l1": ""}}}
	out, err := a.CreateCard(ctx, anna, 1, b.Lists[0].ID, "Queued card")
	if err != nil || !out.OK || out.Version != b.Version+1 {
		t.Fatalf("create: %+v %v (board was at %d)", out, err, b.Version)
	}
	if !seen.has("board:1") || !seen.has(TabTopic(1, tab)) {
		t.Fatalf("topics %v", seen.seen)
	}
	msgs := outbox(t, a, tab)
	if len(msgs) != 1 || msgs[0].After != out.Version || msgs[0].Toast != nil || msgs[0].Status == nil || !strings.Contains(*msgs[0].Status, "Queued card") {
		t.Fatalf("outbox %+v", msgs)
	}
	var sig map[string]any
	if err := json.Unmarshal([]byte(msgs[0].Signals), &sig); err != nil || sig["_acks"].(map[string]any)["c1"] != "ok" || sig["_in"] == nil {
		t.Fatalf("signals %s", msgs[0].Signals)
	}

	// A refusal: the input stays, a toast says why; its replay adds none.
	bad := Actor{User: 1, Op: "c2", Tab: tab, Clear: anna.Clear}
	for range 2 {
		if out, err := a.CreateCard(ctx, bad, 1, b.Lists[0].ID, " "); err != nil || out.OK {
			t.Fatalf("empty title: %+v %v", out, err)
		}
	}
	msgs = outbox(t, a, tab)
	if len(msgs) != 3 || msgs[1].Toast == nil || msgs[2].Toast != nil || strings.Contains(msgs[1].Signals, "_in") {
		t.Fatalf("outbox after a refusal and its replay: %+v", msgs)
	}
	var toasts []Toast
	if err := json.Unmarshal([]byte(*msgs[1].Toast), &toasts); err != nil || len(toasts) != 1 || toasts[0].Variant != "warn" {
		t.Fatalf("toasts %s", *msgs[1].Toast)
	}

	if err := a.DismissToast(ctx, 1, 1, tab, toasts[0].ID); err != nil {
		t.Fatal(err)
	}
	msgs = outbox(t, a, tab)
	if last := msgs[len(msgs)-1]; last.Toast == nil || *last.Toast != "[]" || last.After != 0 || last.Status != nil {
		t.Fatalf("after dismissing: %+v", last)
	}
	// Another user's actions with this tab id don't reach it.
	if out, _ := a.CreateCard(ctx, Actor{User: 2, Op: "c3", Tab: tab}, 1, b.Lists[0].ID, " "); out.OK {
		t.Fatal("refusal expected")
	}
	if list, _, _ := a.toasts(tab, 1); len(list) != 0 {
		t.Fatalf("another user's toast in anna's tab: %v", list)
	}
}

func (a *App) toasts(tab string, user int64) (list []Toast, n int64, err error) {
	var js string
	err = a.DB.R.QueryRow(`SELECT toasts, toast_n FROM tab_state WHERE tab = ? AND user_id = ?`, tab, user).Scan(&js, &n)
	if err == nil {
		err = json.Unmarshal([]byte(js), &list)
	}
	return list, n, err
}

// exec runs one statement through the writer.
func (a *App) exec(ctx context.Context, query string, args ...any) error {
	return a.DB.W.Do(ctx, writer.Func(func(tx *writer.Tx) error {
		_, err := tx.Exec(query, args...)
		return err
	}))
}

func TestSweep(t *testing.T) {
	a := newApp(t)
	ctx := context.Background()
	b := small(t, a)
	for i := range 3 {
		if _, err := a.CreateCard(ctx, Actor{User: 1, Tab: "sweep-tab-000000001"}, 1, b.Lists[0].ID, "c"+string(rune('a'+i))); err != nil {
			t.Fatal(err)
		}
	}
	old := time.Now().Add(-11 * time.Minute).Unix()
	ids := outbox(t, a, "sweep-tab-000000001")
	if err := a.exec(ctx, `UPDATE tab_outbox SET created_at = ? WHERE id < ?`, old, ids[2].ID); err != nil {
		t.Fatal(err)
	}
	if err := a.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if left := outbox(t, a, "sweep-tab-000000001"); len(left) != 1 || left[0].ID != ids[2].ID {
		t.Fatalf("left %+v", left)
	}
	if err := a.exec(ctx, `UPDATE tab_outbox SET created_at = ?`, old); err != nil {
		t.Fatal(err)
	}
	if err := a.Sweep(ctx); err != nil {
		t.Fatal(err)
	}
	if left := outbox(t, a, "sweep-tab-000000001"); len(left) != 0 {
		t.Fatalf("left %+v", left)
	}
	// Ids never come back: a page that has id 3 must not ignore the next message.
	if _, err := a.CreateCard(ctx, Actor{User: 1, Tab: "sweep-tab-000000001"}, 1, b.Lists[0].ID, "after"); err != nil {
		t.Fatal(err)
	}
	if next := outbox(t, a, "sweep-tab-000000001"); len(next) != 1 || next[0].ID <= ids[2].ID {
		t.Fatalf("after the sweep: %+v", next)
	}
}
