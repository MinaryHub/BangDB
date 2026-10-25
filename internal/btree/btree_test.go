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

func TestReverseCursor(t *testing.T) {
	p := pager.OpenMemory()
	v := p.NewView()
	root, _ := Create(v)
	tr := Open(v, root)
	rng := rand.New(rand.NewSource(5))
	present := map[int]bool{}
	for i := 0; i < 20000; i++ {
		k := rng.Intn(8000) * 2 // even keys only
		tr.Put(key(k), bytes.Repeat([]byte{1}, rng.Intn(900)))
		present[k] = true
	}
	for k := range present { // delete many to create empty leaves
		if rng.Intn(3) == 0 {
			tr.Delete(key(k))
			delete(present, k)
		}
	}
	var all []int
	for k := range present {
		all = append(all, k)
	}
	sort.Ints(all)
	check := func(seek []byte, want []int) {
		t.Helper()
		c := tr.SeekReverse(seek)
		for i := len(want) - 1; i >= 0; i-- {
			if !c.Valid() || int(binary.BigEndian.Uint64(c.Key())) != want[i] {
				t.Fatalf("seek %x: expected %d", seek, want[i])
			}
			c.Prev()
		}
		if c.Valid() {
			t.Fatalf("seek %x: extra entries", seek)
		}
	}
	check(nil, all)
	for _, probe := range []int{-1, 0, 1, 777, 778, 9000, 15998, 16001} {
		var want []int
		for _, k := range all {
			if k <= probe {
				want = append(want, k)
			}
		}
		pk := key(0)
		if probe >= 0 {
			pk = key(probe)
		} else {
			pk = []byte{}
		}
		check(pk, want)
	}
}

func checkTree(t *testing.T, v *pager.View, root uint32) int {
	t.Helper()
	r := Check(v, root, func(uint32) {})
	if len(r.Problems) > 0 {
		t.Fatalf("check: %v", r.Problems)
	}
	return r.Entries
}

func countPages(t *testing.T, v *pager.View, root uint32) int {
	t.Helper()
	n := 0
	Check(v, root, func(uint32) { n++ })
	return n
}

func TestDeleteRebalances(t *testing.T) {
	p := pager.OpenMemory()
	v := p.NewView()
	root, _ := Create(v)
	tr := Open(v, root)
	rng := rand.New(rand.NewSource(7))
	const n = 30000
	for _, i := range rng.Perm(n) {
		if err := tr.Put([]byte(fmt.Sprintf("key-%08d-%s", i, bytes.Repeat([]byte("x"), i%40))), key(i)); err != nil {
			t.Fatal(err)
		}
	}
	full := countPages(t, v, root)
	// Delete 98% in random order, checking the structure along the way.
	order := rng.Perm(n)
	for j, i := range order[:n*98/100] {
		ok, err := tr.Delete([]byte(fmt.Sprintf("key-%08d-%s", i, bytes.Repeat([]byte("x"), i%40))))
		if err != nil || !ok {
			t.Fatalf("delete %d: %v %v", i, ok, err)
		}
		if j%5000 == 0 {
			checkTree(t, v, root)
		}
	}
	if got := checkTree(t, v, root); got != n-n*98/100 {
		t.Fatalf("entries = %d", got)
	}
	left := countPages(t, v, root)
	if left*10 > full {
		t.Fatalf("pages after deleting 98%%: %d of %d (no rebalancing?)", left, full)
	}
	for _, i := range order[n*98/100:] {
		if _, err := tr.Delete([]byte(fmt.Sprintf("key-%08d-%s", i, bytes.Repeat([]byte("x"), i%40)))); err != nil {
			t.Fatal(err)
		}
	}
	if got := checkTree(t, v, root); got != 0 || countPages(t, v, root) != 1 {
		t.Fatalf("empty tree: %d entries, %d pages", got, countPages(t, v, root))
	}
	t.Logf("pages: %d full, %d after 98%% deleted", full, left)
}

func TestBuilder(t *testing.T) {
	for _, n := range []int{0, 1, 2, 100, 5000, 40000} {
		p := pager.OpenMemory()
		v := p.NewView()
		b := NewBuilder(v)
		for i := 0; i < n; i++ {
			val := key(i)
			if i%997 == 0 {
				val = bytes.Repeat([]byte{byte(i)}, 5000) // overflow
			}
			if err := b.Add([]byte(fmt.Sprintf("%08d%s", i, bytes.Repeat([]byte("k"), i%300))), val); err != nil {
				t.Fatal(err)
			}
		}
		root, err := b.Finish()
		if err != nil {
			t.Fatal(err)
		}
		if got := checkTree(t, v, root); got != n {
			t.Fatalf("n=%d: %d entries", n, got)
		}
		tr := Open(v, root)
		for i := 0; i < n; i += 37 {
			val, ok, err := tr.Get([]byte(fmt.Sprintf("%08d%s", i, bytes.Repeat([]byte("k"), i%300))))
			if err != nil || !ok || (i%997 != 0 && !bytes.Equal(val, key(i))) || (i%997 == 0 && len(val) != 5000) {
				t.Fatalf("n=%d get %d: %v %v", n, i, ok, err)
			}
		}
		// The built tree accepts further changes.
		if err := tr.Put([]byte("zzz"), []byte("x")); err != nil {
			t.Fatal(err)
		}
		checkTree(t, v, root)
	}
	b := NewBuilder(pager.OpenMemory().NewView())
	b.Add([]byte("b"), nil)
	if err := b.Add([]byte("a"), nil); err == nil {
		t.Fatal("out of order keys accepted")
	}
}
