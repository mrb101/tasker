package server

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"mime"
	"net/http"
	"path"
	"sort"
	"strconv"
	"strings"
)

// asset is a static file held in memory, with a gzipped copy for text types.
type asset struct {
	data  []byte
	gz    []byte
	ctype string
	etag  string
}

// assets serves the embedded static files. URLs carry ?v=<hash of all assets>,
// so browsers may cache them forever; the hash changes whenever a file does.
type assets struct {
	files   map[string]*asset
	version string
}

func loadAssets(fsys fs.FS) (*assets, error) {
	a := &assets{files: map[string]*asset{}}
	all := sha256.New()
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
	sort.Strings(names)
	for _, p := range names {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256(data)
		all.Write(sum[:])
		ct := mime.TypeByExtension(path.Ext(p))
		if ct == "" {
			ct = "application/octet-stream"
		}
		f := &asset{data: data, ctype: ct, etag: `"` + hex.EncodeToString(sum[:8]) + `"`}
		if strings.HasPrefix(ct, "text/") || strings.Contains(ct, "javascript") || strings.Contains(ct, "json") {
			var buf bytes.Buffer
			zw, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
			zw.Write(data)
			zw.Close()
			if buf.Len() < len(data) {
				f.gz = buf.Bytes()
			}
		}
		a.files[p] = f
	}
	a.version = hex.EncodeToString(all.Sum(nil)[:6])
	return a, nil
}

func (a *assets) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f, ok := a.files[strings.TrimPrefix(r.URL.Path, "/static/")]
	if !ok {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", f.ctype)
	h.Set("ETag", f.etag)
	h.Set("Vary", "Accept-Encoding")
	if r.URL.Query().Get("v") == a.version {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	if match := r.Header.Get("If-None-Match"); match != "" && strings.Contains(match, f.etag) {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	body := f.data
	if f.gz != nil && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		h.Set("Content-Encoding", "gzip")
		body = f.gz
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.Write(body)
}
