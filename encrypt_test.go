package bangdb

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

var testKey = []byte("0123456789abcdef0123456789abcdef")

func TestEncryption(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "enc.db")
	db, err := OpenWithOptions(path, &Options{EncryptionKey: testKey})
	if err != nil {
		t.Fatal(err)
	}
	exec(t, db, "CREATE TABLE secret (id INTEGER PRIMARY KEY, v TEXT); CREATE INDEX secret_v ON secret (v)")
	tx, _ := db.Begin()
	for i := 0; i < 2000; i++ {
		tx.Exec("INSERT INTO secret (v) VALUES (?)", "PLAINTEXT-MARKER-"+strings.Repeat("z", i%50))
	}
	tx.Commit()
	// Uncheckpointed commits sit in the WAL: it must not leak either.
	exec(t, db, "INSERT INTO secret (v) VALUES ('PLAINTEXT-MARKER-in-wal')")
	for _, f := range []string{path, path + "-wal"} {
		b, _ := os.ReadFile(f)
		if len(b) == 0 && f == path {
			t.Fatalf("%s is empty", f)
		}
		if bytes.Contains(b, []byte("PLAINTEXT-MARKER")) || bytes.Contains(b, []byte("secret_v")) {
			t.Errorf("%s contains plaintext", f)
		}
	}
	// Crash recovery: open a copy of the files as they are now (with an
	// uncheckpointed WAL).
	copyFile(t, path, filepath.Join(dir, "crash.db"))
	copyFile(t, path+"-wal", filepath.Join(dir, "crash.db-wal"))
	if err := db.Backup(filepath.Join(dir, "backup.db")); err != nil {
		t.Fatal(err)
	}
	if err := db.BackupWithKey(context.Background(), filepath.Join(dir, "plain.db"), nil); err != nil {
		t.Fatal(err)
	}
	newKey := bytes.Repeat([]byte{7}, 32)
	if err := db.BackupWithKey(context.Background(), filepath.Join(dir, "rekeyed.db"), newKey); err != nil {
		t.Fatal(err)
	}
	if err := db.Vacuum(context.Background()); err != nil {
		t.Fatal(err)
	}
	expect(t, db, "SELECT count(*) FROM secret", "2001")
	db.Close()

	for _, c := range []struct {
		file string
		key  []byte
		err  error
	}{
		{"enc.db", testKey, nil},
		{"enc.db", nil, ErrEncrypted},
		{"enc.db", bytes.Repeat([]byte{1}, 32), ErrWrongKey},
		{"crash.db", testKey, nil},
		{"backup.db", testKey, nil},
		{"backup.db", nil, ErrEncrypted},
		{"plain.db", nil, nil},
		{"plain.db", testKey, ErrNotEncrypted},
		{"rekeyed.db", newKey, nil},
		{"rekeyed.db", testKey, ErrWrongKey},
	} {
		db, err := OpenWithOptions(filepath.Join(dir, c.file), &Options{EncryptionKey: c.key})
		if c.err != nil {
			if !errors.Is(err, c.err) {
				t.Errorf("%s with key %v: got %v, want %v", c.file, c.key != nil, err, c.err)
			}
			if err == nil {
				db.Close()
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: %v", c.file, err)
			continue
		}
		expect(t, db, "SELECT count(*), max(v) FROM secret WHERE v LIKE 'PLAINTEXT%'", "2001|PLAINTEXT-MARKER-zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz")
		checkIntegrity(t, db)
		db.Close()
	}
	if b, _ := os.ReadFile(filepath.Join(dir, "plain.db")); !bytes.Contains(b, []byte("PLAINTEXT-MARKER")) {
		t.Error("the plaintext backup is not plaintext")
	}
	if _, err := OpenWithOptions(filepath.Join(dir, "short.db"), &Options{EncryptionKey: []byte("short")}); err == nil {
		t.Error("a 5-byte key was accepted")
	}
}

func copyFile(t *testing.T, from, to string) {
	t.Helper()
	in, err := os.Open(from)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		t.Fatal(err)
	}
}
