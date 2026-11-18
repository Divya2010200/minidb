package minidb

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func pageRecord(id uint64, text string) []byte {
	return encodeRecord(walRecord{kind: recordPage, pageID: id, payload: []byte(text)})
}

func commitRecord() []byte {
	return encodeRecord(walRecord{kind: recordCommit})
}

func joinRecords(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

func wantPages(t *testing.T, got map[uint64][]byte, want map[uint64]string) {
	t.Helper()
	if got == nil {
		t.Fatal("committedPages returned nil, want a map")
	}
	if len(got) != len(want) {
		t.Fatalf("got %d pages, want %d", len(got), len(want))
	}
	for id, text := range want {
		if string(got[id]) != text {
			t.Errorf("page %d: got %q, want %q", id, got[id], text)
		}
	}
}

func TestWALEmptyLogHasNoPages(t *testing.T) {
	wantPages(t, committedPages(nil), map[uint64]string{})
}

func TestWALCommittedPagesAreReturned(t *testing.T) {
	log := joinRecords(pageRecord(1, "one"), pageRecord(2, "two"), commitRecord())
	wantPages(t, committedPages(log), map[uint64]string{1: "one", 2: "two"})
}

func TestWALUncommittedPagesAreIgnored(t *testing.T) {
	log := joinRecords(pageRecord(1, "saved"), commitRecord(), pageRecord(2, "unfinished"))
	wantPages(t, committedPages(log), map[uint64]string{1: "saved"})
}

func TestWALNewestCommittedVersionWins(t *testing.T) {
	log := joinRecords(
		pageRecord(1, "old"), commitRecord(),
		pageRecord(1, "new"), commitRecord(),
		pageRecord(1, "unfinished"),
	)
	wantPages(t, committedPages(log), map[uint64]string{1: "new"})
}

func TestWALPartialRecordIsIgnored(t *testing.T) {
	first := joinRecords(pageRecord(1, "a"), commitRecord())
	torn := pageRecord(2, "b")
	log := joinRecords(first, torn[:len(torn)-1])
	wantPages(t, committedPages(log), map[uint64]string{1: "a"})
}

func TestWALStopsAtDamagedRecord(t *testing.T) {
	damaged := pageRecord(2, "bad")
	damaged[14] ^= 0xFF
	log := joinRecords(
		pageRecord(1, "good"), commitRecord(),
		damaged, commitRecord(),
		pageRecord(3, "after damage"), commitRecord(),
	)
	wantPages(t, committedPages(log), map[uint64]string{1: "good"})
}

func TestWALFileRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	log, err := openWALLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.close() })

	pg := &page{id: 5, data: make([]byte, PageSize)}
	copy(pg.payload(), "page five")
	if err := log.appendTransaction([]*page{pg}); err != nil {
		t.Fatal(err)
	}
	copy(pg.payload(), "page FIVE")
	if err := log.appendTransaction([]*page{pg}); err != nil {
		t.Fatal(err)
	}
	if err := log.close(); err != nil {
		t.Fatal(err)
	}

	if log, err = openWALLog(path); err != nil {
		t.Fatal(err)
	}
	pages, err := log.readCommitted()
	if err != nil {
		t.Fatal(err)
	}
	if len(pages) != 1 || !bytes.Equal(pages[5], pg.payload()) {
		t.Fatalf("got %d pages with wrong contents", len(pages))
	}

	if err := log.reset(); err != nil {
		t.Fatal(err)
	}
	if pages, err = log.readCommitted(); err != nil || len(pages) != 0 {
		t.Fatalf("after reset: %d pages, err=%v", len(pages), err)
	}
}

func TestWALFileIgnoresPartialLastTransaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "test.wal")
	log, err := openWALLog(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { log.close() })

	pg := &page{id: 1, data: make([]byte, PageSize)}
	copy(pg.payload(), "complete")
	if err := log.appendTransaction([]*page{pg}); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	complete := info.Size()

	copy(pg.payload(), "partial")
	if err := log.appendTransaction([]*page{pg}); err != nil {
		t.Fatal(err)
	}
	// Simulate a crash partway through the second transaction.
	if err := log.file.Truncate(complete + 100); err != nil {
		t.Fatal(err)
	}

	pages, err := log.readCommitted()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pages[1], []byte("complete")) {
		t.Fatalf("got %q", pages[1][:8])
	}
}
