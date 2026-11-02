package engine

import (
	"bufio"
	"bytes"
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/knoxxr/bangdb/internal/pager"
	"github.com/knoxxr/bangdb/internal/store"
	"github.com/knoxxr/bangdb/internal/value"
)

// Persistent memory tables.
//
// A memory table created with USING memory WITH (persist = true) keeps its
// rows across clean restarts: Close writes the rows of such tables to
// <database>-mem, and Open loads them back if the database has not changed
// since (same identity and commit sequence; otherwise, e.g. after a crash
// and recovery of later commits, the file is stale and ignored). A crash
// loses the rows, as for any memory table. The file is encrypted (AES-GCM)
// when the database is.
//
//	"BMEM1" dbid(16) seq(8) flags(1) payload crc32(4)
//	payload: ntables(uvarint), per table: name, nrows(uvarint),
//	         per row: rowid(varint) len(uvarint) record
//	         (with flags&1: nonce(12) + AES-GCM(payload))

const memSnapMagic = "BMEM1"

// persistent reports whether t is a memory table to be saved at Close.
func (t *Table) persistent() bool {
	if !t.kind.Volatile() {
		return false
	}
	switch strings.ToLower(t.StoreOptions["persist"]) {
	case "1", "true", "on", "yes":
		return true
	}
	return false
}

// checkVolatileOptions validates the options of a volatile kind: only
// persist is understood.
func checkVolatileOptions(t *Table) error {
	for k, v := range t.StoreOptions {
		if k != "persist" {
			return fmt.Errorf("bangdb: storage kind %s takes only the option persist", kindName(t.Store))
		}
		switch strings.ToLower(v) {
		case "1", "true", "on", "yes", "0", "false", "off", "no":
		default:
			return fmt.Errorf("bangdb: persist must be true or false, not %q", v)
		}
	}
	return nil
}

func (db *DB) memSnapPath() string { return db.path + "-mem" }

func (db *DB) memSnapCipher() (cipher.AEAD, error) {
	k := sha256.Sum256(append([]byte("bangdb memory snapshot\x00"), db.key...))
	b, err := aes.NewCipher(k[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(b)
}

// saveMemoryTables writes the persistent memory tables (called by Close
// with the write lock held, so the data cannot change).
func (db *DB) saveMemoryTables() error {
	if db.path == "" {
		return nil
	}
	path := db.memSnapPath()
	if err := db.p.FlushAll(); err != nil {
		return err
	}
	snap, err := db.p.Snapshot()
	if err != nil {
		return err
	}
	defer snap.Release()
	cat := snap.Meta().(*Catalog)
	var names []string
	for _, t := range cat.tables {
		if t.persistent() {
			names = append(names, t.Name)
		}
	}
	if len(names) == 0 {
		os.Remove(path) // nothing to keep; drop an old file
		return nil
	}
	sort.Strings(names)
	x := &execCtx{v: snap.View(), cat: cat, ctx: context.Background()}
	payload := binary.AppendUvarint(nil, uint64(len(names)))
	for _, name := range names {
		t := cat.table(name)
		payload = binary.AppendUvarint(payload, uint64(len(t.Name)))
		payload = append(payload, t.Name...)
		var rows []byte
		n := 0
		err := x.rows(t).Scan(store.MinRowid, store.MaxRowid, false, func(rowid int64, row []any) (bool, error) {
			rec := value.EncodeRow(row)
			rows = binary.AppendVarint(rows, rowid)
			rows = binary.AppendUvarint(rows, uint64(len(rec)))
			rows = append(rows, rec...)
			n++
			return true, nil
		})
		if err != nil {
			return err
		}
		payload = binary.AppendUvarint(payload, uint64(n))
		payload = append(payload, rows...)
	}
	h := snap.Header()
	out := append([]byte(memSnapMagic), h.DBID[:]...)
	out = binary.BigEndian.AppendUint64(out, snap.Seq())
	if db.key != nil {
		gcm, err := db.memSnapCipher()
		if err != nil {
			return err
		}
		nonce := make([]byte, gcm.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return err
		}
		out = append(out, 1)
		out = append(out, gcm.Seal(nonce, nonce, payload, out[:len(memSnapMagic)+24])...)
	} else {
		out = append(out, 0)
		out = append(out, payload...)
	}
	out = binary.BigEndian.AppendUint32(out, crc32.ChecksumIEEE(out))
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	w := bufio.NewWriter(f)
	w.Write(out)
	if err := w.Flush(); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := renameFile(tmp, path); err != nil {
		return err
	}
	return pager.SyncDir(path)
}

var errStaleMemSnap = errors.New("stale")

// loadMemoryTables restores the persistent memory tables saved by the last
// Close, if the database has not changed since, and removes the file.
func (db *DB) loadMemoryTables() error {
	if db.path == "" {
		return nil
	}
	path := db.memSnapPath()
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	err = db.restoreMemory(data)
	if errors.Is(err, errStaleMemSnap) {
		err = nil
	}
	if err == nil {
		os.Remove(path)
	}
	return err
}

func (db *DB) restoreMemory(data []byte) error {
	head := len(memSnapMagic) + 16 + 8 + 1
	if len(data) < head+4 || string(data[:len(memSnapMagic)]) != memSnapMagic {
		return fmt.Errorf("bangdb: %s is not a memory table snapshot", db.memSnapPath())
	}
	body, sum := data[:len(data)-4], binary.BigEndian.Uint32(data[len(data)-4:])
	if crc32.ChecksumIEEE(body) != sum {
		return errStaleMemSnap // torn write: ignore it
	}
	var dbid [16]byte
	copy(dbid[:], data[len(memSnapMagic):])
	seq := binary.BigEndian.Uint64(data[len(memSnapMagic)+16:])
	h := db.p.Header()
	if dbid != h.DBID || seq != h.Seq {
		return errStaleMemSnap
	}
	payload := body[head:]
	if body[head-1] == 1 {
		if db.key == nil {
			return errStaleMemSnap
		}
		gcm, err := db.memSnapCipher()
		if err != nil {
			return err
		}
		if len(payload) < gcm.NonceSize() {
			return errStaleMemSnap
		}
		if payload, err = gcm.Open(nil, payload[:gcm.NonceSize()], payload[gcm.NonceSize():], body[:head-1]); err != nil {
			return errStaleMemSnap
		}
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer func() {
		if !tx.done {
			tx.Rollback()
		}
	}()
	x := &execCtx{v: tx.v, cat: tx.cat, tx: tx, ctx: context.Background()}
	r := bytes.NewReader(payload)
	uv := func() uint64 { v, _ := binary.ReadUvarint(r); return v }
	ntables := uv()
	for i := uint64(0); i < ntables; i++ {
		name := make([]byte, uv())
		if _, err := io.ReadFull(r, name); err != nil {
			return errStaleMemSnap
		}
		t := x.cat.table(string(name))
		nrows := uv()
		for j := uint64(0); j < nrows; j++ {
			rowid, err := binary.ReadVarint(r)
			if err != nil {
				return errStaleMemSnap
			}
			rec := make([]byte, uv())
			if _, err := io.ReadFull(r, rec); err != nil {
				return errStaleMemSnap
			}
			if t == nil || !t.persistent() {
				continue
			}
			row, err := value.DecodeRow(rec)
			if err != nil {
				return errStaleMemSnap
			}
			if err := x.rows(t).Put(rowid, row); err != nil {
				return err
			}
			full := padRow(t, row)
			for _, ix := range t.indexes {
				k, err := indexEntry(ix, full, rowid)
				if err != nil {
					return err
				}
				if k == nil {
					continue
				}
				if err := x.index(ix).Put(k); err != nil {
					return err
				}
			}
		}
	}
	// The rows live only in object slots: publish them without a commit,
	// so that the commit sequence (which replicas follow) stays as it was.
	if tx.v.Dirty() == 0 {
		if objs := tx.v.ChangedObjects(); objs != nil {
			db.p.SetObjects(objs)
		}
		return tx.Rollback()
	}
	return tx.Commit()
}
