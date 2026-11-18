package minidb

import (
	"bytes"
	"errors"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func testPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "test.db")
}

// openDB opens a database that is closed automatically when the test ends.
func openDB(t *testing.T, path string) *DB {
	t.Helper()
	db, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if !db.closed {
			db.Close()
		}
	})
	return db
}

// crash closes the files without saving, as if the process had stopped.
func crash(db *DB) {
	db.wal.close()
	db.file.Close()
	db.closed = true
}

func mustPut(t *testing.T, db *DB, key, value string) {
	t.Helper()
	if err := db.Put(b(key), b(value)); err != nil {
		t.Fatalf("Put(%q): %v", key, err)
	}
}

func wantValue(t *testing.T, db *DB, key, want string) {
	t.Helper()
	got, found, err := db.Get(b(key))
	if err != nil {
		t.Fatalf("Get(%q): %v", key, err)
	}
	if !found || string(got) != want {
		t.Fatalf("Get(%q) = %q, %v; want %q", key, got, found, want)
	}
}

func wantMissing(t *testing.T, db *DB, key string) {
	t.Helper()
	if got, found, err := db.Get(b(key)); err != nil || found {
		t.Fatalf("Get(%q) = %q, %v, %v; want missing", key, got, found, err)
	}
}

func wantValid(t *testing.T, db *DB, wantKeys int) {
	t.Helper()
	got, err := db.Check()
	if err != nil {
		t.Fatal(err)
	}
	if got != wantKeys {
		t.Fatalf("tree has %d keys, want %d", got, wantKeys)
	}
}

func scanKeys(t *testing.T, db *DB, start, end []byte) []string {
	t.Helper()
	keys := []string{}
	err := db.Scan(start, end, func(k, _ []byte) bool {
		keys = append(keys, string(k))
		return true
	})
	if err != nil {
		t.Fatal(err)
	}
	return keys
}

func TestPutGetReplaceDelete(t *testing.T) {
	db := openDB(t, testPath(t))
	wantMissing(t, db, "name")

	mustPut(t, db, "name", "minidb")
	wantValue(t, db, "name", "minidb")
	mustPut(t, db, "name", "changed")
	wantValue(t, db, "name", "changed")
	wantValid(t, db, 1)

	deleted, err := db.Delete(b("name"))
	if err != nil || !deleted {
		t.Fatalf("Delete = %v, %v", deleted, err)
	}
	wantMissing(t, db, "name")
	if deleted, _ := db.Delete(b("name")); deleted {
		t.Fatal("second delete reported success")
	}
	wantValid(t, db, 0)
}

func TestManyKeysInEveryOrder(t *testing.T) {
	const count = 20000
	orders := map[string]func([]int){
		"ascending":  func([]int) {},
		"descending": func(s []int) { slices.Reverse(s) },
		"random": func(s []int) {
			rand.New(rand.NewSource(7)).Shuffle(len(s), func(i, j int) { s[i], s[j] = s[j], s[i] })
		},
	}
	for name, arrange := range orders {
		t.Run(name, func(t *testing.T) {
			db := openDB(t, testPath(t))
			order := make([]int, count)
			for i := range order {
				order[i] = i
			}
			arrange(order)
			for _, i := range order {
				mustPut(t, db, fmt.Sprintf("key-%06d", i), fmt.Sprintf("value-%d", i))
			}
			wantValid(t, db, count)
			for i := 0; i < count; i += 97 {
				wantValue(t, db, fmt.Sprintf("key-%06d", i), fmt.Sprintf("value-%d", i))
			}
			if height, err := db.Height(); err != nil || height < 2 {
				t.Fatalf("height = %d, %v", height, err)
			}
			keys := scanKeys(t, db, nil, nil)
			if len(keys) != count || !slices.IsSorted(keys) {
				t.Fatalf("scan returned %d keys, sorted=%v", len(keys), slices.IsSorted(keys))
			}
		})
	}
}

func TestLargestEntriesSplitCorrectly(t *testing.T) {
	db := openDB(t, testPath(t))
	value := bytes.Repeat([]byte("v"), MaxValueSize)
	for i := 0; i < 3000; i++ {
		key := fmt.Sprintf("%0*d", MaxKeySize, i)
		if err := db.Put(b(key), value); err != nil {
			t.Fatal(err)
		}
	}
	wantValid(t, db, 3000)
}

func TestGrowingAValueCanSplitALeaf(t *testing.T) {
	db := openDB(t, testPath(t))
	for i := 0; i < 100; i++ {
		mustPut(t, db, fmt.Sprintf("key-%03d", i), "x")
	}
	big := string(bytes.Repeat([]byte("y"), MaxValueSize))
	for i := 0; i < 100; i++ {
		mustPut(t, db, fmt.Sprintf("key-%03d", i), big)
	}
	wantValid(t, db, 100)
	wantValue(t, db, "key-050", big)
}

func TestScanBounds(t *testing.T) {
	db := openDB(t, testPath(t))
	for i := 0; i < 5000; i++ {
		mustPut(t, db, fmt.Sprintf("%05d", i), "v")
	}
	got := scanKeys(t, db, b("01000"), b("01003"))
	if !slices.Equal(got, []string{"01000", "01001", "01002"}) {
		t.Fatalf("got %v", got)
	}
	if got := scanKeys(t, db, b("00999x"), b("01001")); !slices.Equal(got, []string{"01000"}) {
		t.Fatalf("start between keys: got %v", got)
	}
	if got := scanKeys(t, db, b("99999"), nil); len(got) != 0 {
		t.Fatalf("start after last key: got %v", got)
	}

	seen := 0
	err := db.Scan(nil, nil, func(_, _ []byte) bool {
		seen++
		return seen < 3
	})
	if err != nil || seen != 3 {
		t.Fatalf("early stop: seen=%d err=%v", seen, err)
	}
}

func TestScanCrossesEmptyLeaves(t *testing.T) {
	db := openDB(t, testPath(t))
	for i := 0; i < 5000; i++ {
		mustPut(t, db, fmt.Sprintf("%05d", i), "v")
	}
	for i := 1; i < 4999; i++ {
		if _, err := db.Delete(b(fmt.Sprintf("%05d", i))); err != nil {
			t.Fatal(err)
		}
	}
	if got := scanKeys(t, db, nil, nil); !slices.Equal(got, []string{"00000", "04999"}) {
		t.Fatalf("got %v", got)
	}
	wantValid(t, db, 2)
}

func TestDataSurvivesCloseAndReopen(t *testing.T) {
	path := testPath(t)
	db := openDB(t, path)
	for i := 0; i < 10000; i++ {
		mustPut(t, db, fmt.Sprintf("key-%05d", i), fmt.Sprint(i))
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	db = openDB(t, path)
	wantValid(t, db, 10000)
	wantValue(t, db, "key-09999", "9999")
	mustPut(t, db, "key-10000", "added after reopen")
	wantValid(t, db, 10001)
}

func TestUnsyncedChangesAreNotWritten(t *testing.T) {
	path := testPath(t)
	db := openDB(t, path)
	mustPut(t, db, "saved", "yes")
	if err := db.Sync(); err != nil {
		t.Fatal(err)
	}
	mustPut(t, db, "unsaved", "no")

	// Simulate the process stopping before Sync.
	crash(db)

	db = openDB(t, path)
	wantValue(t, db, "saved", "yes")
	wantMissing(t, db, "unsaved")
}

// TestAgainstReferenceMap applies random operations to the database and to a
// Go map, and requires the two to agree throughout.
func TestAgainstReferenceMap(t *testing.T) {
	for _, cachePages := range []int{1, 16, defaultCachePages} {
		t.Run(fmt.Sprintf("cache-%d", cachePages), func(t *testing.T) {
			rng := rand.New(rand.NewSource(int64(cachePages)))
			path := testPath(t)
			db, err := openWithCache(path, cachePages)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if !db.closed {
					db.Close()
				}
			}()
			model := map[string]string{}

			for op := 0; op < 30000; op++ {
				key := fmt.Sprintf("key-%04d", rng.Intn(3000))
				switch roll := rng.Intn(100); {
				case roll < 55:
					value := string(bytes.Repeat([]byte("v"), rng.Intn(300)))
					mustPut(t, db, key, value)
					model[key] = value
				case roll < 80:
					deleted, err := db.Delete(b(key))
					if _, existed := model[key]; err != nil || deleted != existed {
						t.Fatalf("Delete(%q) = %v, %v; existed=%v", key, deleted, err, existed)
					}
					delete(model, key)
				default:
					got, found, err := db.Get(b(key))
					want, exists := model[key]
					if err != nil || found != exists || string(got) != want {
						t.Fatalf("Get(%q) = %q, %v, %v; want %q, %v", key, got, found, err, want, exists)
					}
				}
				if op%5000 == 4999 {
					if err := db.Close(); err != nil {
						t.Fatal(err)
					}
					if db, err = openWithCache(path, cachePages); err != nil {
						t.Fatal(err)
					}
					wantValid(t, db, len(model))
				}
			}

			want := make([]string, 0, len(model))
			for key := range model {
				want = append(want, key)
			}
			slices.Sort(want)
			if got := scanKeys(t, db, nil, nil); !slices.Equal(got, want) {
				t.Fatalf("scan has %d keys, want %d", len(got), len(want))
			}
		})
	}
}

func TestInvalidInputIsRejected(t *testing.T) {
	db := openDB(t, testPath(t))
	if err := db.Put(nil, b("v")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("empty key: %v", err)
	}
	if err := db.Put(make([]byte, MaxKeySize+1), b("v")); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("long key: %v", err)
	}
	if err := db.Put(b("k"), make([]byte, MaxValueSize+1)); !errors.Is(err, ErrValueTooLong) {
		t.Fatalf("long value: %v", err)
	}
	if _, _, err := db.Get(nil); !errors.Is(err, ErrInvalidKey) {
		t.Fatalf("get empty key: %v", err)
	}
}

func TestClosedDatabaseRejectsCalls(t *testing.T) {
	db := openDB(t, testPath(t))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if err := db.Put(b("k"), b("v")); !errors.Is(err, ErrClosed) {
		t.Fatalf("Put: %v", err)
	}
	if err := db.Close(); !errors.Is(err, ErrClosed) {
		t.Fatalf("Close: %v", err)
	}
}

func TestOpenRejectsOtherFiles(t *testing.T) {
	notes := bytes.Repeat([]byte("x"), 100)
	wrongMagic := make([]byte, 2*PageSize)
	for name, content := range map[string][]byte{"wrong size": notes, "bad page": wrongMagic} {
		t.Run(name, func(t *testing.T) {
			path := testPath(t)
			if err := os.WriteFile(path, content, 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Open(path); !errors.Is(err, ErrCorrupt) {
				t.Fatalf("got %v, want ErrCorrupt", err)
			}
		})
	}
}

func TestDamagedDataPageIsReported(t *testing.T) {
	path := testPath(t)
	db := openDB(t, path)
	mustPut(t, db, "key", "value")
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteAt([]byte{0xFF}, PageSize+50)
	file.Close()
	if err != nil {
		t.Fatal(err)
	}

	db = openDB(t, path)
	if _, _, err := db.Get(b("key")); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}

func TestCheckFindsABrokenLeafChain(t *testing.T) {
	db := openDB(t, testPath(t))
	for i := 0; i < 2000; i++ {
		mustPut(t, db, fmt.Sprintf("%05d", i), "value")
	}
	leaf, id, err := db.findLeaf(b("00000"))
	if err != nil {
		t.Fatal(err)
	}
	leaf.next = 0
	if err := db.store(id, leaf); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Check(); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("got %v, want ErrCorrupt", err)
	}
}
