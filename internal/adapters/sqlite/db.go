// Package sqlite implements the storage ports on top of a SQLite database
// file, using a SQLite driver written in pure Go so the server builds into a
// single static binary with no C toolchain.
//
// Reading order: db.go (opening and schema), keyenc.go (how values become
// sortable index keys), store.go (reads and writes), plan.go (how a query is
// turned into index scans), users.go (accounts).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"runtime"

	_ "modernc.org/sqlite" // registers the "sqlite" driver with database/sql
)

// DB is an open database file. It implements app.Store; Users returns the
// app.UserStore that shares the same file.
type DB struct {
	// SQLite allows many readers but only one writer at a time. Giving
	// writes their own handle limited to one connection turns that rule
	// into ordinary queueing inside database/sql, instead of "database is
	// locked" errors under load.
	write *sql.DB
	read  *sql.DB
}

// migrations holds the schema as a list of steps. The database remembers
// how many steps it has applied (in SQLite's user_version field), so opening
// an older file runs just the steps it is missing. To change the schema,
// append a step; never edit one that has shipped.
var migrations = []string{
	`CREATE TABLE meta (
		key   TEXT PRIMARY KEY,
		value INTEGER NOT NULL
	) WITHOUT ROWID;

	-- One row per document. Rows of one collection are stored next to each
	-- other, ordered by ID, because the primary key starts with the parent.
	CREATE TABLE docs (
		parent      TEXT    NOT NULL,
		id          TEXT    NOT NULL,
		data        BLOB    NOT NULL,
		create_time INTEGER NOT NULL,
		update_time INTEGER NOT NULL,
		version     INTEGER NOT NULL,
		PRIMARY KEY (parent, id)
	) WITHOUT ROWID;

	-- One row per indexed value. kind 0 is the value of the field itself;
	-- kind 1 is one element of an array field, for array-contains queries.
	-- The primary key is the index: all entries of one field of one
	-- collection are adjacent and sorted by value, then by document ID.
	CREATE TABLE idx (
		parent TEXT    NOT NULL,
		field  TEXT    NOT NULL,
		kind   INTEGER NOT NULL,
		key    BLOB    NOT NULL,
		id     TEXT    NOT NULL,
		PRIMARY KEY (parent, field, kind, key, id)
	) WITHOUT ROWID;

	-- Lets a write find and remove a document's old index entries.
	CREATE INDEX idx_by_doc ON idx (parent, id);

	CREATE TABLE users (
		id            TEXT PRIMARY KEY,
		email         TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at    INTEGER NOT NULL
	) WITHOUT ROWID;

	CREATE TABLE refresh_tokens (
		hash       TEXT PRIMARY KEY,
		user_id    TEXT NOT NULL,
		expires_at INTEGER NOT NULL
	) WITHOUT ROWID;`,
}

// Open opens the database file at path, creating it and bringing its schema
// up to date as needed.
func Open(ctx context.Context, path string) (*DB, error) {
	// Create the file ourselves so that it is private to the server's user
	// from the start: it holds password hashes and refresh-token hashes.
	// SQLite gives its companion files (-wal, -shm) the same permissions.
	if err := touchPrivate(path); err != nil {
		return nil, fmt.Errorf("creating %s: %w", path, err)
	}
	// Options common to every connection. WAL journaling lets readers
	// proceed while a write is in progress; busy_timeout makes a connection
	// wait for a lock instead of failing at once.
	dsn := func(extra string) string {
		return "file:" + (&url.URL{Path: path}).EscapedPath() +
			"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)" + extra
	}

	write, err := sql.Open("sqlite", dsn("&_txlock=immediate"))
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	write.SetMaxOpenConns(1)

	db := &DB{write: write}
	if err := db.migrate(ctx); err != nil {
		write.Close()
		return nil, fmt.Errorf("preparing %s: %w", path, err)
	}

	read, err := sql.Open("sqlite", dsn("&_pragma=query_only(1)"))
	if err != nil {
		write.Close()
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	read.SetMaxOpenConns(max(4, runtime.NumCPU()))
	db.read = read
	return db, nil
}

func (db *DB) migrate(ctx context.Context) error {
	var applied int
	if err := db.write.QueryRowContext(ctx, "PRAGMA user_version").Scan(&applied); err != nil {
		return err
	}
	if applied > len(migrations) {
		return fmt.Errorf("database schema version %d is newer than this program supports (%d)", applied, len(migrations))
	}
	for i := applied; i < len(migrations); i++ {
		err := db.inTx(ctx, func(tx *sql.Tx) error {
			if _, err := tx.ExecContext(ctx, migrations[i]); err != nil {
				return err
			}
			// PRAGMA does not accept bound parameters; i is our own
			// integer, so formatting it into the statement is safe.
			_, err := tx.ExecContext(ctx, fmt.Sprintf("PRAGMA user_version = %d", i+1))
			return err
		})
		if err != nil {
			return fmt.Errorf("schema migration %d: %w", i+1, err)
		}
	}
	return nil
}

// inTx runs fn inside a write transaction, committing if fn returns nil and
// rolling back otherwise. Wrapping the begin/commit/rollback dance in one
// helper means no caller can forget a rollback on an error path.
func (db *DB) inTx(ctx context.Context, fn func(tx *sql.Tx) error) error {
	tx, err := db.write.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := fn(tx); err != nil {
		// Join keeps both errors if the rollback fails too.
		return errors.Join(err, tx.Rollback())
	}
	return tx.Commit()
}

// Close releases the database file.
func (db *DB) Close() error {
	return errors.Join(db.read.Close(), db.write.Close())
}

// Backup writes a consistent copy of the database to dest, which must not
// exist yet. It is safe to run while the server is serving requests.
func (db *DB) Backup(ctx context.Context, dest string) error {
	if _, err := db.write.ExecContext(ctx, "VACUUM INTO ?", dest); err != nil {
		return err
	}
	// The copy holds the same secrets as the original.
	return os.Chmod(dest, 0o600)
}

// touchPrivate creates path with owner-only permissions if it does not
// exist. An existing file is left as it is.
func touchPrivate(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return err
	}
	return f.Close()
}
