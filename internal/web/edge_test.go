package web

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/zweiundeins/lith/datastar"

	"kanban/internal/views"
	webfiles "kanban/web"
)

// get answers a GET without a cookie: its status and body.
func get(t *testing.T, url string) (int, string) {
	t.Helper()
	res, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	b, _ := io.ReadAll(res.Body)
	noRefusedURL(t, url, string(b))
	return res.StatusCode, string(b)
}

// The probes answer without a session; the page loads lith's Datastar.
func TestProbes(t *testing.T) {
	srv, a, stop := testServer(t, filepath.Join(t.TempDir(), "k.db"))
	defer stop()
	if code, body := get(t, srv.URL+"/healthz"); code != 200 || body != "ok test\n" {
		t.Fatalf("/healthz: %d %q", code, body)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		code, body := get(t, srv.URL+"/readyz")
		if code == 200 && body == "ready\n" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/readyz: %d %q", code, body)
		}
		time.Sleep(20 * time.Millisecond)
	}
	doc := page(t, srv.URL+"/boards/1", session(t, a, "anna@example.com"))
	src := regexp.MustCompile(`<script type="module" src="([^"]+datastar-rocket[^"]+)"`).FindStringSubmatch(doc)
	if src == nil {
		t.Fatal("the page loads no Datastar")
	}
	if code, body := get(t, srv.URL+src[1]); code != 200 || body != string(datastar.JS()) {
		t.Fatalf("%s: %d, %d bytes, not lith's build", src[1], code, len(body))
	}
}

// The vendored Starbase components come from a Starbase commit whose
// Datastar is lith's: two builds of Rocket on one page break each other.
func TestStarbaseMatchesDatastar(t *testing.T) {
	version, err := fs.ReadFile(webfiles.FS, "static/vendor/VERSION")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`sha256 ([0-9a-f]{64})`).FindSubmatch(version)
	if m == nil {
		t.Fatal("web/static/vendor/VERSION names no Datastar hash")
	}
	sum := sha256.Sum256(datastar.JS())
	if got := hex.EncodeToString(sum[:]); string(m[1]) != got {
		t.Fatalf("Starbase was vendored with Datastar %s, lith serves %s (%s): run task starbase with a matching ref", m[1], got, datastar.Version)
	}
}

// A status code carries no text. An action over the rate limit gets a bare
// 429, and its tab a notice that ends the op with the reason.
func TestStatusesAndNotices(t *testing.T) {
	srv, a, stop := testServer(t, filepath.Join(t.TempDir(), "k.db"), testOptions{actions: 1})
	defer stop()
	cookie := session(t, a, "anna@example.com")
	const tab = "notice-tab-0000001"
	events, close := openStream(t, srv.URL, cookie, tab, `{"card":0,"outbox":0}`)
	defer close()
	until(t, events, `id="lanes"`)

	post(t, srv.URL, cookie, "/boards/1/lists", tab, "r1", `{"name":"One"}`)
	until(t, events, `"r1":"ok"`)
	if res := send(t, srv.URL, cookie, "/boards/1/lists", tab, "r2", `{"name":"Two"}`); res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("over the limit: %s", res.Status)
	}
	seen := until(t, events, `"r2":"Too many changes`)
	if !strings.Contains(seen[len(seen)-1].data, `"_net":"Too many changes`) {
		t.Fatalf("the notice doesn't say why: %q", seen[len(seen)-1].data)
	}

	if res := send(t, srv.URL, cookie, "/boards/1/lists", "", "r3", `{"name":"Three"}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("without a tab: %s", res.Status)
	}
	if res := send(t, srv.URL, cookie, "/boards/1/lists", tab, "bad op!", `{}`); res.StatusCode != http.StatusBadRequest {
		t.Fatalf("a bad op: %s", res.Status)
	}
	req, _ := http.NewRequest("GET", srv.URL+"/boards/1/live?tab=short", nil)
	req.Header.Set("Cookie", cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(res.Body)
	res.Body.Close()
	if res.StatusCode != http.StatusBadRequest || len(b) > 0 {
		t.Fatalf("a stream with a bad tab: %s %q", res.Status, b)
	}
}

// A page that a former build rendered with other scripts or stylesheet is
// asked to reload; one with the same assets only learns the new build.
func TestRefreshAfterDeploy(t *testing.T) {
	srv, a, stop := testServer(t, filepath.Join(t.TempDir(), "k.db"))
	defer stop()
	cookie := session(t, a, "anna@example.com")
	m := regexp.MustCompile(`&#34;_assets&#34;:&#34;([0-9a-f]+)&#34;`).FindStringSubmatch(page(t, srv.URL+"/boards/1", cookie))
	if m == nil {
		t.Fatal("the page has no assets version")
	}

	events, close := openStream(t, srv.URL, cookie, "refresh-tab-000001", `{"card":0,"outbox":0,"_build":"old","_assets":"old"}`)
	until(t, events, `"`+views.UpdateSignal+`":true`)
	until(t, events, `"_build":"test"`)
	close()

	events, close = openStream(t, srv.URL, cookie, "refresh-tab-000002", fmt.Sprintf(`{"card":0,"outbox":0,"_build":"old","_assets":%q}`, m[1]))
	defer close()
	seen := until(t, events, `"_build":"test"`)
	if strings.Contains(fmt.Sprint(seen), views.UpdateSignal) {
		t.Fatalf("a page with the same assets was asked to reload: %v", short(seen))
	}
}

// One client address holds at most MaxStreamsPerClient streams.
func TestStreamsPerClient(t *testing.T) {
	srv, a, stop := testServer(t, filepath.Join(t.TempDir(), "k.db"), testOptions{clientStreams: 1})
	defer stop()
	cookie := session(t, a, "anna@example.com")
	events, close := openStream(t, srv.URL, cookie, "client-tab-0000001", `{"card":0,"outbox":0}`)
	defer close()
	until(t, events, `id="lanes"`)
	req, _ := http.NewRequest("GET", srv.URL+"/boards/1/live?tab=client-tab-0000002", nil)
	req.Header.Set("Cookie", cookie)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if res.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("a second stream from the address: %s", res.Status)
	}
}

// A deploy ends each stream with the reason in the offline banner, then
// sse.Reconnect.
func TestStoppingSaysWhy(t *testing.T) {
	srv, a, stop := testServer(t, filepath.Join(t.TempDir(), "k.db"))
	cookie := session(t, a, "anna@example.com")
	events, close := openStream(t, srv.URL, cookie, "stopping-tab-00001", `{"card":0,"outbox":0}`)
	defer close()
	until(t, events, `id="lanes"`)
	go stop()
	until(t, events, views.Updating)
	last := until(t, events, "cloneNode")
	if !strings.Contains(last[len(last)-1].data, `getElementById(&#34;`+views.StreamID+`&#34;)`) {
		t.Fatalf("the reconnect doesn't reopen #%s: %v", views.StreamID, short(last))
	}
}
