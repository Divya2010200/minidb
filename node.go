package minidb

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"sort"
)

const (
	// MaxKeySize and MaxValueSize guarantee that several entries fit in a page,
	// which the B+ tree relies on when it splits a full node.
	MaxKeySize   = 256
	MaxValueSize = 1024

	nodeLeaf     byte = 1
	nodeInternal byte = 2

	// Header: type (1 byte) + entry count (2) + link (8).
	// The link is the next leaf for a leaf, or the first child for an internal node.
	nodeHeaderSize = 11
	lengthSize     = 2
	childIDSize    = 8
)

// node is the in-memory form of one B+ tree page.
//
// A leaf stores keys[i] -> vals[i].
// An internal node stores len(keys)+1 children. children[i+1] holds keys that
// are greater than or equal to keys[i].
type node struct {
	leaf     bool
	keys     [][]byte
	vals     [][]byte
	children []uint64
	next     uint64 // next leaf in key order; 0 means none
}

// size returns the number of bytes the node needs when encoded.
func (n *node) size() int {
	total := nodeHeaderSize
	for i, key := range n.keys {
		total += lengthSize + len(key)
		if n.leaf {
			total += lengthSize + len(n.vals[i])
		} else {
			total += childIDSize
		}
	}
	return total
}

// encode writes the node into buf, which is normally a page payload.
//
// Leaf entry:     key length | value length | key | value
// Internal entry: key length | key | child page ID
func (n *node) encode(buf []byte) error {
	if size := n.size(); size > len(buf) {
		return fmt.Errorf("node needs %d bytes but only %d are available", size, len(buf))
	}
	clear(buf)

	binary.LittleEndian.PutUint16(buf[1:3], uint16(len(n.keys)))
	if n.leaf {
		buf[0] = nodeLeaf
		binary.LittleEndian.PutUint64(buf[3:11], n.next)
	} else {
		buf[0] = nodeInternal
		binary.LittleEndian.PutUint64(buf[3:11], n.children[0])
	}

	pos := nodeHeaderSize
	for i, key := range n.keys {
		binary.LittleEndian.PutUint16(buf[pos:], uint16(len(key)))
		pos += lengthSize
		if n.leaf {
			val := n.vals[i]
			binary.LittleEndian.PutUint16(buf[pos:], uint16(len(val)))
			pos += lengthSize
			pos += copy(buf[pos:], key)
			pos += copy(buf[pos:], val)
		} else {
			pos += copy(buf[pos:], key)
			binary.LittleEndian.PutUint64(buf[pos:], n.children[i+1])
			pos += childIDSize
		}
	}
	return nil
}

// decodeNode reads a node from buf and validates everything it reads, so a
// damaged page returns an error instead of causing a crash.
func decodeNode(buf []byte) (*node, error) {
	if len(buf) < nodeHeaderSize {
		return nil, fmt.Errorf("%w: node buffer too small", ErrCorrupt)
	}
	// Work on a private copy: the page buffer is overwritten by later encodes.
	data := bytes.Clone(buf)

	n := &node{}
	switch data[0] {
	case nodeLeaf:
		n.leaf = true
	case nodeInternal:
	default:
		return nil, fmt.Errorf("%w: unknown node type %d", ErrCorrupt, data[0])
	}

	count := int(binary.LittleEndian.Uint16(data[1:3]))
	link := binary.LittleEndian.Uint64(data[3:11])
	if n.leaf {
		n.next = link
		n.vals = make([][]byte, 0, count)
	} else {
		if count == 0 {
			return nil, fmt.Errorf("%w: internal node has no keys", ErrCorrupt)
		}
		n.children = make([]uint64, 1, count+1)
		n.children[0] = link
	}
	n.keys = make([][]byte, 0, count)

	pos := nodeHeaderSize
	take := func(size int) ([]byte, error) {
		if size > len(data)-pos {
			return nil, fmt.Errorf("%w: node entry runs past end of page", ErrCorrupt)
		}
		out := data[pos : pos+size : pos+size]
		pos += size
		return out, nil
	}
	takeLength := func(limit int) (int, error) {
		raw, err := take(lengthSize)
		if err != nil {
			return 0, err
		}
		size := int(binary.LittleEndian.Uint16(raw))
		if size > limit {
			return 0, fmt.Errorf("%w: length %d exceeds limit %d", ErrCorrupt, size, limit)
		}
		return size, nil
	}

	for i := 0; i < count; i++ {
		keyLen, err := takeLength(MaxKeySize)
		if err != nil {
			return nil, err
		}
		valLen := 0
		if n.leaf {
			if valLen, err = takeLength(MaxValueSize); err != nil {
				return nil, err
			}
		}
		key, err := take(keyLen)
		if err != nil {
			return nil, err
		}
		if i > 0 && bytes.Compare(n.keys[i-1], key) >= 0 {
			return nil, fmt.Errorf("%w: keys out of order", ErrCorrupt)
		}
		n.keys = append(n.keys, key)

		if n.leaf {
			val, err := take(valLen)
			if err != nil {
				return nil, err
			}
			n.vals = append(n.vals, val)
		} else {
			raw, err := take(childIDSize)
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, binary.LittleEndian.Uint64(raw))
		}
	}
	return n, nil
}

// search uses binary search to find key. It returns the key's position, or the
// position where it belongs if it is absent.
func (n *node) search(key []byte) (int, bool) {
	return slices.BinarySearchFunc(n.keys, key, bytes.Compare)
}

// childIndex returns which child of an internal node may contain key.
func (n *node) childIndex(key []byte) int {
	return sort.Search(len(n.keys), func(i int) bool {
		return bytes.Compare(n.keys[i], key) > 0
	})
}

// setLeaf inserts key or replaces its value, keeping the keys sorted.
func (n *node) setLeaf(key, val []byte) {
	i, found := n.search(key)
	if found {
		n.vals[i] = val
		return
	}
	n.keys = slices.Insert(n.keys, i, key)
	n.vals = slices.Insert(n.vals, i, val)
}

// deleteLeaf removes key and reports whether it was present.
func (n *node) deleteLeaf(key []byte) bool {
	i, found := n.search(key)
	if !found {
		return false
	}
	n.keys = slices.Delete(n.keys, i, i+1)
	n.vals = slices.Delete(n.vals, i, i+1)
	return true
}
