package minidb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"testing"
)

func b(s string) []byte { return []byte(s) }

func roundTrip(t *testing.T, n *node) *node {
	t.Helper()
	buf := make([]byte, PayloadSize)
	if err := n.encode(buf); err != nil {
		t.Fatal(err)
	}
	got, err := decodeNode(buf)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestLeafRoundTrip(t *testing.T) {
	want := &node{
		leaf: true,
		keys: [][]byte{b("apple"), b("banana"), b("cherry")},
		vals: [][]byte{b("red"), {}, b("dark red")},
		next: 42,
	}
	got := roundTrip(t, want)
	if !got.leaf || got.next != 42 || len(got.keys) != 3 {
		t.Fatalf("decoded wrong leaf: %+v", got)
	}
	for i := range want.keys {
		if !bytes.Equal(got.keys[i], want.keys[i]) || !bytes.Equal(got.vals[i], want.vals[i]) {
			t.Fatalf("entry %d: got %q=%q", i, got.keys[i], got.vals[i])
		}
	}
}

func TestInternalRoundTrip(t *testing.T) {
	want := &node{keys: [][]byte{b("g"), b("p")}, children: []uint64{7, 8, 9}}
	got := roundTrip(t, want)
	if got.leaf || !reflect.DeepEqual(got.children, want.children) {
		t.Fatalf("decoded wrong internal node: %+v", got)
	}
	if !bytes.Equal(got.keys[0], b("g")) || !bytes.Equal(got.keys[1], b("p")) {
		t.Fatalf("wrong keys: %q", got.keys)
	}
}

func TestRandomLeavesRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	for round := 0; round < 200; round++ {
		n := &node{leaf: true}
		for i := 0; i < rng.Intn(30); i++ {
			val := make([]byte, rng.Intn(100))
			rng.Read(val)
			n.setLeaf(b(fmt.Sprintf("key-%05d", rng.Intn(100000))), val)
		}
		got := roundTrip(t, n)
		if len(got.keys) != len(n.keys) {
			t.Fatalf("got %d keys, want %d", len(got.keys), len(n.keys))
		}
		for i := range n.keys {
			if !bytes.Equal(got.keys[i], n.keys[i]) || !bytes.Equal(got.vals[i], n.vals[i]) {
				t.Fatalf("round %d: entry %d changed", round, i)
			}
		}
	}
}

func TestSizeIsExact(t *testing.T) {
	n := &node{leaf: true, keys: [][]byte{b("abc")}, vals: [][]byte{b("hello")}}
	if want := 11 + 2 + 2 + 3 + 5; n.size() != want {
		t.Fatalf("leaf size %d, want %d", n.size(), want)
	}
	if err := n.encode(make([]byte, n.size())); err != nil {
		t.Fatalf("exact-size buffer rejected: %v", err)
	}
	if err := n.encode(make([]byte, n.size()-1)); err == nil {
		t.Fatal("buffer one byte too small was accepted")
	}

	in := &node{keys: [][]byte{b("abc")}, children: []uint64{1, 2}}
	if want := 11 + 2 + 3 + 8; in.size() != want {
		t.Fatalf("internal size %d, want %d", in.size(), want)
	}
}

func TestThreeLargestEntriesFitInOnePage(t *testing.T) {
	n := &node{leaf: true}
	for i := byte(0); i < 3; i++ {
		key := bytes.Repeat([]byte{'a' + i}, MaxKeySize)
		n.setLeaf(key, make([]byte, MaxValueSize))
	}
	if got := roundTrip(t, n); len(got.keys) != 3 {
		t.Fatalf("got %d keys", len(got.keys))
	}
}

func TestDecodedNodeDoesNotShareThePageBuffer(t *testing.T) {
	buf := make([]byte, PayloadSize)
	n := &node{leaf: true, keys: [][]byte{b("key")}, vals: [][]byte{b("value")}}
	if err := n.encode(buf); err != nil {
		t.Fatal(err)
	}
	got, err := decodeNode(buf)
	if err != nil {
		t.Fatal(err)
	}
	clear(buf)
	if string(got.keys[0]) != "key" || string(got.vals[0]) != "value" {
		t.Fatalf("decoded data changed: %q=%q", got.keys[0], got.vals[0])
	}
}

func TestDamagedNodesAreRejected(t *testing.T) {
	valid := func() []byte {
		buf := make([]byte, 64)
		n := &node{leaf: true, keys: [][]byte{b("a"), b("b")}, vals: [][]byte{b("1"), b("2")}}
		if err := n.encode(buf); err != nil {
			t.Fatal(err)
		}
		return buf
	}
	tests := map[string]func([]byte) []byte{
		"unknown type":   func(p []byte) []byte { p[0] = 9; return p },
		"too many keys":  func(p []byte) []byte { binary.LittleEndian.PutUint16(p[1:], 60000); return p },
		"key too long":   func(p []byte) []byte { binary.LittleEndian.PutUint16(p[11:], MaxKeySize+1); return p },
		"value too long": func(p []byte) []byte { binary.LittleEndian.PutUint16(p[13:], MaxValueSize+1); return p },
		"keys unsorted":  func(p []byte) []byte { p[15] = 'z'; return p },
		"cut off":        func(p []byte) []byte { return p[:16] },
		"tiny buffer":    func(p []byte) []byte { return p[:4] },
	}
	for name, damage := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeNode(damage(valid())); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("got error %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestOversizedLengthsAreRejected(t *testing.T) {
	// The buffer is large enough that only the size limits can catch these.
	for name, offset := range map[string]int{"key": 11, "value": 13} {
		t.Run(name, func(t *testing.T) {
			buf := make([]byte, PayloadSize)
			n := &node{leaf: true, keys: [][]byte{b("a")}, vals: [][]byte{b("1")}}
			if err := n.encode(buf); err != nil {
				t.Fatal(err)
			}
			binary.LittleEndian.PutUint16(buf[offset:], MaxValueSize+1)
			if _, err := decodeNode(buf); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("got error %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestSearch(t *testing.T) {
	n := &node{leaf: true, keys: [][]byte{b("b"), b("d"), b("f")}}
	tests := []struct {
		key   string
		index int
		found bool
	}{{"a", 0, false}, {"b", 0, true}, {"c", 1, false}, {"f", 2, true}, {"z", 3, false}}
	for _, tc := range tests {
		if i, found := n.search(b(tc.key)); i != tc.index || found != tc.found {
			t.Errorf("search(%q) = %d,%v, want %d,%v", tc.key, i, found, tc.index, tc.found)
		}
	}
}

func TestChildIndex(t *testing.T) {
	n := &node{keys: [][]byte{b("g"), b("p")}, children: []uint64{1, 2, 3}}
	tests := map[string]int{"a": 0, "f": 0, "g": 1, "m": 1, "p": 2, "z": 2}
	for key, want := range tests {
		if got := n.childIndex(b(key)); got != want {
			t.Errorf("childIndex(%q) = %d, want %d", key, got, want)
		}
	}
}

func TestSetAndDeleteLeaf(t *testing.T) {
	n := &node{leaf: true}
	n.setLeaf(b("c"), b("3"))
	n.setLeaf(b("a"), b("1"))
	n.setLeaf(b("b"), b("2"))
	n.setLeaf(b("b"), b("two"))

	if len(n.keys) != 3 || string(n.keys[0]) != "a" || string(n.keys[2]) != "c" {
		t.Fatalf("keys not sorted: %q", n.keys)
	}
	if string(n.vals[1]) != "two" {
		t.Fatalf("value not replaced: %q", n.vals[1])
	}
	if !n.deleteLeaf(b("b")) || n.deleteLeaf(b("b")) {
		t.Fatal("delete returned the wrong result")
	}
	if len(n.keys) != 2 || len(n.vals) != 2 || string(n.vals[1]) != "3" {
		t.Fatalf("wrong node after delete: %q=%q", n.keys, n.vals)
	}
}
