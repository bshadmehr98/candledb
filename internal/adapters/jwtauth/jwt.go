// Package jwtauth implements app.TokenManager with JSON Web Tokens signed
// using HMAC-SHA256 (the "HS256" algorithm).
package jwtauth

import (
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/golang-jwt/jwt/v5"

	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// MinSecretBytes is the shortest signing secret accepted. HS256 is only as
// strong as its key, and a short key can be found by brute force from a
// single captured token.
const MinSecretBytes = 32

// Manager signs and verifies access tokens with one shared secret.
type Manager struct {
	secret []byte
	issuer string
	clock  app.Clock
	parser *jwt.Parser
}

var _ app.TokenManager = (*Manager)(nil)

// New returns a Manager. The issuer is written into every token, and the
// clock decides when a token counts as expired.
func New(secret []byte, issuer string, clock app.Clock) (*Manager, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("jwt secret must be at least %d bytes, got %d", MinSecretBytes, len(secret))
	}
	return &Manager{
		secret: secret,
		issuer: issuer,
		clock:  clock,
		// Accepting only HS256 closes the classic JWT attack in which a
		// forged token names a different algorithm (or "none") and the
		// verifier trusts it.
		parser: jwt.NewParser(jwt.WithValidMethods([]string{"HS256"}), jwt.WithExpirationRequired(), jwt.WithTimeFunc(clock.Now)),
	}, nil
}

func (m *Manager) Issue(c app.Claims) (string, error) {
	claims := jwt.MapClaims{
		"sub": c.UID,
		"iss": m.issuer,
		"iat": m.clock.Now().Unix(),
		"exp": c.Expires.Unix(),
		// A random ID makes every token unique, even two issued to the same
		// user within the same second.
		"jti": rand.Text(),
	}
	if c.Email != "" {
		claims["email"] = c.Email
	}
	return jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(m.secret)
}

func (m *Manager) Verify(token string) (app.Claims, error) {
	claims := jwt.MapClaims{}
	_, err := m.parser.ParseWithClaims(token, claims, func(*jwt.Token) (any, error) { return m.secret, nil })
	if err != nil {
		if errors.Is(err, jwt.ErrTokenExpired) {
			return app.Claims{}, fmt.Errorf("%w: access token has expired", domain.ErrUnauthenticated)
		}
		return app.Claims{}, fmt.Errorf("%w: invalid access token", domain.ErrUnauthenticated)
	}
	uid, _ := claims["sub"].(string)
	if uid == "" {
		return app.Claims{}, fmt.Errorf("%w: access token has no subject", domain.ErrUnauthenticated)
	}
	out := app.Claims{UID: uid, Extra: domain.Map{}}
	if exp, err := claims.GetExpirationTime(); err == nil && exp != nil {
		out.Expires = exp.Time
	}
	out.Email, _ = claims["email"].(string)

	// Pass any other claims through to the security rules. The registered
	// claims are bookkeeping and are left out.
	for name, raw := range claims {
		switch name {
		case "sub", "iss", "iat", "exp", "nbf", "aud", "jti", "email":
			continue
		}
		if domain.ValidateFieldName(name) != nil {
			continue
		}
		encoded, err := json.Marshal(raw)
		if err != nil {
			continue
		}
		if v, err := domain.ParseJSON(encoded); err == nil {
			out.Extra[name] = v
		}
	}
	return out, nil
}
