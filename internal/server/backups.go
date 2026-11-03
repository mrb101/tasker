package server

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

// Backups takes consistent snapshots of the database on a schedule and keeps
// the newest few. A snapshot is SQLite's VACUUM INTO: a complete, compact copy
// taken while the server keeps running.
type Backups struct {
	Dir   string        // where snapshots go
	Every time.Duration // 0 turns the schedule off (manual backups still work)
	Keep  int           // how many snapshots to keep

	mu   sync.Mutex
	last error
}

// BackupFile is one snapshot on disk.
type BackupFile struct {
	Name string    `json:"name"`
	Size int64     `json:"size"`
	Time time.Time `json:"time"`
}

var backupName = regexp.MustCompile(`^tasker-\d{8}-\d{6}\.db$`)

// List returns the snapshots, newest first.
func (b *Backups) List() ([]BackupFile, error) {
	entries, err := os.ReadDir(b.Dir)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []BackupFile
	for _, e := range entries {
		if e.IsDir() || !backupName.MatchString(e.Name()) {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		t, _ := time.ParseInLocation("tasker-20060102-150405.db", e.Name(), time.UTC)
		out = append(out, BackupFile{Name: e.Name(), Size: info.Size(), Time: t})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name > out[j].Name })
	return out, nil
}

// BackupNow takes a snapshot and removes the ones beyond Keep.
func (s *Server) BackupNow() (BackupFile, error) {
	b := s.backups
	if b == nil || b.Dir == "" {
		return BackupFile{}, fmt.Errorf("backups are not configured")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if err := os.MkdirAll(b.Dir, 0o750); err != nil {
		return BackupFile{}, err
	}
	now := time.Now().UTC()
	name := now.Format("tasker-20060102-150405.db")
	final := filepath.Join(b.Dir, name)
	tmp := final + ".partial"
	os.Remove(tmp)
	if err := s.store.Backup(tmp); err != nil {
		os.Remove(tmp)
		b.last = err
		return BackupFile{}, err
	}
	if err := os.Rename(tmp, final); err != nil {
		b.last = err
		return BackupFile{}, err
	}
	b.last = nil
	list, err := b.List()
	if err == nil && b.Keep > 0 && len(list) > b.Keep {
		for _, old := range list[b.Keep:] {
			os.Remove(filepath.Join(b.Dir, old.Name))
		}
	}
	info, _ := os.Stat(final)
	return BackupFile{Name: name, Size: info.Size(), Time: now}, nil
}

// StartBackups runs the schedule until ctx ends: a snapshot at startup when
// the newest is older than Every, then one every Every.
func (s *Server) StartBackups(ctx context.Context, b *Backups) {
	s.backups = b
	if b == nil || b.Dir == "" || b.Every <= 0 {
		return
	}
	due := func() bool {
		list, err := b.List()
		return err != nil || len(list) == 0 || time.Since(list[0].Time) >= b.Every
	}
	take := func() {
		if f, err := s.BackupNow(); err != nil {
			log.Printf("backup failed: %v", err)
		} else {
			log.Printf("backup %s (%d bytes)", f.Name, f.Size)
		}
	}
	go func() {
		if due() {
			take()
		}
		// Check every few minutes rather than sleeping a full period, so a
		// restart or a sleeping laptop does not push the next backup out.
		tick := time.NewTicker(min(b.Every, 10*time.Minute))
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				if due() {
					take()
				}
			}
		}
	}()
	log.Printf("backups every %s into %s, keeping %d", b.Every, b.Dir, b.Keep)
}

// backupStatus is what the settings page and API show.
func (s *Server) backupStatus() map[string]any {
	b := s.backups
	if b == nil || b.Dir == "" {
		return map[string]any{"enabled": false}
	}
	list, err := b.List()
	b.mu.Lock()
	last := b.last
	b.mu.Unlock()
	st := map[string]any{"enabled": true, "dir": b.Dir, "every": b.Every.String(), "keep": b.Keep, "backups": list}
	if b.Every <= 0 {
		st["every"] = "off (manual only)"
	}
	if err != nil {
		st["error"] = err.Error()
	} else if last != nil {
		st["error"] = last.Error()
	}
	return st
}

func (s *Server) routesBackups(mux *http.ServeMux) {
	mux.HandleFunc("GET /api/backups", h(func(r *http.Request) (any, error) { return s.backupStatus(), nil }))
	mux.HandleFunc("POST /api/backups", h(func(r *http.Request) (any, error) { return s.BackupNow() }))
	mux.HandleFunc("GET /api/backups/{name}", func(w http.ResponseWriter, r *http.Request) {
		name := r.PathValue("name")
		if s.backups == nil || !backupName.MatchString(name) {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/vnd.sqlite3")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
		http.ServeFile(w, r, filepath.Join(s.backups.Dir, name))
	})
}
