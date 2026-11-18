package minidb

import "errors"

// ErrCorrupt means data on disk failed a validity check.
var ErrCorrupt = errors.New("minidb: corrupt database")
