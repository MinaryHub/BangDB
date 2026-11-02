package bangdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// VerifyBackup opens a backup of this database (with its encryption key)
// and runs the integrity check; it returns an error describing any
// problem.
func (db *DB) VerifyBackup(ctx context.Context, path string) error {
	problems, err := db.e.VerifyBackup(ctx, path)
	if err != nil {
		return fmt.Errorf("bangdb: backup %s cannot be opened: %w", path, err)
	}
	if len(problems) > 0 {
		return fmt.Errorf("bangdb: backup %s is damaged: %s", path, strings.Join(problems, "; "))
	}
	return nil
}

// BackupSchedule configures StartBackups.
type BackupSchedule struct {
	Dir      string        // where backups go (created if needed)
	Interval time.Duration // time between backups (default 24h); the first runs at once
	Keep     int           // newest backups kept; older ones are deleted (0 = keep all)
	// NoVerify skips opening each backup to run the integrity check.
	NoVerify bool
	// OnBackup and OnError report each outcome (both optional).
	OnBackup func(path string, took time.Duration)
	OnError  func(err error)
}

// StartBackups takes a backup now and then every Interval into Dir, named
// <database>-YYYYMMDD-HHMMSS.db, verifies it, and deletes the oldest beyond
// Keep. A failed backup is deleted and reported to OnError. Stop ends the
// schedule (waiting for a running backup).
func (db *DB) StartBackups(s BackupSchedule) (stop func(), err error) {
	if s.Interval <= 0 {
		s.Interval = 24 * time.Hour
	}
	if err := os.MkdirAll(s.Dir, 0o755); err != nil {
		return nil, err
	}
	base := strings.TrimSuffix(filepath.Base(db.e.Path()), filepath.Ext(db.e.Path()))
	if base == "" || base == "." {
		base = "bangdb"
	}
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			db.scheduledBackup(ctx, s, base)
			select {
			case <-ctx.Done():
				return
			case <-time.After(s.Interval):
			}
		}
	}()
	return func() { cancel(); wg.Wait() }, nil
}

func (db *DB) scheduledBackup(ctx context.Context, s BackupSchedule, base string) {
	fail := func(err error) {
		if s.OnError != nil && ctx.Err() == nil {
			s.OnError(err)
		}
	}
	start := time.Now()
	path := filepath.Join(s.Dir, fmt.Sprintf("%s-%s.db", base, start.UTC().Format("20060102-150405")))
	if err := db.BackupContext(ctx, path); err != nil {
		fail(fmt.Errorf("bangdb: scheduled backup: %w", err))
		return
	}
	if !s.NoVerify {
		if err := db.VerifyBackup(ctx, path); err != nil {
			os.Remove(path)
			fail(err)
			return
		}
	}
	if s.OnBackup != nil {
		s.OnBackup(path, time.Since(start))
	}
	if s.Keep > 0 {
		old, _ := filepath.Glob(filepath.Join(s.Dir, base+"-*.db"))
		sort.Strings(old) // the timestamp sorts by time
		for len(old) > s.Keep {
			os.Remove(old[0])
			os.Remove(old[0] + "-wal")
			old = old[1:]
		}
	}
}
