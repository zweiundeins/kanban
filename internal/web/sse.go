package web

import (
	"compress/gzip"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/andybalholm/brotli"
)

// eventStream writes formatted events to one page. Brotli's window spans
// the whole stream, so a frame that repeats an earlier one costs little.
type eventStream struct {
	w   io.Writer
	enc interface {
		Flush() error
		Close() error
	}
	rc *http.ResponseController
}

func openEventStream(w http.ResponseWriter, r *http.Request, level, lgwin int) (*eventStream, error) {
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Add("Vary", "Accept-Encoding")
	if r.ProtoMajor == 1 {
		h.Set("Connection", "keep-alive")
	}
	s := &eventStream{w: w, rc: http.NewResponseController(w)}
	switch pickEncoding(r.Header.Get("Accept-Encoding")) {
	case "br":
		h.Set("Content-Encoding", "br")
		bw := brotli.NewWriterOptions(w, brotli.WriterOptions{Quality: level, LGWin: lgwin})
		s.w, s.enc = bw, bw
	case "gzip":
		h.Set("Content-Encoding", "gzip")
		gw := gzip.NewWriter(w)
		s.w, s.enc = gw, gw
	}
	return s, s.rc.Flush()
}

func (s *eventStream) write(ev []byte) error {
	if _, err := s.w.Write(ev); err != nil {
		return err
	}
	if s.enc != nil {
		if err := s.enc.Flush(); err != nil {
			return err
		}
	}
	return s.rc.Flush()
}

// close ends the compressed stream cleanly (the page reconnects).
func (s *eventStream) close() {
	if s.enc != nil {
		_ = s.enc.Close()
		_ = s.rc.Flush()
	}
}

// pickEncoding prefers brotli, then gzip, among what the browser accepts.
func pickEncoding(accept string) string {
	for _, enc := range []string{"br", "gzip"} {
		if acceptsEncoding(accept, enc) {
			return enc
		}
	}
	return ""
}

// acceptsEncoding reads an Accept-Encoding header: enc is listed without q=0.
func acceptsEncoding(accept, enc string) bool {
	for part := range strings.SplitSeq(accept, ",") {
		name, params, _ := strings.Cut(part, ";")
		if !strings.EqualFold(strings.TrimSpace(name), enc) {
			continue
		}
		if v, found := strings.CutPrefix(strings.ReplaceAll(params, " ", ""), "q="); found {
			q, err := strconv.ParseFloat(v, 64)
			return err == nil && q > 0
		}
		return true
	}
	return false
}
