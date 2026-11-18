// Command minibench measures minidb on the current machine.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"math/rand"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"time"

	"minidb"
)

const (
	valueSize   = 100
	syncEvery   = 1000
	durableOps  = 500
	rangeScans  = 1000
	rangeLength = 100
)

var value = bytes.Repeat([]byte("v"), valueSize)

func key(i int) []byte { return []byte(fmt.Sprintf("%016d", i)) }

func main() {
	count := flag.Int("n", 200000, "number of keys")
	flag.Parse()
	if err := run(*count); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(n int) error {
	if n < rangeLength*2 {
		return fmt.Errorf("-n must be at least %d", rangeLength*2)
	}
	dir, err := os.MkdirTemp("", "minibench")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	rng := rand.New(rand.NewSource(1))

	fmt.Printf("%d keys, 16-byte keys, %d-byte values, %s/%s, %s\n\n",
		n, valueSize, runtime.GOOS, runtime.GOARCH, runtime.Version())
	fmt.Println("| Operation | Count | Per second | Median | 99th percentile |")
	fmt.Println("|---|---:|---:|---:|---:|")

	order := make([]int, n)
	for i := range order {
		order[i] = i
	}
	if err := load(filepath.Join(dir, "seq.db"), "Sequential insert (sync every 1,000)", order); err != nil {
		return err
	}
	rng.Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] })
	path := filepath.Join(dir, "random.db")
	if err := load(path, "Random insert (sync every 1,000)", order); err != nil {
		return err
	}

	db, err := minidb.Open(path)
	if err != nil {
		return err
	}
	defer db.Close()

	rng.Shuffle(n, func(i, j int) { order[i], order[j] = order[j], order[i] })
	err = measure("Random point read", n, func(i int) error {
		_, found, err := db.Get(key(order[i]))
		if err == nil && !found {
			err = fmt.Errorf("key %d is missing", order[i])
		}
		return err
	})
	if err != nil {
		return err
	}

	err = measure(fmt.Sprintf("Range scan (%d keys)", rangeLength), rangeScans, func(int) error {
		rows := 0
		err := db.Scan(key(rng.Intn(n-rangeLength)), nil, func(_, _ []byte) bool {
			rows++
			return rows < rangeLength
		})
		if err == nil && rows != rangeLength {
			err = fmt.Errorf("scan returned %d keys", rows)
		}
		return err
	})
	if err != nil {
		return err
	}

	start := time.Now()
	rows := 0
	if err := db.Scan(nil, nil, func(_, _ []byte) bool { rows++; return true }); err != nil {
		return err
	}
	elapsed := time.Since(start)
	fmt.Printf("| Full scan | %d | %.0f | - | - |\n", rows, float64(rows)/elapsed.Seconds())

	err = measure("Durable write (sync every write)", durableOps, func(i int) error {
		if err := db.Put(key(n+i), value); err != nil {
			return err
		}
		return db.Sync()
	})
	if err != nil {
		return err
	}

	checked, err := db.Check()
	if err != nil {
		return err
	}
	height, err := db.Height()
	if err != nil {
		return err
	}
	if err := db.Sync(); err != nil {
		return err
	}
	info, err := os.Stat(path)
	if err != nil {
		return err
	}
	fmt.Printf("\nTree check passed: %d keys, height %d, file size %.1f MB\n",
		checked, height, float64(info.Size())/(1024*1024))
	return nil
}

func load(path, name string, order []int) error {
	db, err := minidb.Open(path)
	if err != nil {
		return err
	}
	err = measure(name, len(order), func(i int) error {
		if err := db.Put(key(order[i]), value); err != nil {
			return err
		}
		if (i+1)%syncEvery == 0 {
			return db.Sync()
		}
		return nil
	})
	if closeErr := db.Close(); err == nil {
		err = closeErr
	}
	return err
}

func measure(name string, count int, op func(int) error) error {
	times := make([]time.Duration, count)
	start := time.Now()
	for i := 0; i < count; i++ {
		before := time.Now()
		if err := op(i); err != nil {
			return fmt.Errorf("%s: %w", name, err)
		}
		times[i] = time.Since(before)
	}
	elapsed := time.Since(start)
	slices.Sort(times)
	fmt.Printf("| %s | %d | %.0f | %s | %s |\n", name, count, float64(count)/elapsed.Seconds(),
		micros(times[count/2]), micros(times[count*99/100]))
	return nil
}

// micros uses plain "us" because some Windows terminals garble the micro sign.
func micros(d time.Duration) string {
	return fmt.Sprintf("%.1f us", float64(d.Nanoseconds())/1000)
}
