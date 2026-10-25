package btree

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"math/rand"
	"path/filepath"
	"sort"
	"testing"

	"github.com/knoxxr/bangdb/internal/pager"
)

func key(i int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, uint64(i))
	return b
}

func TestRandomOps(t *testing.T) {
	p := pager.OpenMemory()
	v := p.NewView()
	root, err := Create(v)
	if err != nil {
		t.Fatal(err)
	}
	tr := Open(v, root)
	ref := map[string][]byte{}
	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 20000; i++ {
		k := []byte(fmt.Sprintf("k%06d", rng.Intn(5000)))
		switch rng.Intn(4) {
		case 0:
			if _, err := tr.Delete(k); err != nil {
				t.Fatal(err)
			}
			delete(ref, string(k))
		default:
			val := bytes.Repeat([]byte{byte(i)}, rng.Intn(3000))
			if err := tr.Put(k, val); err != nil {
				t.Fatal(err)
			}
			ref[string(k)] = val
		}
	}
	keys := make([]string, 0, len(ref))
	for k := range ref {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	c := tr.Seek(nil)
	for _, k := range keys {
		if !c.Valid() || string(c.Key()) != k {
			t.Fatalf("cursor mismatch at %s", k)
		}
		val, err := c.Value()
		if err != nil || !bytes.Equal(val, ref[k]) {
			t.Fatalf("value mismatch at %s: %v", k, err)
		}
		c.Next()
	}
	if c.Valid() {
		t.Fatal("cursor has extra entries")
	}
	last, ok, _ := tr.Last()
	if !ok || string(last) != keys[len(keys)-1] {
		t.Fatalf("Last = %q", last)
	}
	if err := p.Commit(v); err != nil {
		t.Fatal(err)
	}
	v2 := p.NewView()
	if err := Drop(v2, root); err != nil {
		t.Fatal(err)
	}
}

func TestPersistAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.db")
	p, err := pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	v := p.NewView()
	root, _ := Create(v)
	v.SetMeta(0, root)
	tr := Open(v, root)
	for i := 0; i < 10000; i++ {
		if err := tr.Put(key(i), key(i*2)); err != nil {
			t.Fatal(err)
		}
	}
	if err := p.Commit(v); err != nil {
		t.Fatal(err)
	}
	p.Close()

	p, err = pager.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()
	v = p.NewView()
	tr = Open(v, v.Meta(0))
	n := 0
	for c := tr.Seek(key(5000)); c.Valid(); c.Next() {
		n++
	}
	if n != 5000 {
		t.Fatalf("got %d entries", n)
	}
	val, ok, err := tr.Get(key(1234))
	if err != nil || !ok || !bytes.Equal(val, key(2468)) {
		t.Fatal("Get failed", err)
	}
}
