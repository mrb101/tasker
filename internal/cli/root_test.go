package cli

import (
	"testing"
	"time"
)

func TestBackupConfig(t *testing.T) {
	t.Setenv("TASKER_BACKUP_DIR", "")
	t.Setenv("TASKER_BACKUP_EVERY", "")
	t.Setenv("TASKER_BACKUP_KEEP", "")
	b, err := backupConfig("/data/tasker.db")
	if err != nil || b.Dir != "/data/backups" || b.Every != 24*time.Hour || b.Keep != 14 {
		t.Fatalf("defaults: %+v %v", b, err)
	}
	t.Setenv("TASKER_BACKUP_EVERY", "0")
	t.Setenv("TASKER_BACKUP_KEEP", "3")
	if b, err = backupConfig("x.db"); err != nil || b.Every != 0 || b.Keep != 3 {
		t.Fatalf("manual: %+v %v", b, err)
	}
	for _, bad := range [][2]string{{"daily", "3"}, {"6h", "0"}, {"-1h", "3"}} {
		t.Setenv("TASKER_BACKUP_EVERY", bad[0])
		t.Setenv("TASKER_BACKUP_KEEP", bad[1])
		if _, err := backupConfig("x.db"); err == nil {
			t.Fatalf("accepted %v", bad)
		}
	}
}
