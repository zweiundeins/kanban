package web

import (
	"bufio"
	"context"
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
	webfiles "kanban/web"
)

// testServer runs the whole server on the database at path, seeding it
// first if it is new.
func testServer(t *testing.T, path string) (*httptest.Server, *app.App, func()) {
	t.Helper()
	var hub *lithlive.Hub
	d, err := db.Open(context.Background(), path, db.Options{Synchronous: "NORMAL", OnCommit: func(ts []string) { hub.Changed(ts) }})
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.DiscardHandler)
	hub = lithlive.New(lithlive.Options{Read: d.R, Tick: 5 * time.Millisecond, Log: log})
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
	s := &Server{App: a, Hub: hub, Seats: live.NewSeats(hub.Changed, time.Second), Render: NewRenderer(), Static: static, Log: log, Started: time.Now()}
	srv := httptest.NewServer(s.Handler())
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

func post(t *testing.T, base, cookie, path, body string) {
	t.Helper()
	req, _ := http.NewRequest("POST", base+path, strings.NewReader(body))
	req.Header.Set("Cookie", cookie)
	req.Header.Set("Origin", base)
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Content-Type", "application/json")
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusNoContent {
		t.Fatalf("POST %s: %s", path, res.Status)
	}
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
	post(t, srv.URL, cookie, fmt.Sprintf("/boards/1/cards/%d/move", card.ID), fmt.Sprintf(`{"tab":%q,"op":"m1","col":%d,"before":"","seen":%d}`, tab, doing.ID, card.MoveVersion))
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
	post(t, srv.URL, cookie, fmt.Sprintf("/boards/1/cards/%d/move", card.ID), fmt.Sprintf(`{"tab":%q,"op":"m2","col":%d,"before":"","seen":%d}`, tab, b.Lists[2].ID, card.MoveVersion))
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
