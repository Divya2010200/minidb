# minidb

A disk-backed key-value storage engine written from scratch in Go, using only the standard library. It stores data in a B+ tree inside a single file.

## Features

- Fixed 4 KB pages with CRC32 checksums
- LRU page cache
- B+ tree with point lookups, inserts, updates, deletes and ordered range scans
- Node splitting by byte size, supporting variable-length keys and values
- Validation of all on-disk data when it is read
- A structural checker for the tree (`Check`)
- An interactive shell and a benchmark tool

## Architecture

```
Shell / Go API      Put, Get, Delete, Scan, Sync
B+ tree             db.go: search, splits, range scans, checks
Node format         node.go: encoding and validation
Pager               pager.go: pages, cache, checksums, flush order
Database file       page 0 = metadata, other pages = tree nodes
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

## Testing

- Randomised tests compare the database with a Go map through inserts, updates, deletes and reopens
- Tests use several cache sizes, including a single page
- Damaged files and invalid page contents must return errors

## Current limitations

- Not yet crash-safe: a failure during `Sync` can leave the file inconsistent
- One goroutine and one process at a time
- Keys are limited to 256 bytes and values to 1024 bytes
- Deleted space is not fully reclaimed because nodes are not merged

## Roadmap

- Write-ahead log and crash recovery
- Transactions
- A small SQL layer
