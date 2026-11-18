package minidb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"slices"
)

const (
	magic = "MINIDB01"

	metaPageID = 0
	// Metadata layout in page 0: magic (8) | root page ID (8) | page count (8).
	metaSize = 24
)

var (
	ErrClosed       = errors.New("minidb: database is closed")
	ErrInvalidKey   = errors.New("minidb: key must be 1 to 256 bytes")
	ErrValueTooLong = errors.New("minidb: value must be at most 1024 bytes")
)

// DB is a key-value store backed by a B+ tree in a single file.
// It is not safe for use by several goroutines or processes at once.
type DB struct {
	file   *os.File
	wal    *walLog
	pager  *pager
	root   uint64
	closed bool
}

// Open opens the database at path, creating it if it does not exist. Changes
// left in the log by a crash are recovered first. The log is stored at
// path + ".wal" and must stay with the database file.
func Open(path string) (*DB, error) {
	return openWithCache(path, defaultCachePages)
}

func openWithCache(path string, cachePages int) (*DB, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	wal, err := openWALLog(path + ".wal")
	if err != nil {
		file.Close()
		return nil, err
	}
	db, err := recoverAndLoad(file, wal, cachePages)
	if err != nil {
		wal.close()
		file.Close()
		return nil, err
	}
	return db, nil
}

func recoverAndLoad(file *os.File, wal *walLog, cachePages int) (*DB, error) {
	if err := recoverFromLog(file, wal); err != nil {
		return nil, err
	}
	return load(file, wal, cachePages)
}

// recoverFromLog copies complete logged transactions into the database file.
// It is safe to repeat if another crash interrupts it, because the log is only
// emptied after the pages are durable.
func recoverFromLog(file *os.File, wal *walLog) error {
	pages, err := wal.readCommitted()
	if err != nil {
		return err
	}
	ids := make([]uint64, 0, len(pages))
	for id, payload := range pages {
		if len(payload) != PayloadSize {
			return fmt.Errorf("%w: log entry for page %d has %d bytes", ErrCorrupt, id, len(payload))
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)

	data := make([]byte, PageSize)
	for _, id := range ids {
		copy(data[checksumSize:], pages[id])
		sealPage(data)
		if _, err := file.WriteAt(data, int64(id)*PageSize); err != nil {
			return fmt.Errorf("recover page %d: %w", id, err)
		}
	}
	if len(ids) > 0 {
		if err := file.Sync(); err != nil {
			return fmt.Errorf("sync recovered pages: %w", err)
		}
	}
	// This also discards any incomplete transaction at the end of the log.
	return wal.reset()
}

func load(file *os.File, wal *walLog, cachePages int) (*DB, error) {
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	size := info.Size()
	if size == 0 {
		return create(file, wal, cachePages)
	}
	if size%PageSize != 0 || size < 2*PageSize {
		return nil, fmt.Errorf("%w: invalid file size %d", ErrCorrupt, size)
	}

	p := newPager(file, 1, cachePages)
	meta, err := p.get(metaPageID)
	if err != nil {
		return nil, err
	}
	data := meta.payload()
	if string(data[:len(magic)]) != magic {
		return nil, fmt.Errorf("%w: not a minidb file", ErrCorrupt)
	}
	root := binary.LittleEndian.Uint64(data[8:16])
	numPages := binary.LittleEndian.Uint64(data[16:24])
	if numPages < 2 || numPages > uint64(size/PageSize) || root == metaPageID || root >= numPages {
		return nil, fmt.Errorf("%w: invalid metadata", ErrCorrupt)
	}
	p.numPages = numPages
	return &DB{file: file, wal: wal, pager: p, root: root}, nil
}

func create(file *os.File, wal *walLog, cachePages int) (*DB, error) {
	db := &DB{file: file, wal: wal, pager: newPager(file, 0, cachePages)}
	db.pager.allocate() // page 0 is reserved for metadata
	root, err := db.storeNew(&node{leaf: true})
	if err != nil {
		return nil, err
	}
	db.root = root
	if err := db.Sync(); err != nil {
		return nil, err
	}
	return db, nil
}

// Put stores value under key, replacing any existing value.
// The change is durable only after Sync or Close.
func (db *DB) Put(key, value []byte) error {
	if db.closed {
		return ErrClosed
	}
	if err := validateKey(key); err != nil {
		return err
	}
	if len(value) > MaxValueSize {
		return ErrValueTooLong
	}

	separator, right, err := db.insert(db.root, key, value)
	if err != nil {
		return err
	}
	if right == 0 {
		return nil
	}
	// The root split, so the tree grows one level taller.
	newRoot, err := db.storeNew(&node{
		keys:     [][]byte{separator},
		children: []uint64{db.root, right},
	})
	if err != nil {
		return err
	}
	db.root = newRoot
	return db.markMetaDirty()
}

// insert adds key below page id. If that page splits, it returns the separator
// key and new right-hand page for the parent to record.
func (db *DB) insert(id uint64, key, value []byte) ([]byte, uint64, error) {
	n, err := db.load(id)
	if err != nil {
		return nil, 0, err
	}

	if n.leaf {
		n.setLeaf(key, value)
	} else {
		i := n.childIndex(key)
		separator, right, err := db.insert(n.children[i], key, value)
		if err != nil {
			return nil, 0, err
		}
		if right == 0 {
			return nil, 0, nil // the child absorbed the change
		}
		n.keys = slices.Insert(n.keys, i, separator)
		n.children = slices.Insert(n.children, i+1, right)
	}

	if n.size() <= PayloadSize {
		return nil, 0, db.store(id, n)
	}
	if n.leaf {
		return db.splitLeaf(id, n)
	}
	return db.splitInternal(id, n)
}

func (db *DB) splitLeaf(id uint64, n *node) ([]byte, uint64, error) {
	mid := splitPoint(n)
	right := &node{
		leaf: true,
		keys: slices.Clone(n.keys[mid:]),
		vals: slices.Clone(n.vals[mid:]),
		next: n.next,
	}
	rightID, err := db.storeNew(right)
	if err != nil {
		return nil, 0, err
	}
	n.keys = n.keys[:mid]
	n.vals = n.vals[:mid]
	n.next = rightID
	if err := db.store(id, n); err != nil {
		return nil, 0, err
	}
	// The key is copied up: it stays in the right leaf and also guides searches.
	return right.keys[0], rightID, nil
}

func (db *DB) splitInternal(id uint64, n *node) ([]byte, uint64, error) {
	mid := min(splitPoint(n), len(n.keys)-2)
	separator := n.keys[mid]
	right := &node{
		keys:     slices.Clone(n.keys[mid+1:]),
		children: slices.Clone(n.children[mid+1:]),
	}
	rightID, err := db.storeNew(right)
	if err != nil {
		return nil, 0, err
	}
	n.keys = n.keys[:mid]
	n.children = n.children[:mid+1]
	if err := db.store(id, n); err != nil {
		return nil, 0, err
	}
	// The key moves up: internal nodes need only len(children)-1 separators.
	return separator, rightID, nil
}

// splitPoint divides a node by bytes, not entry count, because entries vary in size.
func splitPoint(n *node) int {
	half := (n.size() - nodeHeaderSize) / 2
	used := 0
	for i, key := range n.keys {
		used += lengthSize + len(key)
		if n.leaf {
			used += lengthSize + len(n.vals[i])
		} else {
			used += childIDSize
		}
		if used >= half {
			return max(1, min(i+1, len(n.keys)-1))
		}
	}
	return len(n.keys) - 1
}

// Get returns the value for key and whether it exists.
func (db *DB) Get(key []byte) ([]byte, bool, error) {
	if db.closed {
		return nil, false, ErrClosed
	}
	if err := validateKey(key); err != nil {
		return nil, false, err
	}
	leaf, _, err := db.findLeaf(key)
	if err != nil {
		return nil, false, err
	}
	i, found := leaf.search(key)
	if !found {
		return nil, false, nil
	}
	return leaf.vals[i], true, nil
}

// Delete removes key and reports whether it existed.
//
// Leaves are not merged afterwards, so the file does not shrink and space in
// a leaf is only reused by later inserts that land in that leaf.
func (db *DB) Delete(key []byte) (bool, error) {
	if db.closed {
		return false, ErrClosed
	}
	if err := validateKey(key); err != nil {
		return false, err
	}
	leaf, id, err := db.findLeaf(key)
	if err != nil {
		return false, err
	}
	if !leaf.deleteLeaf(key) {
		return false, nil
	}
	return true, db.store(id, leaf)
}

// Scan calls fn for each key in [start, end), in order. A nil start begins at
// the first key and a nil end continues to the last. Returning false stops the
// scan. The database must not be modified during a scan.
func (db *DB) Scan(start, end []byte, fn func(key, value []byte) bool) error {
	if db.closed {
		return ErrClosed
	}
	leaf, _, err := db.findLeaf(start)
	if err != nil {
		return err
	}
	i, _ := leaf.search(start)

	for hops := uint64(0); ; hops++ {
		if hops > db.pager.numPages {
			return fmt.Errorf("%w: cycle in leaf chain", ErrCorrupt)
		}
		for ; i < len(leaf.keys); i++ {
			if end != nil && bytes.Compare(leaf.keys[i], end) >= 0 {
				return nil
			}
			if !fn(leaf.keys[i], leaf.vals[i]) {
				return nil
			}
		}
		if leaf.next == 0 {
			return nil
		}
		if leaf, err = db.load(leaf.next); err != nil {
			return err
		}
		i = 0
	}
}

// Sync makes all changes durable as one transaction.
//
// The order matters: once the log and its commit record are on disk, the
// transaction is safe. Only then is the database file changed, and only after
// that is the log emptied. A crash at any point leaves either the old state or
// a log that can complete the new state.
func (db *DB) Sync() error {
	if db.closed {
		return ErrClosed
	}
	if len(db.pager.dirty) == 0 {
		return nil
	}
	if err := db.markMetaDirty(); err != nil {
		return err
	}
	if err := db.wal.appendTransaction(db.pager.dirtyPages()); err != nil {
		return err
	}
	if err := db.pager.flush(); err != nil {
		return err
	}
	return db.wal.reset()
}

// Close saves all changes and closes the file.
func (db *DB) Close() error {
	if db.closed {
		return ErrClosed
	}
	syncErr := db.Sync()
	db.closed = true
	return errors.Join(syncErr, db.wal.close(), db.file.Close())
}

// Height returns the number of levels in the tree.
func (db *DB) Height() (int, error) {
	if db.closed {
		return 0, ErrClosed
	}
	height := 1
	id := db.root
	for {
		n, err := db.load(id)
		if err != nil {
			return 0, err
		}
		if n.leaf {
			return height, nil
		}
		id = n.children[0]
		height++
	}
}

func (db *DB) findLeaf(key []byte) (*node, uint64, error) {
	id := db.root
	for depth := uint64(0); depth <= db.pager.numPages; depth++ {
		n, err := db.load(id)
		if err != nil {
			return nil, 0, err
		}
		if n.leaf {
			return n, id, nil
		}
		id = n.children[n.childIndex(key)]
	}
	return nil, 0, fmt.Errorf("%w: cycle in tree", ErrCorrupt)
}

func (db *DB) load(id uint64) (*node, error) {
	if id == metaPageID {
		return nil, fmt.Errorf("%w: node link points to metadata page", ErrCorrupt)
	}
	pg, err := db.pager.get(id)
	if err != nil {
		return nil, err
	}
	n, err := decodeNode(pg.payload())
	if err != nil {
		return nil, fmt.Errorf("page %d: %w", id, err)
	}
	return n, nil
}

func (db *DB) store(id uint64, n *node) error {
	pg, err := db.pager.get(id)
	if err != nil {
		return err
	}
	if err := n.encode(pg.payload()); err != nil {
		return err
	}
	db.pager.markDirty(pg)
	return nil
}

func (db *DB) storeNew(n *node) (uint64, error) {
	pg := db.pager.allocate()
	if err := n.encode(pg.payload()); err != nil {
		return 0, err
	}
	return pg.id, nil
}

func (db *DB) markMetaDirty() error {
	pg, err := db.pager.get(metaPageID)
	if err != nil {
		return err
	}
	data := pg.payload()
	copy(data, magic)
	binary.LittleEndian.PutUint64(data[8:16], db.root)
	binary.LittleEndian.PutUint64(data[16:metaSize], db.pager.numPages)
	db.pager.markDirty(pg)
	return nil
}

func validateKey(key []byte) error {
	if len(key) == 0 || len(key) > MaxKeySize {
		return ErrInvalidKey
	}
	return nil
}

// Check walks the whole tree, verifies its structure and returns the key count.
func (db *DB) Check() (int, error) {
	if db.closed {
		return 0, ErrClosed
	}
	c := checker{db: db, seen: make(map[uint64]bool), leafDepth: -1}
	if err := c.walk(db.root, nil, nil, 0); err != nil {
		return 0, err
	}
	for i, id := range c.leaves {
		want := uint64(0)
		if i+1 < len(c.leaves) {
			want = c.leaves[i+1]
		}
		if c.nexts[i] != want {
			return 0, fmt.Errorf("%w: leaf %d links to %d, want %d", ErrCorrupt, id, c.nexts[i], want)
		}
	}
	return c.keys, nil
}

type checker struct {
	db        *DB
	seen      map[uint64]bool
	leaves    []uint64
	nexts     []uint64
	leafDepth int
	keys      int
}

// walk verifies that every key in page id satisfies low <= key < high.
func (c *checker) walk(id uint64, low, high []byte, depth int) error {
	if c.seen[id] {
		return fmt.Errorf("%w: page %d is reachable twice", ErrCorrupt, id)
	}
	c.seen[id] = true

	n, err := c.db.load(id)
	if err != nil {
		return err
	}
	for _, key := range n.keys {
		if (low != nil && bytes.Compare(key, low) < 0) || (high != nil && bytes.Compare(key, high) >= 0) {
			return fmt.Errorf("%w: key %q on page %d is outside its range", ErrCorrupt, key, id)
		}
	}
	if n.leaf {
		if c.leafDepth >= 0 && c.leafDepth != depth {
			return fmt.Errorf("%w: leaves at depths %d and %d", ErrCorrupt, c.leafDepth, depth)
		}
		c.leafDepth = depth
		c.leaves = append(c.leaves, id)
		c.nexts = append(c.nexts, n.next)
		c.keys += len(n.keys)
		return nil
	}
	if len(n.children) != len(n.keys)+1 {
		return fmt.Errorf("%w: page %d has %d keys and %d children", ErrCorrupt, id, len(n.keys), len(n.children))
	}
	for i, child := range n.children {
		childLow, childHigh := low, high
		if i > 0 {
			childLow = n.keys[i-1]
		}
		if i < len(n.keys) {
			childHigh = n.keys[i]
		}
		if err := c.walk(child, childLow, childHigh, depth+1); err != nil {
			return err
		}
	}
	return nil
}
