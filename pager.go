package minidb

import (
	"container/list"
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"slices"
)

const (
	// PageSize is the unit of disk I/O. 4 KiB matches common OS page sizes.
	PageSize = 4096
	// The first bytes of each page hold a CRC32 of the rest of that page.
	checksumSize = 4
	// PayloadSize is the space in a page available to higher layers.
	PayloadSize = PageSize - checksumSize

	defaultCachePages = 1024
)

var crcTable = crc32.MakeTable(crc32.Castagnoli)

type page struct {
	id   uint64
	data []byte // always PageSize bytes
}

func (p *page) payload() []byte { return p.data[checksumSize:] }

// pager moves fixed-size pages between the file and memory.
//
// Clean pages live in an LRU cache and may be evicted. Dirty pages are pinned
// in memory until flush, so unsynced changes never reach the data file early.
// Milestone 2 (the write-ahead log) relies on that property.
type pager struct {
	file     *os.File
	numPages uint64
	capacity int
	dirty    map[uint64]*page
	clean    map[uint64]*list.Element
	lru      *list.List // front = most recently used
	reads    uint64
	writes   uint64
}

func newPager(file *os.File, numPages uint64, capacity int) *pager {
	return &pager{
		file:     file,
		numPages: numPages,
		capacity: capacity,
		dirty:    make(map[uint64]*page),
		clean:    make(map[uint64]*list.Element),
		lru:      list.New(),
	}
}

func (p *pager) get(id uint64) (*page, error) {
	if id >= p.numPages {
		return nil, fmt.Errorf("%w: page %d out of range (%d pages)", ErrCorrupt, id, p.numPages)
	}
	if pg, ok := p.dirty[id]; ok {
		return pg, nil
	}
	if el, ok := p.clean[id]; ok {
		p.lru.MoveToFront(el)
		return el.Value.(*page), nil
	}

	pg := &page{id: id, data: make([]byte, PageSize)}
	if _, err := p.file.ReadAt(pg.data, int64(id)*PageSize); err != nil {
		return nil, fmt.Errorf("read page %d: %w", id, err)
	}
	p.reads++
	want := binary.LittleEndian.Uint32(pg.data[:checksumSize])
	if got := crc32.Checksum(pg.payload(), crcTable); got != want {
		return nil, fmt.Errorf("%w: bad checksum on page %d", ErrCorrupt, id)
	}
	p.addClean(pg)
	return pg, nil
}

func (p *pager) allocate() *page {
	pg := &page{id: p.numPages, data: make([]byte, PageSize)}
	p.numPages++
	p.dirty[pg.id] = pg
	return pg
}

func (p *pager) markDirty(pg *page) {
	if el, ok := p.clean[pg.id]; ok {
		p.lru.Remove(el)
		delete(p.clean, pg.id)
	}
	p.dirty[pg.id] = pg
}

// flush writes dirty pages, then page 0 (the metadata), with an fsync after
// each stage so the metadata never points at pages that are not yet on disk.
func (p *pager) flush() error {
	ids := make([]uint64, 0, len(p.dirty))
	for id := range p.dirty {
		if id != 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids) // sequential writes are kinder to the disk
	for _, id := range ids {
		if err := p.writePage(p.dirty[id]); err != nil {
			return err
		}
	}
	if err := p.file.Sync(); err != nil {
		return fmt.Errorf("sync data pages: %w", err)
	}
	if meta, ok := p.dirty[0]; ok {
		if err := p.writePage(meta); err != nil {
			return err
		}
		if err := p.file.Sync(); err != nil {
			return fmt.Errorf("sync metadata: %w", err)
		}
	}
	for id, pg := range p.dirty {
		delete(p.dirty, id)
		p.addClean(pg)
	}
	return nil
}

func (p *pager) writePage(pg *page) error {
	binary.LittleEndian.PutUint32(pg.data[:checksumSize], crc32.Checksum(pg.payload(), crcTable))
	if _, err := p.file.WriteAt(pg.data, int64(pg.id)*PageSize); err != nil {
		return fmt.Errorf("write page %d: %w", pg.id, err)
	}
	p.writes++
	return nil
}

func (p *pager) addClean(pg *page) {
	p.clean[pg.id] = p.lru.PushFront(pg)
	for p.lru.Len() > p.capacity {
		oldest := p.lru.Back()
		p.lru.Remove(oldest)
		delete(p.clean, oldest.Value.(*page).id)
	}
}
