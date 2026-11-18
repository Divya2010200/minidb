// Command minidb is an interactive shell for a minidb database file.
package main

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"minidb"
)

const maxScanRows = 50

const help = `Commands:
  put <key> <value>      store a value (the value may contain spaces)
  get <key>              show a value
  del <key>              delete a key
  scan [start] [end]     list keys from start up to, but not including, end
  check                  verify the tree and count keys
  height                 show the number of tree levels
  help                   show this list
  exit                   save and quit`

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: minidb <database-file>")
		os.Exit(2)
	}
	db, err := minidb.Open(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
	runErr := run(db, os.Stdin, os.Stdout)
	if err := errors.Join(runErr, db.Close()); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(db *minidb.DB, in io.Reader, out io.Writer) error {
	fmt.Fprintln(out, "minidb shell. Type help for commands.")
	lines := bufio.NewScanner(in)
	for {
		fmt.Fprint(out, "minidb> ")
		if !lines.Scan() {
			fmt.Fprintln(out)
			return lines.Err()
		}
		line := strings.TrimSpace(lines.Text())
		if line == "" {
			continue
		}
		if line == "exit" || line == "quit" {
			return nil
		}
		if err := execute(db, line, out); err != nil {
			fmt.Fprintln(out, "error:", err)
		}
	}
}

func execute(db *minidb.DB, line string, out io.Writer) error {
	command, rest, _ := strings.Cut(line, " ")
	rest = strings.TrimSpace(rest)
	args := strings.Fields(rest)

	switch command {
	case "put":
		key, value, ok := strings.Cut(rest, " ")
		if !ok || key == "" {
			return errors.New("usage: put <key> <value>")
		}
		if err := db.Put([]byte(key), []byte(strings.TrimSpace(value))); err != nil {
			return err
		}
		if err := db.Sync(); err != nil {
			return err
		}
		fmt.Fprintln(out, "OK")

	case "get":
		if len(args) != 1 {
			return errors.New("usage: get <key>")
		}
		value, found, err := db.Get([]byte(args[0]))
		if err != nil {
			return err
		}
		if !found {
			fmt.Fprintln(out, "(not found)")
			return nil
		}
		fmt.Fprintln(out, string(value))

	case "del":
		if len(args) != 1 {
			return errors.New("usage: del <key>")
		}
		deleted, err := db.Delete([]byte(args[0]))
		if err != nil {
			return err
		}
		if err := db.Sync(); err != nil {
			return err
		}
		if deleted {
			fmt.Fprintln(out, "OK")
		} else {
			fmt.Fprintln(out, "(not found)")
		}

	case "scan":
		if len(args) > 2 {
			return errors.New("usage: scan [start] [end]")
		}
		var start, end []byte
		if len(args) > 0 {
			start = []byte(args[0])
		}
		if len(args) > 1 {
			end = []byte(args[1])
		}
		rows := 0
		err := db.Scan(start, end, func(key, value []byte) bool {
			if rows == maxScanRows {
				fmt.Fprintf(out, "(stopped after %d rows)\n", maxScanRows)
				return false
			}
			fmt.Fprintf(out, "%s = %s\n", key, value)
			rows++
			return true
		})
		if err != nil {
			return err
		}
		if rows == 0 {
			fmt.Fprintln(out, "(no keys)")
		}

	case "check":
		count, err := db.Check()
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "OK: %d keys\n", count)

	case "height":
		height, err := db.Height()
		if err != nil {
			return err
		}
		fmt.Fprintln(out, height)

	case "help":
		fmt.Fprintln(out, help)

	default:
		return fmt.Errorf("unknown command %q (type help)", command)
	}
	return nil
}
