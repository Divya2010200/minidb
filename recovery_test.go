package minidb

import (
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"testing"
)

// crashCase holds the files as they were just before a transaction, plus the
// bytes that transaction would write.
type crashCase struct {
	before  []byte
	log     []byte
	pages   []*page
	oldData map[string]string
	newData map[string]string
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// newCrashCase builds a transaction that updates, deletes and adds enough keys
// to split pages, then stops after writing its log.
func newCrashCase(t *testing.T) crashCase {
	t.Helper()
	path := testPath(t)
	db := openDB(t, path)
	oldData := map[string]string{}
	for i := 0; i < 1500; i += 2 {
		key, value := fmt.Sprintf("key-%05d", i), fmt.Sprintf("old-%d", i)
		mustPut(t, db, key, value)
		oldData[key] = value
	}
	if err := db.Sync(); err != nil {
		t.Fatal(err)
	}
	before := readFile(t, path)

	newData := map[string]string{}
	for key, value := range oldData {
		newData[key] = value
	}
	for i := 1; i < 1500; i += 2 {
		key, value := fmt.Sprintf("key-%05d", i), fmt.Sprintf("new-%d", i)
		mustPut(t, db, key, value)
		newData[key] = value
	}
	mustPut(t, db, "key-00000", "changed")
	newData["key-00000"] = "changed"
	if _, err := db.Delete(b("key-00002")); err != nil {
		t.Fatal(err)
	}
	delete(newData, "key-00002")

	if err := db.markMetaDirty(); err != nil {
		t.Fatal(err)
	}
	pages := db.pager.dirtyPages()
	if err := db.wal.appendTransaction(pages); err != nil {
		t.Fatal(err)
	}
	log := readFile(t, path+".wal")
	crash(db)
	return crashCase{before: before, log: log, pages: pages, oldData: oldData, newData: newData}
}

func wantExactly(t *testing.T, path string, want map[string]string) {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if count, err := db.Check(); err != nil || count != len(want) {
		t.Fatalf("Check = %d keys, %v; want %d keys", count, err, len(want))
	}
	for key, value := range want {
		got, found, err := db.Get(b(key))
		if err != nil || !found || string(got) != value {
			t.Fatalf("Get(%q) = %q, %v, %v; want %q", key, got, found, err, value)
		}
	}
}

// A crash while writing the log must leave the old data untouched, unless the
// whole transaction, including its commit record, reached the log.
func TestCrashWhileWritingLog(t *testing.T) {
	c := newCrashCase(t)
	rng := rand.New(rand.NewSource(1))
	cuts := []int{0, 1, recordHeaderSize, len(c.log) - 1, len(c.log)}
	for i := 0; i < 40; i++ {
		cuts = append(cuts, rng.Intn(len(c.log)))
	}
	for _, cut := range cuts {
		path := testPath(t)
		writeFile(t, path, c.before)
		writeFile(t, path+".wal", c.log[:cut])
		want := c.oldData
		if cut == len(c.log) {
			want = c.newData
		}
		wantExactly(t, path, want)
	}
}

// A crash while updating the database file, including one that tears a page in
// half, must be repaired from the log.
func TestCrashWhileUpdatingDatabaseFile(t *testing.T) {
	c := newCrashCase(t)
	rng := rand.New(rand.NewSource(2))
	for round := 0; round < 40; round++ {
		size := int64(c.pages[len(c.pages)-1].id+1) * PageSize
		image := make([]byte, max(size, int64(len(c.before))))
		copy(image, c.before)

		// Write a random subset of pages, with the last one only partly written.
		var last *page
		for _, pg := range rng.Perm(len(c.pages))[:rng.Intn(len(c.pages)+1)] {
			last = c.pages[pg]
			data := make([]byte, PageSize)
			copy(data[checksumSize:], last.payload())
			sealPage(data)
			copy(image[int64(last.id)*PageSize:], data)
		}
		if last != nil {
			start := int64(last.id)*PageSize + int64(rng.Intn(PageSize))
			clear(image[start : int64(last.id+1)*PageSize])
		}

		path := testPath(t)
		writeFile(t, path, image)
		writeFile(t, path+".wal", c.log)
		wantExactly(t, path, c.newData)
	}
}

func TestRecoveryCanRunAgainAfterItFinishes(t *testing.T) {
	c := newCrashCase(t)
	path := testPath(t)
	writeFile(t, path, c.before)
	writeFile(t, path+".wal", c.log)
	wantExactly(t, path, c.newData)
	if info, err := os.Stat(path + ".wal"); err != nil || info.Size() != 0 {
		t.Fatalf("log after recovery: %v, %v", info, err)
	}

	// Restoring the same log, as if the reset was lost, must change nothing.
	writeFile(t, path+".wal", c.log)
	wantExactly(t, path, c.newData)
}

func TestCrashWhileCreatingDatabase(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new.db")
	writeFile(t, path, nil)
	writeFile(t, path+".wal", []byte("partial"))
	wantExactly(t, path, map[string]string{})
}

// TestRepeatedCrashes runs random work and crashes after every round. Only
// changes completed by Sync may survive.
func TestRepeatedCrashes(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	path := testPath(t)
	model := map[string]string{}

	for round := 0; round < 60; round++ {
		db, err := openWithCache(path, 1+rng.Intn(64))
		if err != nil {
			t.Fatalf("round %d: %v", round, err)
		}
		working := make(map[string]string, len(model))
		for key, value := range model {
			working[key] = value
		}
		for op := 0; op < 300; op++ {
			key := fmt.Sprintf("key-%04d", rng.Intn(800))
			if rng.Intn(4) == 0 {
				if _, err := db.Delete(b(key)); err != nil {
					t.Fatal(err)
				}
				delete(working, key)
			} else {
				value := fmt.Sprintf("%d-%d", round, op)
				mustPut(t, db, key, value)
				working[key] = value
			}
			if rng.Intn(100) == 0 {
				if err := db.Sync(); err != nil {
					t.Fatal(err)
				}
				model = make(map[string]string, len(working))
				for k, v := range working {
					model[k] = v
				}
			}
		}
		crash(db)
		if round%10 == 9 {
			wantExactly(t, path, model)
		}
	}
	wantExactly(t, path, model)
}

// If Sync stops after the commit reaches the log, the change must be recovered.
func TestSyncFailureAfterLogCommitIsRecovered(t *testing.T) {
	path := testPath(t)
	db := openDB(t, path)
	mustPut(t, db, "key", "old")
	if err := db.Sync(); err != nil {
		t.Fatal(err)
	}
	mustPut(t, db, "key", "new")
	db.file.Close() // writing to the database file will now fail
	if err := db.Sync(); err == nil {
		t.Fatal("Sync succeeded with a closed database file")
	}
	crash(db)
	wantExactly(t, path, map[string]string{"key": "new"})
}

// The database file must not change until the log has been written.
func TestSyncFailureBeforeLogCommitKeepsOldData(t *testing.T) {
	path := testPath(t)
	db := openDB(t, path)
	mustPut(t, db, "key", "old")
	if err := db.Sync(); err != nil {
		t.Fatal(err)
	}
	mustPut(t, db, "key", "new")
	db.wal.close() // writing to the log will now fail
	if err := db.Sync(); err == nil {
		t.Fatal("Sync succeeded with a closed log")
	}
	crash(db)
	wantExactly(t, path, map[string]string{"key": "old"})
}
