package memtree

import (
	"cmp"
	"math/rand"
	"reflect"
	"sort"
	"testing"

	"github.com/minaryhub/bangdb/internal/pager"
)

func keys(it *Iter[int, int]) []int {
	var out []int
	for ; it.Valid(); it.Next() {
		out = append(out, it.Key())
	}
	return out
}

func sortedKeys(m map[int]int) []int {
	var out []int
	for k := range m {
		out = append(out, k)
	}
	sort.Ints(out)
	return out
}

// check verifies the B+tree invariants.
func check[K, V any](t *testing.T, tr Tree[K, V]) {
	t.Helper()
	count := 0
	depth := -1
	var walk func(n *node[K, V], d int, lo, hi *K)
	walk = func(n *node[K, V], d int, lo, hi *K) {
		for i := 1; i < len(n.keys); i++ {
			if tr.cmp(n.keys[i-1], n.keys[i]) >= 0 {
				t.Fatalf("keys out of order")
			}
		}
		for _, k := range n.keys {
			if lo != nil && tr.cmp(k, *lo) < 0 || hi != nil && tr.cmp(k, *hi) >= 0 {
				t.Fatalf("key outside parent range")
			}
		}
		if n != tr.root && n.entries() < minEntries {
			t.Fatalf("underfull node: %d entries", n.entries())
		}
		if n.entries() > maxEntries {
			t.Fatalf("overfull node")
		}
		if n.leaf() {
			if depth == -1 {
				depth = d
			} else if depth != d {
				t.Fatalf("leaves at different depths")
			}
			count += len(n.keys)
			return
		}
		if len(n.keys) != len(n.kids)-1 {
			t.Fatalf("internal node with %d keys and %d children", len(n.keys), len(n.kids))
		}
		for i, c := range n.kids {
			l, h := lo, hi
			if i > 0 {
				l = &n.keys[i-1]
			}
			if i < len(n.keys) {
				h = &n.keys[i]
			}
			walk(c, d+1, l, h)
		}
	}
	if tr.root != nil {
		walk(tr.root, 0, nil, nil)
	}
	if count != tr.size {
		t.Fatalf("size %d, counted %d", tr.size, count)
	}
}

func TestRandomOpsAndSnapshots(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	tr := New[int, int](cmp.Compare[int])
	ref := map[int]int{}
	type snap struct {
		tree Tree[int, int]
		ref  map[int]int
	}
	var snaps []snap
	tok := new(pager.Token)
	for i := 0; i < 60000; i++ {
		if i%2000 == 0 {
			// Publish: keep this state and continue under a new token.
			cp := map[int]int{}
			for k, v := range ref {
				cp[k] = v
			}
			snaps = append(snaps, snap{tr, cp})
			tok = new(pager.Token)
		}
		k := rng.Intn(5000)
		if rng.Intn(3) == 0 {
			var ok bool
			tr, ok = tr.Delete(tok, k)
			if _, want := ref[k]; ok != want {
				t.Fatalf("Delete(%d) = %v", k, ok)
			}
			delete(ref, k)
		} else {
			tr = tr.Put(tok, k, i)
			ref[k] = i
		}
		if i%5000 == 0 {
			check(t, tr)
		}
	}
	check(t, tr)
	if got := keys(tr.First()); !reflect.DeepEqual(got, sortedKeys(ref)) {
		t.Fatal("iteration differs from model")
	}
	for k, v := range ref {
		if got, ok := tr.Get(k); !ok || got != v {
			t.Fatalf("Get(%d) = %d %v, want %d", k, got, ok, v)
		}
	}
	// Earlier states are unaffected by later changes.
	for i, s := range snaps {
		check(t, s.tree)
		if got := keys(s.tree.First()); !reflect.DeepEqual(got, sortedKeys(s.ref)) {
			t.Fatalf("snapshot %d changed", i)
		}
		for k, v := range s.ref {
			if got, _ := s.tree.Get(k); got != v {
				t.Fatalf("snapshot %d: key %d = %d, want %d", i, k, got, v)
			}
		}
	}
	// Delete everything.
	for _, k := range sortedKeys(ref) {
		tr, _ = tr.Delete(tok, k)
	}
	if tr.Len() != 0 || tr.root != nil {
		t.Fatal("tree not empty")
	}
}

func TestSeek(t *testing.T) {
	tok := new(pager.Token)
	tr := New[int, int](cmp.Compare[int])
	for i := 0; i < 1000; i += 2 {
		tr = tr.Put(tok, i, i)
	}
	if got := keys(tr.Seek(501)); len(got) != 249 || got[0] != 502 {
		t.Fatalf("Seek(501): %d keys from %v", len(got), got[:1])
	}
	if got := keys(tr.Seek(500)); got[0] != 500 {
		t.Fatal("Seek(500)")
	}
	if got := keys(tr.SeekReverse(501)); len(got) != 251 || got[0] != 500 || got[250] != 0 {
		t.Fatalf("SeekReverse(501): %d keys", len(got))
	}
	if got := keys(tr.SeekReverse(-1)); len(got) != 0 {
		t.Fatal("SeekReverse below the first key")
	}
	if got := keys(tr.Seek(1000)); len(got) != 0 {
		t.Fatal("Seek past the last key")
	}
	if got := keys(tr.LastIter()); len(got) != 500 || got[0] != 998 {
		t.Fatal("LastIter")
	}
	if k, ok := tr.Last(); !ok || k != 998 {
		t.Fatal("Last")
	}
	empty := New[int, int](cmp.Compare[int])
	if empty.Seek(0).Valid() || empty.LastIter().Valid() || empty.First().Valid() {
		t.Fatal("iterator on empty tree")
	}
}
