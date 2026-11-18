package minidb

import (
	"encoding/binary"
	"errors"
	"hash/crc32"
	"io"
)

// A write-ahead log (WAL) records changes in a separate file before the main
// database file is touched. After a crash, complete changes can be replayed.
//
// Each record has this layout:
//
//	byte 0        record type
//	bytes 1-8     page ID (uint64)
//	bytes 9-12    payload length (uint32)
//	next bytes    payload
//	last 4 bytes  CRC32 of everything before it (uint32)
const (
	recordPage   byte = 1 // holds the new contents of one page
	recordCommit byte = 2 // marks all earlier page records as complete

	recordHeaderSize = 13
	recordCRCSize    = 4
)

// errBadRecord means a log record is damaged or only partly written.
var errBadRecord = errors.New("minidb: bad log record")

type walRecord struct {
	kind    byte
	pageID  uint64
	payload []byte
}

// encodeRecord turns a record into bytes using the layout above.
//
// TODO: write this function yourself.
//
//  1. Work out the total size: header + payload + CRC.
//  2. Create a byte slice of that size with make.
//  3. Put r.kind in byte 0.
//  4. Put r.pageID in bytes 1-8 and the payload length in bytes 9-12.
//  5. Copy r.payload to the position right after the header.
//  6. Calculate the CRC of everything before the last 4 bytes, using
//     crc32.Checksum(data, crcTable), and store it in the last 4 bytes.
//  7. Return the slice.
func encodeRecord(r walRecord) []byte {
	total := recordHeaderSize + len(r.payload) + recordCRCSize
	buf := make([]byte, total)

	buf[0] = r.kind
	binary.LittleEndian.PutUint64(buf[1:9], r.pageID)
	binary.LittleEndian.PutUint32(buf[9:13], uint32(len(r.payload)))
	copy(buf[recordHeaderSize:], r.payload)
	crcAt := total - recordCRCSize
	checksum := crc32.Checksum(buf[:crcAt], crcTable)
	binary.LittleEndian.PutUint32(buf[crcAt:], checksum)

	return buf
}

// decodeRecord reads the first record in buf and returns it with the number of
// bytes it used. It returns io.EOF when buf is empty, and errBadRecord when the
// record is incomplete or damaged.
func decodeRecord(buf []byte) (walRecord, int, error) {
	if len(buf) == 0 {
		return walRecord{}, 0, io.EOF
	}
	if len(buf) < recordHeaderSize+recordCRCSize {
		return walRecord{}, 0, errBadRecord
	}

	length := binary.LittleEndian.Uint32(buf[9:13])
	if length > PageSize {
		return walRecord{}, 0, errBadRecord
	}
	total := recordHeaderSize + int(length) + recordCRCSize
	if len(buf) < total {
		return walRecord{}, 0, errBadRecord
	}

	crcAt := total - recordCRCSize
	if crc32.Checksum(buf[:crcAt], crcTable) != binary.LittleEndian.Uint32(buf[crcAt:total]) {
		return walRecord{}, 0, errBadRecord
	}
	kind := buf[0]
	if kind != recordPage && kind != recordCommit {
		return walRecord{}, 0, errBadRecord
	}

	payload := make([]byte, length)
	copy(payload, buf[recordHeaderSize:crcAt])
	return walRecord{
		kind:    kind,
		pageID:  binary.LittleEndian.Uint64(buf[1:9]),
		payload: payload,
	}, total, nil
}
