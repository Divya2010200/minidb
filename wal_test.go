package minidb

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
	"testing"
)

func TestWALRecordHasExactLayout(t *testing.T) {
	got := encodeRecord(walRecord{kind: recordPage, pageID: 7, payload: []byte("abc")})

	want := make([]byte, 20)
	want[0] = recordPage
	binary.LittleEndian.PutUint64(want[1:9], 7)
	binary.LittleEndian.PutUint32(want[9:13], 3)
	copy(want[13:], "abc")
	binary.LittleEndian.PutUint32(want[16:], crc32.Checksum(want[:16], crcTable))

	if len(got) != len(want) {
		t.Fatalf("record is %d bytes, want %d (13 header + 3 payload + 4 CRC)", len(got), len(want))
	}
	checks := []struct {
		name       string
		start, end int
	}{{"type", 0, 1}, {"page ID", 1, 9}, {"payload length", 9, 13}, {"payload", 13, 16}, {"CRC", 16, 20}}
	for _, c := range checks {
		if !bytes.Equal(got[c.start:c.end], want[c.start:c.end]) {
			t.Errorf("%s (bytes %d to %d): got %v, want %v", c.name, c.start, c.end-1, got[c.start:c.end], want[c.start:c.end])
		}
	}
}

func TestWALRecordRoundTrip(t *testing.T) {
	records := []walRecord{
		{kind: recordPage, pageID: 1, payload: bytes.Repeat([]byte("x"), PageSize)},
		{kind: recordPage, pageID: 1 << 40, payload: []byte("large page ID")},
		{kind: recordCommit, payload: []byte{}},
	}
	var log []byte
	for _, r := range records {
		log = append(log, encodeRecord(r)...)
	}
	for i, want := range records {
		got, used, err := decodeRecord(log)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if got.kind != want.kind || got.pageID != want.pageID || !bytes.Equal(got.payload, want.payload) {
			t.Fatalf("record %d decoded differently: kind=%d pageID=%d payload=%d bytes", i, got.kind, got.pageID, len(got.payload))
		}
		log = log[used:]
	}
	if _, _, err := decodeRecord(log); !errors.Is(err, io.EOF) {
		t.Fatalf("after last record: got %v, want io.EOF", err)
	}
}

func TestWALDetectsEveryFlippedByte(t *testing.T) {
	clean := encodeRecord(walRecord{kind: recordPage, pageID: 9, payload: []byte("data")})
	if len(clean) == 0 {
		t.Fatal("encodeRecord returned nothing")
	}
	for i := range clean {
		damaged := bytes.Clone(clean)
		damaged[i] ^= 0xFF
		if _, _, err := decodeRecord(damaged); !errors.Is(err, errBadRecord) {
			t.Fatalf("flipped byte %d: got %v, want errBadRecord", i, err)
		}
	}
}

func TestWALDetectsPartialWrites(t *testing.T) {
	clean := encodeRecord(walRecord{kind: recordPage, pageID: 9, payload: []byte("data")})
	if len(clean) == 0 {
		t.Fatal("encodeRecord returned nothing")
	}
	for size := 1; size < len(clean); size++ {
		if _, _, err := decodeRecord(clean[:size]); !errors.Is(err, errBadRecord) {
			t.Fatalf("first %d bytes: got %v, want errBadRecord", size, err)
		}
	}
}
