package minidb

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func openTestFile(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { f.Close() })
	return f
}

func TestPagesSurviveReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	p := newPager(openTestFile(t, path), 0, 10)

	pg := p.allocate()
	copy(pg.payload(), "hello page")
	if err := p.flush(); err != nil {
		t.Fatal(err)
	}

	reopened := newPager(openTestFile(t, path), 1, 10)
	got, err := reopened.get(0)
	if err != nil {
		t.Fatal(err)
	}
	if string(got.payload()[:10]) != "hello page" {
		t.Fatalf("got %q", got.payload()[:10])
	}
}

func TestDirtyPagesStayOffDiskUntilFlush(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	f := openTestFile(t, path)
	p := newPager(f, 0, 10)
	p.allocate()

	info, err := f.Stat()
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() != 0 {
		t.Fatalf("file has %d bytes before flush, want 0", info.Size())
	}
}

func TestChecksumDetectsCorruption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.db")
	f := openTestFile(t, path)
	p := newPager(f, 0, 10)
	copy(p.allocate().payload(), "important data")
	if err := p.flush(); err != nil {
		t.Fatal(err)
	}

	// Flip one byte on disk, as a failing drive might.
	if _, err := f.WriteAt([]byte{0xFF}, 100); err != nil {
		t.Fatal(err)
	}

	fresh := newPager(f, 1, 10)
	if _, err := fresh.get(0); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got error %v, want ErrCorrupt", err)
	}
}

func TestOutOfRangePageIsRejected(t *testing.T) {
	p := newPager(openTestFile(t, filepath.Join(t.TempDir(), "test.db")), 0, 10)
	if _, err := p.get(5); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got error %v, want ErrCorrupt", err)
	}
}

func TestCacheEvictsCleanPagesOnly(t *testing.T) {
	p := newPager(openTestFile(t, filepath.Join(t.TempDir(), "test.db")), 0, 2)
	for i := 0; i < 5; i++ {
		p.allocate()
	}
	if len(p.dirty) != 5 {
		t.Fatalf("%d dirty pages, want all 5 kept", len(p.dirty))
	}
	if err := p.flush(); err != nil {
		t.Fatal(err)
	}
	if p.lru.Len() != 2 || len(p.clean) != 2 {
		t.Fatalf("cache holds %d pages, want 2", p.lru.Len())
	}

	// An evicted page must come back from disk. Which pages stay cached after
	// a flush is not fixed, so find one that was actually evicted.
	var evicted uint64
	for id := uint64(0); id < 5; id++ {
		if _, cached := p.clean[id]; !cached {
			evicted = id
			break
		}
	}
	if _, err := p.get(evicted); err != nil {
		t.Fatal(err)
	}
	if p.reads != 1 {
		t.Fatalf("%d disk reads, want 1", p.reads)
	}
}
