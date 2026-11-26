// Package bcrypthash implements app.PasswordHasher with bcrypt.
package bcrypthash

import (
	"golang.org/x/crypto/bcrypt"

	"github.com/bshadmehr98/candledb/internal/app"
)

// Hasher hashes passwords with bcrypt at a configurable cost. Each step up
// in cost doubles the time one hash takes, for the server and for an
// attacker trying guesses against a stolen database alike.
type Hasher struct{ cost int }

var _ app.PasswordHasher = Hasher{}

// New returns a Hasher. A cost of 0 selects bcrypt's default; tests pass
// bcrypt.MinCost to stay fast.
func New(cost int) Hasher {
	if cost == 0 {
		cost = bcrypt.DefaultCost
	}
	return Hasher{cost: cost}
}

func (h Hasher) Hash(password string) (string, error) {
	hash, err := bcrypt.GenerateFromPassword([]byte(password), h.cost)
	return string(hash), err
}

func (h Hasher) Verify(hash, password string) bool {
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil
}
