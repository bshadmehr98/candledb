package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"modernc.org/sqlite"
	sqlite3 "modernc.org/sqlite/lib"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// UserStore is the app.UserStore backed by the same database file.
type UserStore struct{ db *DB }

var _ app.UserStore = (*UserStore)(nil)

// Users returns the account store that shares this database.
func (db *DB) Users() *UserStore { return &UserStore{db: db} }

func (s *UserStore) CreateUser(ctx context.Context, u app.User) error {
	_, err := s.db.write.ExecContext(ctx,
		"INSERT INTO users (id, email, password_hash, created_at) VALUES (?, ?, ?, ?)",
		u.ID, u.Email, u.PasswordHash, u.CreatedAt.UnixMicro())
	// errors.As digs the driver's own error type out of the chain, which
	// carries SQLite's numeric result code. Checking the code is sturdier
	// than looking for words in the message.
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code() == sqlite3.SQLITE_CONSTRAINT_UNIQUE {
		return fmt.Errorf("%w: an account with this email", domain.ErrAlreadyExists)
	}
	return err
}

func (s *UserStore) UserByEmail(ctx context.Context, email string) (app.User, error) {
	return s.user(ctx, "email = ?", email)
}

func (s *UserStore) UserByID(ctx context.Context, id string) (app.User, error) {
	return s.user(ctx, "id = ?", id)
}

// user runs the one user query with the given condition. where is always
// one of the two constants above, never caller input, so building the SQL by
// concatenation is safe here.
func (s *UserStore) user(ctx context.Context, where string, arg any) (app.User, error) {
	var (
		u       app.User
		created int64
	)
	err := s.db.read.QueryRowContext(ctx,
		"SELECT id, email, password_hash, created_at FROM users WHERE "+where, arg).
		Scan(&u.ID, &u.Email, &u.PasswordHash, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return u, fmt.Errorf("%w: user", domain.ErrNotFound)
	}
	u.CreatedAt = time.UnixMicro(created).UTC()
	return u, err
}

func (s *UserStore) PutRefreshToken(ctx context.Context, t app.RefreshToken) error {
	now := time.Now().UnixMicro()
	return s.db.inTx(ctx, func(tx *sql.Tx) error {
		// Expired tokens are swept whenever a new one is stored, which
		// keeps the table from growing without a background job.
		if _, err := tx.ExecContext(ctx, "DELETE FROM refresh_tokens WHERE expires_at < ?", now); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx,
			"INSERT INTO refresh_tokens (hash, user_id, expires_at) VALUES (?, ?, ?)",
			t.Hash, t.UserID, t.ExpiresAt.UnixMicro())
		return err
	})
}

func (s *UserStore) TakeRefreshToken(ctx context.Context, hash string) (app.RefreshToken, error) {
	t := app.RefreshToken{Hash: hash}
	var expires int64
	// DELETE ... RETURNING removes the row and hands it back in a single
	// statement, so two requests racing with the same token cannot both
	// receive it.
	err := s.db.write.QueryRowContext(ctx,
		"DELETE FROM refresh_tokens WHERE hash = ? RETURNING user_id, expires_at", hash).
		Scan(&t.UserID, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return t, fmt.Errorf("%w: refresh token", domain.ErrNotFound)
	}
	t.ExpiresAt = time.UnixMicro(expires).UTC()
	return t, err
}
