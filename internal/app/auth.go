package app

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/bshadmehr98/candledb/internal/domain"
)

// AuthConfig tunes the account service.
type AuthConfig struct {
	AccessTTL  time.Duration // lifetime of an access token
	RefreshTTL time.Duration // lifetime of a refresh token
	// DisableSignup turns off self-registration; existing users can still
	// sign in.
	DisableSignup bool
	// AdminKey, when not empty, is a secret that grants admin access (which
	// bypasses security rules) to a request presenting it as bearer token.
	AdminKey string
}

// DefaultAuthConfig is used for fields left at their zero value.
var DefaultAuthConfig = AuthConfig{
	AccessTTL:  time.Hour,
	RefreshTTL: 30 * 24 * time.Hour,
}

// AuthSession is what a successful sign-in returns to the client.
type AuthSession struct {
	User         User
	AccessToken  string
	RefreshToken string
	ExpiresAt    time.Time
}

// AuthService implements accounts: sign-up, sign-in, token refresh and the
// verification of bearer tokens on incoming requests.
type AuthService struct {
	users  UserStore
	hasher PasswordHasher
	tokens TokenManager
	clock  Clock
	ids    IDGenerator
	cfg    AuthConfig
	// dummyHash is compared against when a sign-in names an unknown email,
	// so that the response takes as long as it does for a known one.
	dummyHash string
}

// NewAuthService wires the account use cases to their ports.
func NewAuthService(users UserStore, hasher PasswordHasher, tokens TokenManager, clock Clock, cfg AuthConfig) (*AuthService, error) {
	if cfg.AccessTTL <= 0 {
		cfg.AccessTTL = DefaultAuthConfig.AccessTTL
	}
	if cfg.RefreshTTL <= 0 {
		cfg.RefreshTTL = DefaultAuthConfig.RefreshTTL
	}
	dummy, err := hasher.Hash("placeholder password")
	if err != nil {
		return nil, fmt.Errorf("preparing password hasher: %w", err)
	}
	return &AuthService{
		users: users, hasher: hasher, tokens: tokens, clock: clock,
		ids: RandomIDs{}, cfg: cfg, dummyHash: dummy,
	}, nil
}

var errBadCredentials = fmt.Errorf("%w: invalid email or password", domain.ErrUnauthenticated)

// SignUp creates an account and signs it in.
func (a *AuthService) SignUp(ctx context.Context, email, password string) (AuthSession, error) {
	if a.cfg.DisableSignup {
		return AuthSession{}, fmt.Errorf("%w: sign-up is disabled", domain.ErrPermissionDenied)
	}
	email, err := normalizeEmail(email)
	if err != nil {
		return AuthSession{}, err
	}
	// bcrypt, the default hasher, silently ignores everything after 72
	// bytes, so longer passwords are refused instead of being weakened.
	if len(password) < 8 || len(password) > 72 {
		return AuthSession{}, fmt.Errorf("%w: password must be 8 to 72 bytes long", domain.ErrInvalidArgument)
	}
	hash, err := a.hasher.Hash(password)
	if err != nil {
		return AuthSession{}, fmt.Errorf("hashing password: %w", err)
	}
	user := User{ID: a.ids.NewID(), Email: email, PasswordHash: hash, CreatedAt: a.clock.Now().UTC()}
	if err := a.users.CreateUser(ctx, user); err != nil {
		return AuthSession{}, err
	}
	return a.startSession(ctx, user)
}

// SignIn checks an email and password.
//
// An unknown email and a wrong password produce the same error after the
// same amount of work, so the response does not reveal which emails have
// accounts.
func (a *AuthService) SignIn(ctx context.Context, email, password string) (AuthSession, error) {
	email, err := normalizeEmail(email)
	if err != nil {
		return AuthSession{}, errBadCredentials
	}
	user, err := a.users.UserByEmail(ctx, email)
	if errors.Is(err, domain.ErrNotFound) {
		a.hasher.Verify(a.dummyHash, password)
		return AuthSession{}, errBadCredentials
	}
	if err != nil {
		return AuthSession{}, err
	}
	if !a.hasher.Verify(user.PasswordHash, password) {
		return AuthSession{}, errBadCredentials
	}
	return a.startSession(ctx, user)
}

// Refresh exchanges a refresh token for a new access token and a new
// refresh token. The old refresh token stops working (rotation), which
// limits how long a stolen one is useful.
func (a *AuthService) Refresh(ctx context.Context, refreshToken string) (AuthSession, error) {
	invalid := fmt.Errorf("%w: invalid refresh token", domain.ErrUnauthenticated)
	stored, err := a.users.TakeRefreshToken(ctx, hashToken(refreshToken))
	if errors.Is(err, domain.ErrNotFound) {
		return AuthSession{}, invalid
	}
	if err != nil {
		return AuthSession{}, err
	}
	if !a.clock.Now().Before(stored.ExpiresAt) {
		return AuthSession{}, invalid
	}
	user, err := a.users.UserByID(ctx, stored.UserID)
	if errors.Is(err, domain.ErrNotFound) {
		return AuthSession{}, invalid
	}
	if err != nil {
		return AuthSession{}, err
	}
	return a.startSession(ctx, user)
}

// SignOut revokes a refresh token. Signing out with an unknown token is not
// an error: the caller's goal, that the token no longer works, is met.
func (a *AuthService) SignOut(ctx context.Context, refreshToken string) error {
	_, err := a.users.TakeRefreshToken(ctx, hashToken(refreshToken))
	if errors.Is(err, domain.ErrNotFound) {
		return nil
	}
	return err
}

// Authenticate turns the bearer token of a request into a Principal. An
// empty token is an anonymous caller, which is not an error: whether
// anonymous callers may do anything is for the security rules to say.
func (a *AuthService) Authenticate(bearer string) (Principal, error) {
	if bearer == "" {
		return Principal{}, nil
	}
	// ConstantTimeCompare takes the same time whether the first or the last
	// byte differs, so the key cannot be guessed by timing the response.
	if a.cfg.AdminKey != "" && subtle.ConstantTimeCompare([]byte(bearer), []byte(a.cfg.AdminKey)) == 1 {
		return Principal{UID: "admin", Admin: true}, nil
	}
	claims, err := a.tokens.Verify(bearer)
	if err != nil {
		return Principal{}, err
	}
	token := domain.Map{}
	for k, v := range claims.Extra {
		token[k] = v
	}
	if claims.Email != "" {
		token["email"] = domain.String(claims.Email)
	}
	return Principal{UID: claims.UID, Claims: token, Expires: claims.Expires}, nil
}

// User returns the account of a signed-in principal.
func (a *AuthService) User(ctx context.Context, p Principal) (User, error) {
	if p.UID == "" || p.Admin {
		return User{}, fmt.Errorf("%w: not signed in", domain.ErrUnauthenticated)
	}
	return a.users.UserByID(ctx, p.UID)
}

func (a *AuthService) startSession(ctx context.Context, user User) (AuthSession, error) {
	now := a.clock.Now()
	expires := now.Add(a.cfg.AccessTTL)
	access, err := a.tokens.Issue(Claims{UID: user.ID, Email: user.Email, Expires: expires})
	if err != nil {
		return AuthSession{}, fmt.Errorf("issuing access token: %w", err)
	}
	refresh := rand.Text() + rand.Text()
	err = a.users.PutRefreshToken(ctx, RefreshToken{
		Hash:      hashToken(refresh),
		UserID:    user.ID,
		ExpiresAt: now.Add(a.cfg.RefreshTTL),
	})
	if err != nil {
		return AuthSession{}, fmt.Errorf("storing refresh token: %w", err)
	}
	return AuthSession{User: user, AccessToken: access, RefreshToken: refresh, ExpiresAt: expires}, nil
}

// hashToken returns the SHA-256 of a refresh token. A fast hash is right
// here, unlike for passwords: the token is 260 random bits, so it cannot be
// guessed no matter how quickly guesses can be checked.
func hashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func normalizeEmail(email string) (string, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	at := strings.LastIndexByte(email, '@')
	if at <= 0 || at == len(email)-1 || len(email) > 254 || strings.ContainsAny(email, " \t\r\n") {
		return "", fmt.Errorf("%w: %q is not an email address", domain.ErrInvalidArgument, email)
	}
	return email, nil
}
