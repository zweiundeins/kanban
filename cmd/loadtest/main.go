// Command loadtest puts simulated people on one board: each opens the page
// and its stream (brotli, like a browser) and moves random cards. It
// measures what a person waits for: from sending a move to its outcome
// arriving on their own stream, after the frame that shows it. Moves go out
// on schedule, whether or not the last one was answered, so a slow server
// doesn't get less work.
//
//	go run ./cmd/loadtest -base http://127.0.0.1:8080 -tabs 50 -board 2 -duration 30s -interval 1s
package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/andybalholm/brotli"
)

var (
	base     = flag.String("base", "http://127.0.0.1:8080", "server")
	tabs     = flag.Int("tabs", 50, "simulated tabs (spread over the four demo accounts)")
	board    = flag.Int("board", 2, "board id")
	duration = flag.Duration("duration", 30*time.Second, "how long to move cards")
	interval = flag.Duration("interval", time.Second, "time between two moves of one tab (jittered)")
	accounts = flag.String("accounts", "anna@example.com,ben@example.com,chiara@example.com,dev@example.com", "demo accounts")
	out      = flag.String("out", "", "also write the results to this JSON file")
)

// One connection pool for every tab, big enough that no request waits for
// a connection or opens a new one.
var transport = func() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	t.MaxIdleConns = 0
	t.MaxIdleConnsPerHost = 10_000
	return t
}()

var (
	tabRe  = regexp.MustCompile(`&#34;_tab&#34;:&#34;([A-Za-z0-9_-]+)&#34;|"_tab":"([A-Za-z0-9_-]+)"`)
	cardRe = regexp.MustCompile(`data-kanban-card="(\d+)" data-seen="(\d+)"`)
	laneRe = regexp.MustCompile(`data-kanban-lane data-col="(\d+)"`)
	csrfRe = regexp.MustCompile(`name="csrf" value="([^"]+)"`)
)

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}

type stats struct {
	mu                  sync.Mutex
	confirmed           []time.Duration // send -> outcome on the stream
	responded           []time.Duration // send -> 204
	ok, refused, failed int
	frames              atomic.Int64
	wire                atomic.Int64
}

func login(email string) (*http.Client, error) {
	jar, _ := cookiejar.New(nil)
	c := &http.Client{Jar: jar, Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	res, err := c.Get(*base + "/login")
	if err != nil {
		return nil, err
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	m := csrfRe.FindSubmatch(body)
	if m == nil {
		return nil, fmt.Errorf("no csrf token")
	}
	form := url.Values{"csrf": {string(m[1])}, "email": {email}, "password": {"demo"}}
	req, _ := http.NewRequest("POST", *base+"/login", strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", *base)
	res, err = c.Do(req)
	if err != nil {
		return nil, err
	}
	res.Body.Close()
	if res.StatusCode != http.StatusSeeOther {
		return nil, fmt.Errorf("login %s: %s", email, res.Status)
	}
	return c, nil
}

type tab struct {
	c       *http.Client
	id      string
	mu      sync.Mutex
	cards   map[int64]int64 // card id -> the move version the page shows
	lanes   []int64
	pending sync.Map // op -> time sent
	st      *stats
}

func (t *tab) open(ctx context.Context) error {
	res, err := t.c.Get(fmt.Sprintf("%s/boards/%d", *base, *board))
	if err != nil {
		return err
	}
	body, _ := io.ReadAll(res.Body)
	res.Body.Close()
	m := tabRe.FindSubmatch(body)
	if m == nil {
		return fmt.Errorf("no tab id in the page (%s)", res.Status)
	}
	t.id = string(m[1]) + string(m[2])
	t.parse(body, true)
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/boards/%d/live?tab=%s", *base, *board, t.id), nil)
	req.Header.Set("Accept-Encoding", "br")
	req.Header.Set("Datastar-Request", "true")
	res, err = t.c.Do(req)
	if err != nil {
		return err
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("stream: %s", res.Status)
	}
	go t.read(res.Body)
	return nil
}

// parse merges what a page or a frame shows: a frame carries only the lanes
// that changed (the others are empty placeholders), so cards it doesn't
// mention keep what the tab knew.
func (t *tab) parse(html []byte, full bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.cards == nil {
		t.cards = map[int64]int64{}
	}
	for _, c := range cardRe.FindAllSubmatch(html, -1) {
		id, _ := strconv.ParseInt(string(c[1]), 10, 64)
		seen, _ := strconv.ParseInt(string(c[2]), 10, 64)
		t.cards[id] = seen
	}
	if full {
		t.lanes = t.lanes[:0]
		for _, l := range laneRe.FindAllSubmatch(html, -1) {
			id, _ := strconv.ParseInt(string(l[1]), 10, 64)
			t.lanes = append(t.lanes, id)
		}
	}
}

func (t *tab) read(body io.ReadCloser) {
	defer body.Close()
	sc := bufio.NewScanner(brotli.NewReader(countingReader{body, &t.st.wire}))
	sc.Buffer(make([]byte, 0, 1<<20), 16<<20)
	var event string
	var data bytes.Buffer
	for sc.Scan() {
		line := sc.Bytes()
		switch {
		case bytes.HasPrefix(line, []byte("event: ")):
			event = string(line[7:])
		case bytes.HasPrefix(line, []byte("data: ")):
			data.Write(line[6:])
			data.WriteByte('\n')
		case len(line) == 0:
			switch event {
			case "datastar-patch-elements":
				if bytes.Contains(data.Bytes(), []byte(`id="lanes"`)) {
					t.st.frames.Add(1)
					t.parse(data.Bytes(), false)
				}
			case "datastar-patch-signals":
				t.acks(bytes.TrimPrefix(bytes.TrimSpace(data.Bytes()), []byte("signals ")))
			}
			event = ""
			data.Reset()
		}
	}
}

func (t *tab) acks(js []byte) {
	var sig struct {
		Acks map[string]string `json:"_acks"`
	}
	if json.Unmarshal(js, &sig) != nil {
		return
	}
	for op, result := range sig.Acks {
		if v, ok := t.pending.LoadAndDelete(op); ok {
			d := time.Since(v.(time.Time))
			t.st.mu.Lock()
			t.st.confirmed = append(t.st.confirmed, d)
			if result == "ok" {
				t.st.ok++
			} else {
				t.st.refused++
			}
			t.st.mu.Unlock()
		}
	}
}

func (t *tab) move(n int) {
	t.mu.Lock()
	if len(t.cards) == 0 || len(t.lanes) == 0 {
		t.mu.Unlock()
		return
	}
	n0 := rand.IntN(len(t.cards))
	var c [2]int64
	for id, seen := range t.cards {
		if n0 == 0 {
			c = [2]int64{id, seen}
			break
		}
		n0--
	}
	lane := t.lanes[rand.IntN(len(t.lanes))]
	t.mu.Unlock()
	op := fmt.Sprintf("lt%s%d", strings.ReplaceAll(t.id[:8], "-", "x"), n)
	payload, _ := json.Marshal(map[string]any{"tab": t.id, "op": op, "col": lane, "before": "", "seen": c[1]})
	req, _ := http.NewRequest("POST", fmt.Sprintf("%s/boards/%d/cards/%d/move", *base, *board, c[0]), bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Datastar-Request", "true")
	req.Header.Set("Origin", *base)
	start := time.Now()
	t.pending.Store(op, start)
	res, err := t.c.Do(req)
	if err == nil {
		io.Copy(io.Discard, res.Body)
		res.Body.Close()
	}
	t.st.mu.Lock()
	defer t.st.mu.Unlock()
	if err != nil || res.StatusCode != http.StatusNoContent {
		t.pending.Delete(op)
		t.st.failed++
		return
	}
	t.st.responded = append(t.st.responded, time.Since(start))
}

func pct(ds []time.Duration, p float64) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	s := slices.Clone(ds)
	slices.Sort(s)
	return s[min(len(s)-1, int(float64(len(s))*p))]
}

func main() {
	flag.Parse()
	st := &stats{}
	emails := strings.Split(*accounts, ",")
	clients := make([]*http.Client, len(emails))
	for i, e := range emails {
		c, err := login(e)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		clients[i] = c
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ts := make([]*tab, *tabs)
	for i := range ts {
		ts[i] = &tab{c: clients[i%len(clients)], st: st}
		if err := ts[i].open(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "tab", i, err)
			os.Exit(1)
		}
	}
	time.Sleep(time.Second)
	framesBefore, wireBefore := st.frames.Load(), st.wire.Load()
	before := health()
	cpu0 := cpuSeconds()
	fmt.Printf("%d tabs on board %d, a move every ~%s each, for %s\n", *tabs, *board, *interval, *duration)
	var wg, inflight sync.WaitGroup
	start := time.Now()
	deadline := start.Add(*duration)
	for _, t := range ts {
		wg.Go(func() {
			time.Sleep(time.Duration(rand.Int64N(int64(*interval))))
			for n := 0; time.Now().Before(deadline); n++ {
				inflight.Go(func() { t.move(n) })
				time.Sleep(*interval/2 + time.Duration(rand.Int64N(int64(*interval))))
			}
		})
	}
	wg.Wait()
	elapsed := time.Since(start)
	inflight.Wait()
	time.Sleep(2 * time.Second) // the last outcomes
	after := health()
	toolCPU := cpuSeconds() - cpu0
	frames, wire := st.frames.Load()-framesBefore, st.wire.Load()-wireBefore
	unanswered := 0
	for _, t := range ts {
		t.pending.Range(func(any, any) bool { unanswered++; return true })
	}
	st.mu.Lock()
	defer st.mu.Unlock()
	sent := len(st.responded) + st.failed
	r := map[string]any{
		"tabs": *tabs, "interval_ms": interval.Milliseconds(), "duration_s": duration.Seconds(),
		"moves": sent, "moves_per_s": float64(sent) / elapsed.Seconds(),
		"confirmed": st.ok, "refused": st.refused, "failed": st.failed, "unanswered": unanswered,
		"ms_to_204":            ms(st.responded),
		"ms_to_outcome":        ms(st.confirmed),
		"frames_per_tab_s":     float64(frames) / float64(*tabs) / elapsed.Seconds(),
		"wire_bytes_per_frame": float64(wire) / max(float64(frames), 1),
		"tool_cpu_cores":       toolCPU / (elapsed.Seconds() + 2),
	}
	if before != nil && after != nil {
		d := func(path ...string) float64 { return num(after, path...) - num(before, path...) }
		batches := max(d("writer", "batches"), 1)
		r["server"] = map[string]any{
			"cpu_cores":       d("process", "cpu_s") / (elapsed.Seconds() + 2),
			"peak_rss_mb":     num(after, "process", "peak_rss_mb"),
			"gomaxprocs":      num(after, "process", "gomaxprocs"),
			"alloc_mb_per_s":  d("memory", "total_alloc") / 1e6 / (elapsed.Seconds() + 2),
			"gc_cycles":       d("memory", "gc_cycles"),
			"batches":         batches,
			"moves_per_batch": d("writer", "commands") / batches,
			"ms_per_batch":    d("writer", "commit_nanos") / batches / 1e6,
			"ms_queue_wait":   d("writer", "wait_nanos") / max(d("writer", "commands"), 1) / 1e6,
			"writer_busy":     d("writer", "commit_nanos") / 1e9 / (elapsed.Seconds() + 2),
			"max_batch":       num(after, "writer", "max_batch"),
			"renders":         d("live", "renders"),
			"ms_per_render":   d("live", "render_nanos") / max(d("live", "renders"), 1) / 1e6,
			"frames_sent":     d("live", "frames_sent"),
			"frames_skipped":  d("live", "frames_skipped"),
		}
	}
	js, _ := json.MarshalIndent(r, "", "  ")
	fmt.Println(string(js))
	if *out != "" {
		if err := os.WriteFile(*out, append(js, '\n'), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
	}
}

func ms(ds []time.Duration) map[string]float64 {
	f := func(d time.Duration) float64 { return float64(d.Microseconds()) / 1000 }
	return map[string]float64{"p50": f(pct(ds, .5)), "p95": f(pct(ds, .95)), "p99": f(pct(ds, .99)), "max": f(pct(ds, 1))}
}

func health() map[string]any {
	res, err := http.Get(*base + "/_health")
	if err != nil {
		return nil
	}
	defer res.Body.Close()
	var h map[string]any
	if json.NewDecoder(res.Body).Decode(&h) != nil {
		return nil
	}
	return h
}

func num(m map[string]any, path ...string) float64 {
	var v any = m
	for _, p := range path {
		mm, ok := v.(map[string]any)
		if !ok {
			return 0
		}
		v = mm[p]
	}
	f, _ := v.(float64)
	return f
}

func cpuSeconds() float64 {
	var ru syscall.Rusage
	if syscall.Getrusage(syscall.RUSAGE_SELF, &ru) != nil {
		return 0
	}
	return float64(ru.Utime.Nano()+ru.Stime.Nano()) / 1e9
}
