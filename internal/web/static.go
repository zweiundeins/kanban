package web

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"slices"
	"strings"

	"github.com/andybalholm/brotli"
)

type asset struct {
	body, br, gz []byte
	ctype        string
}

// Static serves the embedded files under /s/<hash>/: the hash covers every
// file, so their URLs change with any of them and can be cached for a year.
// Text files are compressed once, at startup.
type Static struct {
	Prefix string
	files  map[string]*asset
}

func NewStatic(fsys fs.FS) (*Static, error) {
	s := &Static{files: map[string]*asset{}}
	h := sha256.New()
	var names []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		names = append(names, p)
		return nil
	})
	if err != nil {
		return nil, err
	}
	slices.Sort(names)
	for _, name := range names {
		body, err := fs.ReadFile(fsys, name)
		if err != nil {
			return nil, err
		}
		h.Write([]byte(name))
		h.Write(body)
		a := &asset{body: body, ctype: mime.TypeByExtension(path.Ext(name))}
		switch path.Ext(name) {
		case ".js":
			a.ctype = "text/javascript; charset=utf-8"
		case ".css":
			a.ctype = "text/css; charset=utf-8"
		case ".svg":
			a.ctype = "image/svg+xml"
		case ".txt", "":
			a.ctype = "text/plain; charset=utf-8"
		}
		if strings.HasPrefix(a.ctype, "text/") || strings.HasPrefix(a.ctype, "image/svg") {
			var br bytes.Buffer
			bw := brotli.NewWriterLevel(&br, brotli.BestCompression)
			bw.Write(body)
			bw.Close()
			var gz bytes.Buffer
			gw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
			gw.Write(body)
			gw.Close()
			if br.Len() < len(body) {
				a.br = br.Bytes()
			}
			if gz.Len() < len(body) {
				a.gz = gz.Bytes()
			}
		}
		s.files[name] = a
	}
	s.Prefix = "/s/" + hex.EncodeToString(h.Sum(nil))[:12]
	return s, nil
}

func accepts(r *http.Request, enc string) bool {
	return acceptsEncoding(r.Header.Get("Accept-Encoding"), enc)
}

// ServeHTTP serves /s/{hash}/{path...}.
func (s *Static) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	a := s.files[r.PathValue("path")]
	if a == nil {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", a.ctype)
	h.Set("Vary", "Accept-Encoding")
	h.Set("X-Content-Type-Options", "nosniff")
	if "/s/"+r.PathValue("hash") == s.Prefix {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache") // an old page asking for an old version gets today's file
	}
	body := a.body
	switch {
	case a.br != nil && accepts(r, "br"):
		h.Set("Content-Encoding", "br")
		body = a.br
	case a.gz != nil && accepts(r, "gzip"):
		h.Set("Content-Encoding", "gzip")
		body = a.gz
	}
	h.Set("Content-Length", itoa(int64(len(body))))
	if r.Method == http.MethodHead {
		return
	}
	io.Copy(w, bytes.NewReader(body))
}
