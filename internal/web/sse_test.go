package web

import (
	"io"
	"net/http/httptest"
	"testing"

	"github.com/andybalholm/brotli"

	"kanban/internal/live"
)

func TestPickEncoding(t *testing.T) {
	for in, want := range map[string]string{
		"gzip, deflate, br, zstd": "br",
		"br;q=0, gzip;q=0.5":      "gzip",
		"identity":                "",
		"":                        "",
	} {
		if got := pickEncoding(in); got != want {
			t.Errorf("%q: %q, want %q", in, got, want)
		}
	}
}

func TestEventStreamBrotli(t *testing.T) {
	ev := live.Elements([]byte("<p id=\"a\">\n</p>"))
	want := "event: datastar-patch-elements\ndata: elements <p id=\"a\">\ndata: elements </p>\n\n"
	if got := string(ev.Wire()); got != want {
		t.Fatalf("wire %q", got)
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("Accept-Encoding", "gzip, br")
	w := httptest.NewRecorder()
	es, err := openEventStream(w, r, 4, 18)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if err := es.write(ev.Wire()); err != nil {
			t.Fatal(err)
		}
	}
	es.close()
	if w.Header().Get("Content-Encoding") != "br" || w.Header().Get("Content-Type") != "text/event-stream" {
		t.Fatalf("headers %v", w.Header())
	}
	body, err := io.ReadAll(brotli.NewReader(w.Body))
	if err != nil {
		t.Fatal(err)
	}
	if string(body) != want+want {
		t.Fatalf("body %q", body)
	}
}

func TestAcceptsEncoding(t *testing.T) {
	for _, c := range []struct {
		accept, enc string
		want        bool
	}{
		{"gzip, br;q=0.5", "br", true},
		{"gzip, br;q=0", "br", false},
		{"gzip, br; q=0.0", "br", false},
		{"GZIP", "gzip", true},
		{"deflate", "gzip", false},
	} {
		if got := acceptsEncoding(c.accept, c.enc); got != c.want {
			t.Errorf("%q %s: %v", c.accept, c.enc, got)
		}
	}
}
