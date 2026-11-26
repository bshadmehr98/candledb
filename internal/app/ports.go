package app

import (
	"context"
	"crypto/rand"
	"time"

	"github.com/bshadmehr98/candledb/internal/domain"
)

// Store is the port to document storage.
//
// The interface is declared here, next to the code that uses it, and not in
// the packages that implement it. That is the usual Go arrangement and it is
// what makes the dependency point inwards: adapters know about app, app
// does not know about adapters.
type Store interface {
	// Get returns the document at path, or an error wrapping
	// domain.ErrNotFound.
	Get(ctx context.Context, path domain.DocPath) (domain.Document, error)

	// Query returns the documents selected by q, in query order. keep, when
	// not nil, is an extra filter that is applied before the limit, so a
	// limited query still returns as many documents as it can.
	Query(ctx context.Context, q domain.Query, keep func(domain.Document) bool) ([]domain.Document, error)

	// Apply atomically stores a batch of mutations and records version as
	// the new version of the database. Either all of it happens or none.
	Apply(ctx context.Context, version int64, batch []Mutation) error

	// Version returns the version recorded by the last Apply, or 0.
	Version(ctx context.Context) (int64, error)

	Close() error
}

// Mutation is the final state of one document after a commit: the document
// to store, or nil to delete whatever is at Path.
type Mutation struct {
	Path domain.DocPath
	Doc  *domain.Document
}

// Clock tells the time. Production code uses the system clock; tests pass a
// clock they control so that timestamps and token expiry are predictable.
type Clock interface {
	Now() time.Time
}

// IDGenerator produces identifiers for new documents, users and sessions.
type IDGenerator interface {
	NewID() string
}

// SystemClock is the Clock backed by the operating system.
type SystemClock struct{}

func (SystemClock) Now() time.Time { return time.Now() }

// RandomIDs is the IDGenerator used in production. Each ID is 26 characters
// carrying 130 bits from the operating system's secure random source.
type RandomIDs struct{}

func (RandomIDs) NewID() string { return rand.Text() }

// User is an account of the built-in email and password authentication.
type User struct {
	ID           string
	Email        string
	PasswordHash string
	CreatedAt    time.Time
}

// RefreshToken is a long-lived credential that can be exchanged for a new
// access token. Only a hash of the token is stored, so a leaked database
// does not leak usable tokens.
type RefreshToken struct {
	Hash      string
	UserID    string
	ExpiresAt time.Time
}

// UserStore is the port to account storage.
type UserStore interface {
	// CreateUser stores a new user. It returns an error wrapping
	// domain.ErrAlreadyExists if the email is taken.
	CreateUser(ctx context.Context, u User) error
	// UserByEmail and UserByID return an error wrapping domain.ErrNotFound
	// when there is no such user.
	UserByEmail(ctx context.Context, email string) (User, error)
	UserByID(ctx context.Context, id string) (User, error)

	PutRefreshToken(ctx context.Context, t RefreshToken) error
	// TakeRefreshToken removes the token with the given hash and returns
	// it, as one atomic step, so a refresh token can be used only once.
	TakeRefreshToken(ctx context.Context, hash string) (RefreshToken, error)
}

// PasswordHasher is the port to a password hashing algorithm.
type PasswordHasher interface {
	Hash(password string) (string, error)
	// Verify reports whether password matches hash.
	Verify(hash, password string) bool
}

// Claims is the content of an access token.
type Claims struct {
	UID     string
	Email   string
	Expires time.Time
	// Extra holds any further claims found in the token. Rules can read
	// them as request.auth.token.<name>.
	Extra domain.Map
}

// TokenManager is the port to access-token signing and verification.
type TokenManager interface {
	Issue(c Claims) (string, error)
	// Verify returns an error wrapping domain.ErrUnauthenticated when the
	// token is malformed, forged or expired.
	Verify(token string) (Claims, error)
}
