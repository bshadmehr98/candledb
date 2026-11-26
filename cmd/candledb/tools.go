package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/bshadmehr98/candledb/internal/adapters/sqlite"
	"github.com/bshadmehr98/candledb/internal/rules"
)

// checkRules parses a rules file and reports whether it is valid. It is
// meant for CI and for checking a file before sending SIGHUP to a server.
func checkRules(args []string, stdout io.Writer) error {
	if len(args) != 1 {
		return errors.New("usage: candledb check-rules <file>")
	}
	src, err := os.ReadFile(args[0])
	if err != nil {
		return err
	}
	if _, err := rules.Parse(string(src)); err != nil {
		return fmt.Errorf("%s: %w", args[0], err)
	}
	fmt.Fprintf(stdout, "%s: ok\n", args[0])
	return nil
}

// backup copies the database to a new file. It can run while a server is
// using the same data directory.
func backup(ctx context.Context, args []string, stdout, stderr io.Writer) error {
	fs := flag.NewFlagSet("candledb backup", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dataDir := fs.String("data-dir", env("CANDLEDB_DATA_DIR", "./candledb-data"), "directory holding the database [CANDLEDB_DATA_DIR]")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: candledb backup [-data-dir dir] <destination file>")
	}
	source := filepath.Join(*dataDir, "candledb.db")
	if _, err := os.Stat(source); err != nil {
		return fmt.Errorf("no database to back up: %w", err)
	}
	if _, err := os.Stat(fs.Arg(0)); err == nil {
		return fmt.Errorf("%s already exists", fs.Arg(0))
	}
	db, err := sqlite.Open(ctx, source)
	if err != nil {
		return err
	}
	defer db.Close()
	if err := db.Backup(ctx, fs.Arg(0)); err != nil {
		return fmt.Errorf("backup failed: %w", err)
	}
	fmt.Fprintf(stdout, "backup written to %s\n", fs.Arg(0))
	return nil
}
