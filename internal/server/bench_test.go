package server

import (
	"net/http/httptest"
	"os"
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
	for _, p := range []string{"/p/BIG", "/p/BIG/items"} {
		b.Run(p, func(b *testing.B) {
			for i := 0; i < b.N; i++ {
				w := httptest.NewRecorder()
				h.ServeHTTP(w, httptest.NewRequest("GET", p, nil))
			}
		})
	}
}
