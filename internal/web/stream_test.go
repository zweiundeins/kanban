package web

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	lithlive "github.com/zweiundeins/lith/live"

	"kanban/internal/app"
	"kanban/internal/db"
	"kanban/internal/live"
	"kanban/internal/views"
	webfiles "kanban/web"
)

// testOptions change testServer's defaults.
type testOptions struct {
	actions       int // actions per user and 10 s (0: no limit)
	clientStreams int // streams per client address (0: lith's default)
}

// testServer runs the whole server on the database at path, seeding it
// first if it is new. Its hub runs the build "test".
func testServer(t *testing.T, path string, opts ...testOptions) (*httptest.Server, *app.App, func()) {
	t.Helper()
	var o testOptions
	if len(opts) > 0 {
		o = opts[0]
	}
	var hub *lithlive.Hub
	d, err := db.Open(context.Background(), path, db.Options{Synchronous: "OFF", OnCommit: func(ts []string) { hub.Changed(ts) }})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	hub = lithlive.New(lithlive.Options{Read: d.R, Tick: 5 * time.Millisecond, Heartbeat: 50 * time.Millisecond, HeartbeatSignal: views.PingSignal,
		MaxStreamsPerClient: o.clientStreams, Build: "test", Stopping: Stopping, Log: log})
	ctx, stopHub := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- hub.Run(ctx) }()
	a := &app.App{DB: d}
	if _, err := a.UserByID(context.Background(), 1); err != nil {
		if err := a.Seed(context.Background(), "", ""); err != nil {
			t.Fatal(err)
		}
	}
	sub, _ := fs.Sub(webfiles.FS, "static")
	static, err := NewStatic(sub)
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{App: a, Hub: hub, Seats: live.NewSeats(hub.Changed, time.Second), Render: NewRenderer(), Static: static, Log: log, Started: time.Now(), Actions: o.actions}
	srv := httptest.NewServer(s.Handler("test"))
	stop := func() {
		stopHub()
		<-done
		srv.Close()
		d.Close()
	}
	return srv, a, stop
}

type event struct{ name, data string }

// openStream opens a tab's stream as a browser does and returns its
// events as they come.
func openStream(t *testing.T, base, cookie, tab, signals string) (<-chan event, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, "GET", base+"/boards/1/live?tab="+tab+"&datastar="+url.QueryEscape(signals), nil)
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Accept-Encoding", "gzip, deflate, br, zstd")
	res, err := http.DefaultTransport.RoundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != 200 || res.Header.Get("Content-Encoding") != "zstd" {
		t.Fatalf("stream: %s, %q", res.Status, res.Header.Get("Content-Encoding"))
	}
	zr, err := zstd.NewReader(res.Body)
	if err != nil {
		t.Fatal(err)
	}
	events := make(chan event, 1000)
	go func() {
		defer close(events)
		defer zr.Close()
		sc := bufio.NewScanner(zr)
		sc.Buffer(nil, 4<<20)
		var ev event
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event: "):
				ev.name = line[7:]
			case strings.HasPrefix(line, "data: "):
				ev.data += line[6:] + "\n"
			case line == "":
				if ev.name != "" {
					events <- ev
				}
				ev = event{}
			}
		}
	}()
	return events, func() { cancel(); res.Body.Close() }
}

// until reads events until one contains want, and returns those read.
func until(t *testing.T, events <-chan event, want string) []event {
	t.Helper()
	var seen []event
	timeout := time.After(5 * time.Second)
	for {
		select {
		case ev, ok := <-events:
			if !ok {
				t.Fatalf("the stream ended before %q", want)
			}
			seen = append(seen, ev)
			noRefusedURL(t, "an event", ev.data)
			if strings.Contains(ev.data, want) {
				return seen
			}
		case <-timeout:
			t.Fatalf("no %q in %d events", want, len(seen))
		}
	}
}

// short is events for a failure message.
func short(events []event) []string {
	var out []string
	for _, ev := range events {
		out = append(out, ev.data[:min(len(ev.data), 80)])
	}
	return out
}

// post sends an action as the page does: tab and op in headers, the rest
// in the body.
func post(t *testing.T, base, cookie, path, tab, op, body string) {
	t.Helper()
	if res := send(t, base, cookie, path, tab, op, body); res.StatusCode != http.StatusNoContent {
		t.Fatalf("POST %s: %s", path, res.Status)
	}
}

func send(t *testing.T, base, cookie, path, tab, op, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", base)
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Tab-Id", tab)
	req.Header.Set("Op-Id", op)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if len(b) > 0 {
		t.Errorf("POST %s: %d answered with text %q", path, res.StatusCode, b)
	}
	return res
}

// A move's outcome reaches its tab after the frame that shows the move;
// what the tab was told survives a restart, and a page that reconnects
// gets only what it lacks.
func TestOutcomeAfterFrameAndAcrossRestarts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "k.db")
	srv, a, stop := testServer(t, path)
	ctx := context.Background()
	token, _, err := a.Login(ctx, "anna@example.com", "demo")
	if err != nil {
		t.Fatal(err)
	}
	cookie := sessionCookie + "=" + token
	b, _ := a.LoadBoard(ctx, 1)
	card, doing := b.Lists[0].Cards[0], b.Lists[1]
	const tab = "outcome-tab-0000001"

	events, close := openStream(t, srv.URL, cookie, tab, `{"card":0,"outbox":0}`)
	until(t, events, `id="lanes"`)
	post(t, srv.URL, cookie, fmt.Sprintf("/boards/1/cards/%d/move", card.ID), tab, "m1", fmt.Sprintf(`{"col":%d,"before":"","seen":%d}`, doing.ID, card.MoveVersion))
	seen := until(t, events, `"m1":"ok"`)
	moved := regexp.MustCompile(fmt.Sprintf(`(?s)id="list-%d".*data-kanban-card="%d"`, doing.ID, card.ID))
	frame := -1
	for i, ev := range seen {
		if strings.Contains(ev.data, `id="lanes"`) && moved.MatchString(ev.data) {
			frame = i
		}
	}
	if frame < 0 || frame == len(seen)-1 {
		t.Fatalf("the outcome came without a frame showing the move before it: %v", short(seen))
	}
	if !strings.Contains(seen[len(seen)-1].data, `"outbox":`) {
		t.Fatalf("no outbox position: %q", seen[len(seen)-1].data)
	}
	close()

	// A refusal while no stream is open, then a restart.
	post(t, srv.URL, cookie, fmt.Sprintf("/boards/1/cards/%d/move", card.ID), tab, "m2", fmt.Sprintf(`{"col":%d,"before":"","seen":%d}`, b.Lists[2].ID, card.MoveVersion))
	stop()
	srv, _, stop = testServer(t, path)
	defer stop()
	events, close = openStream(t, srv.URL, cookie, tab, `{"card":0,"outbox":0}`)
	seen = until(t, events, `"m2":`)
	all := fmt.Sprint(seen)
	if !strings.Contains(all, `"m1":"ok"`) || !strings.Contains(all, "You moved") || !strings.Contains(all, `sb-toast`) {
		t.Fatalf("after the restart: %v", short(seen))
	}
	close()
	// The page has the first message (its id is 1).
	events, close = openStream(t, srv.URL, cookie, tab, `{"card":0,"outbox":1}`)
	defer close()
	seen = until(t, events, `"m2":`)
	if strings.Contains(fmt.Sprint(seen), `"m1":`) {
		t.Fatalf("a message the page has came again: %v", short(seen))
	}
}

// session logs the user in and returns the session's cookie.
func session(t *testing.T, a *app.App, email string) string {
	t.Helper()
	token, _, err := a.Login(context.Background(), email, "demo")
	if err != nil {
		t.Fatal(err)
	}
	return sessionCookie + "=" + token
}

// page fetches a page with the cookie.
func page(t *testing.T, url, cookie string) string {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	req.Header.Set("Cookie", cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(res.Body)
	noRefusedURL(t, url, string(body))
	return string(body)
}

// noRefusedURL fails on an attribute templ refused as a URL: since
// v0.3.1100 templ classifies attributes the way html/template does.
func noRefusedURL(t *testing.T, where, html string) {
	t.Helper()
	if i := strings.Index(html, "TemplFailedSanitization"); i >= 0 {
		t.Errorf("%s has an attribute templ refused as a URL: %.200s", where, html[max(0, i-150):])
	}
}

// A page drawn now starts at the newest message of any tab, since its own
// tab's messages all come later; its stream reads only those.
func TestPageStartsAtTheOutboxEnd(t *testing.T) {
	srv, a, stop := testServer(t, filepath.Join(t.TempDir(), "k.db"))
	defer stop()
	cookie := session(t, a, "anna@example.com")
	post(t, srv.URL, cookie, "/boards/1/lists", "another-tab-0000001", "o1", `{"name":"Other"}`)
	end, err := app.OutboxEnd(context.Background(), a.DB.R)
	if err != nil || end == 0 {
		t.Fatalf("the outbox ends at %d, %v", end, err)
	}
	m := regexp.MustCompile(`&#34;outbox&#34;:(\d+)`).FindStringSubmatch(page(t, srv.URL+"/boards/1", cookie))
	if m == nil || m[1] != fmt.Sprint(end) {
		t.Fatalf("the page starts at %v, the outbox ends at %d", m, end)
	}
	const tab = "fresh-tab-00000001"
	events, close := openStream(t, srv.URL, cookie, tab, fmt.Sprintf(`{"card":0,"outbox":%d}`, end))
	defer close()
	until(t, events, `id="lanes"`)
	post(t, srv.URL, cookie, "/boards/1/lists", tab, "f1", `{"name":"Fresh"}`)
	until(t, events, `"f1":"ok"`)
}

// tab_outbox has no index on the tab: the tab's view walks the ids after
// the newest message it has, and must never scan the whole table.
func TestOutboxQueryWalksTheIDs(t *testing.T) {
	d, err := db.Open(context.Background(), filepath.Join(t.TempDir(), "k.db"), db.Options{Synchronous: "OFF"})
	if err != nil {
		t.Fatal(err)
	}
	defer d.Close()
	rows, err := d.R.Query("EXPLAIN QUERY PLAN "+outboxQuery, 7, "tab", 1, keepMessages)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if len(plan) != 1 || !strings.Contains(plan[0], "INTEGER PRIMARY KEY (rowid>?)") {
		t.Fatalf("plan %q", plan)
	}
}

// The page's watchdog watches the heartbeat's signal: every heartbeat
// patches it with a bigger number.
func TestHeartbeatPatchesThePing(t *testing.T) {
	srv, a, stop := testServer(t, filepath.Join(t.TempDir(), "k.db"))
	defer stop()
	cookie := session(t, a, "anna@example.com")
	doc := page(t, srv.URL+"/boards/1", cookie)
	if !strings.Contains(doc, `data-effect="$`+views.PingSignal+`, `) || !strings.Contains(doc, `&#34;`+views.PingSignal+`&#34;:0`) {
		t.Fatalf("the page doesn't watch $%s", views.PingSignal)
	}

	events, close := openStream(t, srv.URL, cookie, "heartbeat-tab-000001", `{"card":0,"outbox":0}`)
	defer close()
	var pings []int64
	for len(pings) < 3 {
		ev := until(t, events, `"`+views.PingSignal+`":`)
		var sig map[string]int64
		data := strings.TrimPrefix(strings.TrimSpace(ev[len(ev)-1].data), "signals ")
		if err := json.Unmarshal([]byte(data), &sig); err != nil {
			t.Fatalf("%q: %v", data, err)
		}
		pings = append(pings, sig[views.PingSignal])
	}
	if pings[0] >= pings[1] || pings[1] >= pings[2] {
		t.Fatalf("the heartbeat's numbers don't grow: %v", pings)
	}
}
