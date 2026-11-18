package minidb

import (
	"fmt"
	"io"
	"os"
)

// walLog is the log file stored next to the database.
type walLog struct {
	file *os.File
}

func openWALLog(path string) (*walLog, error) {
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return nil, err
	}
	return &walLog{file: file}, nil
}

// appendTransaction durably records a group of page changes. The commit record
// goes last, so it can only be valid if every page record before it was written.
func (l *walLog) appendTransaction(pages []*page) error {
	var data []byte
	for _, pg := range pages {
		data = append(data, encodeRecord(walRecord{
			kind:    recordPage,
			pageID:  pg.id,
			payload: pg.payload(),
		})...)
	}
	data = append(data, encodeRecord(walRecord{kind: recordCommit})...)

	info, err := l.file.Stat()
	if err != nil {
		return err
	}
	if _, err := l.file.WriteAt(data, info.Size()); err != nil {
		return fmt.Errorf("write log: %w", err)
	}
	if err := l.file.Sync(); err != nil {
		return fmt.Errorf("sync log: %w", err)
	}
	return nil
}

// readCommitted returns the page contents from complete transactions.
func (l *walLog) readCommitted() (map[uint64][]byte, error) {
	info, err := l.file.Stat()
	if err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.NewSectionReader(l.file, 0, info.Size()))
	if err != nil {
		return nil, fmt.Errorf("read log: %w", err)
	}
	return committedPages(data), nil
}

// reset empties the log once its changes are safely in the database file.
func (l *walLog) reset() error {
	if err := l.file.Truncate(0); err != nil {
		return fmt.Errorf("truncate log: %w", err)
	}
	return l.file.Sync()
}

func (l *walLog) close() error {
	return l.file.Close()
}

// committedPages reads the log from the start and returns the newest contents
// of each page changed by a complete transaction.
//
// Page records wait in "pending" until a commit record arrives. Reading stops
// at the end of the log or at the first bad record, so anything after a damaged
// record is ignored.
func committedPages(log []byte) map[uint64][]byte {
	committed := make(map[uint64][]byte)
	pending := make(map[uint64][]byte)

	for {
		record, used, err := decodeRecord(log)
		if err != nil {
			return committed
		}
		log = log[used:]

		if record.kind == recordPage {
			pending[record.pageID] = record.payload
		}
		if record.kind == recordCommit {
			for id, data := range pending {
				committed[id] = data
			}
			pending = make(map[uint64][]byte)
		}
	}
}
