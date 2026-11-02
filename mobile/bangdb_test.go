package bangdb

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
)

func TestMobileAPI(t *testing.T) {
	dir := t.TempDir()
	o := NewOptions()
	o.EncryptionKey = bytes.Repeat([]byte{9}, 32)
	o.TempDir = dir
	o.CacheSize = 8 << 20
	path := filepath.Join(dir, "app.db")
	db, err := OpenWithOptions(path, o)
	if err != nil {
		t.Fatal(err)
	}
	n, err := db.Migrate(NewMigrations().
		Add(1, "notes", "CREATE TABLE note (id INTEGER PRIMARY KEY, body TEXT NOT NULL UNIQUE, price DECIMAL(10,2), pic BLOB, done BOOLEAN DEFAULT 0)").
		Add(2, "index", "CREATE INDEX note_done ON note (done)"))
	if err != nil || n != 2 {
		t.Fatalf("migrate: %d %v", n, err)
	}
	if v, _ := db.SchemaVersion(); v != 2 {
		t.Fatalf("schema version %d", v)
	}
	args, err := NewArgs().AddString("hello").AddBytes([]byte{1, 2, 3}).AddBool(true).AddDecimal("12.50")
	if err != nil {
		t.Fatal(err)
	}
	r, err := db.Exec("INSERT INTO note (body, pic, done, price) VALUES (?, ?, ?, ?)", args)
	if err != nil || r.RowsAffected != 1 || r.LastInsertID != 1 {
		t.Fatalf("insert: %+v %v", r, err)
	}
	_, err = db.Exec("INSERT INTO note (body) VALUES (?)", NewArgs().AddString("hello"))
	if err == nil || !IsConstraintError(err.Error()) {
		t.Fatalf("duplicate: %v", err)
	}

	tx, err := db.Begin()
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		if _, err := tx.Exec("INSERT INTO note (body, price) VALUES (?, ?)", NewArgs().AddString("n"+strings.Repeat("x", i)).AddDouble(0.1)); err != nil {
			t.Fatal(err)
		}
	}
	if js, err := tx.QueryJSON("SELECT count(*) AS n FROM note", nil); err != nil || js != `{"columns":["n"],"rows":[[4]]}` {
		t.Fatalf("in tx: %s %v", js, err)
	}
	if err := tx.Commit(); err != nil {
		t.Fatal(err)
	}

	rows, err := db.Query("SELECT id, body, price, pic, done, NULL AS nothing FROM note WHERE id = ?", NewArgs().AddLong(1))
	if err != nil {
		t.Fatal(err)
	}
	if rows.ColumnCount() != 6 || rows.ColumnName(1) != "body" || rows.ColumnIndex("price") != 2 {
		t.Fatal("columns")
	}
	if !rows.Next() {
		t.Fatal("no row", rows.Err())
	}
	id, _ := rows.GetLong(0)
	done, _ := rows.GetBool(4)
	if id != 1 || rows.GetString(1) != "hello" || rows.GetString(2) != "12.50" || !bytes.Equal(rows.GetBytes(3), []byte{1, 2, 3}) || !done ||
		!rows.IsNull(5) || rows.ColumnType(2) != TypeDecimal || rows.ColumnType(3) != TypeBytes || rows.ColumnType(5) != TypeNull {
		t.Fatalf("values: %v", rows.cur)
	}
	if rows.Next() {
		t.Fatal("extra row")
	}
	js, err := db.QueryJSON("SELECT body, price, pic FROM note WHERE id = 1", nil)
	if err != nil || js != `{"columns":["body","price","pic"],"rows":[["hello",12.50,"AQID"]]}` {
		t.Fatalf("json: %s %v", js, err)
	}

	// The file is locked while open; backups verify; reopening keeps data.
	if _, err := Open(path); err == nil || !IsLockedError(err.Error()) {
		t.Fatalf("second open: %v", err)
	}
	bk := filepath.Join(dir, "backup.db")
	if err := db.Backup(bk); err != nil {
		t.Fatal(err)
	}
	if err := db.VerifyBackup(bk); err != nil {
		t.Fatal(err)
	}
	if s, err := db.IntegrityCheck(); err != nil || s != "ok" {
		t.Fatalf("integrity: %s %v", s, err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path); err == nil {
		t.Fatal("opened an encrypted file without the key")
	}
	db, err = OpenWithOptions(path, o)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if n, err := db.Migrate(NewMigrations().Add(1, "notes", "x").Add(2, "index", "x")); err != nil || n != 0 {
		t.Fatalf("migrations re-applied: %d %v", n, err)
	}
	if js, _ := db.QueryJSON("SELECT count(*) AS n FROM note", nil); js != `{"columns":["n"],"rows":[[4]]}` {
		t.Fatalf("after reopen: %s", js)
	}
	if Version() == "" {
		t.Fatal("no version")
	}
}
