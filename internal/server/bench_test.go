package server

import (
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/mrb101/tasker/internal/store"
)

func BenchmarkPages(b *testing.B) {
	path := os.Getenv("BENCH_DB")
	if path == "" {
		b.Skip("BENCH_DB not set")
	}
	st, err := store.Open(path)
	if err != nil {
		b.Fatal(err)
	}
	srv, _ := New(st, "bench")
	h := srv.Handler()
	paths := []string{"/p/BIG", "/p/BIG/items"}
	if env := os.Getenv("BENCH_PATHS"); env != "" {
		paths = strings.Split(env, ",")
	}
	for _, p := range paths {
		b.Run(p, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
			}
		})
	}
}
