# minidb

A disk-backed key-value storage engine written in Go using only the standard library. It stores data in a B+ tree and uses a write-ahead log to recover from crashes.

## Features

- Fixed 4 KB pages with CRC32 checksums
- LRU page cache
- B+ tree with point lookups, inserts, updates, deletes and ordered range scans
- Node splitting by byte size, supporting variable-length keys and values
- Write-ahead log with commit records, so each `Sync` is applied completely or not at all
- Crash recovery when the database is opened
- Validation of all on-disk data when it is read
- A structural checker for the tree (`Check`)
- An interactive shell and a benchmark tool

## Architecture

```
Shell / Go API      Put, Get, Delete, Scan, Sync
B+ tree             db.go: search, splits, range scans, checks
Node format         node.go: encoding and validation
Write-ahead log     wal.go, wal_log.go: records, commits, recovery input
Pager               pager.go: pages, cache, checksums, flush order
Database files      data.db (pages) and data.db.wal (log)
```

## Run it

```
go test ./...
go run ./cmd/minidb demo.db
go run ./cmd/minibench
```

Example shell session:

```
minidb> put city London
OK
minidb> get city
London
minidb> scan
city = London
```

## Use it from Go

```go
db, err := minidb.Open("data.db")
if err != nil {
    return err
}
defer db.Close()

if err := db.Put([]byte("city"), []byte("London")); err != nil {
    return err
}
value, found, err := db.Get([]byte("city"))
```

## How a `Sync` stays safe

1. Write every changed page and a commit record to the log.
2. `fsync` the log. The transaction is now safe.
3. Write the pages to the database file and `fsync` it.
4. Empty the log.

After a crash, `Open` replays complete transactions and ignores incomplete ones.

## Testing

- Randomised tests compare the database with a Go map through inserts, updates, deletes and reopens
- Tests use several cache sizes, including a single page
- Damaged files and invalid page contents must return errors
- Simulated crashes cut the log at many positions and leave partly written database pages
- Repeated crash rounds confirm that only synced changes survive

## Current limitations

- Crash recovery is tested by simulating failures, not by cutting power to real hardware
- `Sync` is the only transaction boundary; there is no rollback API yet
- One goroutine and one process at a time
- Keys are limited to 256 bytes and values to 1024 bytes
- Deleted space is not fully reclaimed because nodes are not merged

## Roadmap

- Explicit transactions with rollback
- Node merging and free-page reuse
- A small SQL layer
