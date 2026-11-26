package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

var _ app.Store = (*DB)(nil)

const docColumns = "id, data, create_time, update_time, version"

// scanner is satisfied by both *sql.Row and *sql.Rows, so one function can
// decode a document from either.
type scanner interface {
	Scan(dest ...any) error
}

func scanDoc(row scanner, parent domain.CollectionPath) (domain.Document, error) {
	var (
		id               string
		raw              []byte
		created, updated int64
		doc              domain.Document
	)
	if err := row.Scan(&id, &raw, &created, &updated, &doc.Version); err != nil {
		return doc, err
	}
	data, err := domain.ParseMapJSON(raw)
	if err != nil {
		return doc, fmt.Errorf("stored document %s/%s is corrupt: %w", parent, id, err)
	}
	doc.Path = domain.DocPath(string(parent) + "/" + id)
	doc.Data = data
	doc.CreateTime = time.UnixMicro(created).UTC()
	doc.UpdateTime = time.UnixMicro(updated).UTC()
	return doc, nil
}

func (db *DB) Get(ctx context.Context, path domain.DocPath) (domain.Document, error) {
	parent := path.Parent()
	row := db.read.QueryRowContext(ctx,
		"SELECT "+docColumns+" FROM docs WHERE parent = ? AND id = ?", string(parent), path.ID())
	doc, err := scanDoc(row, parent)
	if errors.Is(err, sql.ErrNoRows) {
		return doc, fmt.Errorf("%w: document %s", domain.ErrNotFound, path)
	}
	return doc, err
}

func (db *DB) Version(ctx context.Context) (int64, error) {
	var version int64
	err := db.read.QueryRowContext(ctx, "SELECT value FROM meta WHERE key = 'version'").Scan(&version)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, nil
	}
	return version, err
}

// Apply stores a commit. The documents, their index entries and the new
// database version are written in one transaction, so a crash at any point
// leaves either the whole commit or none of it.
func (db *DB) Apply(ctx context.Context, version int64, batch []app.Mutation) error {
	return db.inTx(ctx, func(tx *sql.Tx) error {
		// A commit inserts many index rows, so that statement is compiled
		// once and reused.
		insertIndex, err := tx.PrepareContext(ctx,
			"INSERT OR IGNORE INTO idx (parent, field, kind, key, id) VALUES (?, ?, ?, ?, ?)")
		if err != nil {
			return err
		}
		defer insertIndex.Close()

		for _, m := range batch {
			parent, id := string(m.Path.Parent()), m.Path.ID()
			if _, err := tx.ExecContext(ctx, "DELETE FROM idx WHERE parent = ? AND id = ?", parent, id); err != nil {
				return err
			}
			if m.Doc == nil {
				if _, err := tx.ExecContext(ctx, "DELETE FROM docs WHERE parent = ? AND id = ?", parent, id); err != nil {
					return err
				}
				continue
			}
			data, err := json.Marshal(m.Doc.Data)
			if err != nil {
				return fmt.Errorf("encoding document %s: %w", m.Path, err)
			}
			_, err = tx.ExecContext(ctx,
				`INSERT INTO docs (parent, id, data, create_time, update_time, version)
				 VALUES (?, ?, ?, ?, ?, ?)
				 ON CONFLICT (parent, id) DO UPDATE SET
				   data = excluded.data, create_time = excluded.create_time,
				   update_time = excluded.update_time, version = excluded.version`,
				parent, id, data, m.Doc.CreateTime.UnixMicro(), m.Doc.UpdateTime.UnixMicro(), m.Doc.Version)
			if err != nil {
				return err
			}
			for _, e := range indexEntries(m.Doc.Data) {
				if _, err := insertIndex.ExecContext(ctx, parent, e.field, e.kind, e.key, id); err != nil {
					return err
				}
			}
		}
		_, err = tx.ExecContext(ctx,
			`INSERT INTO meta (key, value) VALUES ('version', ?)
			 ON CONFLICT (key) DO UPDATE SET value = excluded.value`, version)
		return err
	})
}

// Index entry kinds, stored in the idx.kind column.
const (
	kindValue   = 0 // the value of the field
	kindElement = 1 // one element of an array field
)

type indexEntry struct {
	field string
	kind  int
	key   []byte
}

// indexEntries lists every index row a document needs: one per field at
// every nesting level ("address" and "address.city"), plus one per element
// of every array field.
func indexEntries(data domain.Map) []indexEntry {
	var entries []indexEntry
	var visit func(field string, v domain.Value)
	visit = func(field string, v domain.Value) {
		entries = append(entries, indexEntry{field, kindValue, encodeKey(v)})
		switch x := v.(type) {
		case domain.Array:
			for _, e := range x {
				entries = append(entries, indexEntry{field, kindElement, encodeKey(e)})
			}
		case domain.Map:
			for name, child := range x {
				visit(field+"."+name, child)
			}
		}
	}
	for name, v := range data {
		visit(name, v)
	}
	return entries
}
