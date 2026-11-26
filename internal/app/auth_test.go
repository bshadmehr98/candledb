package app_test

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/adapters/memory"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

// The auth tests replace two more ports with trivial fakes. Real bcrypt is
// deliberately slow and real tokens are opaque; the fakes are instant and
// readable, and the real adapters have their own tests.

type plainHasher struct{ verifications int }

func (h *plainHasher) Hash(password string) (string, error) { return "hashed:" + password, nil }
func (h *plainHasher) Verify(hash, password string) bool {
	h.verifications++
	return hash == "hashed:"+password
}

// fakeTokens "signs" a token by writing the claims out in plain text.
type fakeTokens struct{ clock app.Clock }

func (f fakeTokens) Issue(c app.Claims) (string, error) {
	return fmt.Sprintf("token|%s|%s|%d", c.UID, c.Email, c.Expires.Unix()), nil
}

func (f fakeTokens) Verify(token string) (app.Claims, error) {
	parts := strings.Split(token, "|")
	if len(parts) != 4 || parts[0] != "token" {
		return app.Claims{}, fmt.Errorf("%w: malformed token", domain.ErrUnauthenticated)
	}
	var unix int64
	fmt.Sscan(parts[3], &unix)
	expires := time.Unix(unix, 0)
	if !f.clock.Now().Before(expires) {
		return app.Claims{}, fmt.Errorf("%w: expired", domain.ErrUnauthenticated)
	}
	return app.Claims{UID: parts[1], Email: parts[2], Expires: expires}, nil
}

func newAuth(t *testing.T, cfg app.AuthConfig) (*app.AuthService, *fakeClock, *plainHasher) {
	t.Helper()
	clock := newClock()
	hasher := &plainHasher{}
	auth, err := app.NewAuthService(memory.NewUserStore(), hasher, fakeTokens{clock}, clock, cfg)
	if err != nil {
		t.Fatal(err)
	}
	return auth, clock, hasher
}

func TestSignUpAndSignIn(t *testing.T) {
	auth, _, _ := newAuth(t, app.AuthConfig{})

	created, err := auth.SignUp(ctx, "  Ada@Example.COM ", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if created.User.Email != "ada@example.com" {
		t.Errorf("email was not normalised: %q", created.User.Email)
	}
	if created.User.PasswordHash == "correct horse" || created.User.PasswordHash == "" {
		t.Error("the password was stored unhashed")
	}

	p, err := auth.Authenticate(created.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if p.UID != created.User.ID || p.Admin || p.Claims["email"] != domain.String("ada@example.com") {
		t.Errorf("principal = %+v", p)
	}

	signedIn, err := auth.SignIn(ctx, "ADA@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if signedIn.User.ID != created.User.ID {
		t.Error("sign-in returned a different user")
	}
	me, err := auth.User(ctx, p)
	if err != nil || me.Email != "ada@example.com" {
		t.Errorf("User = %+v, %v", me, err)
	}
}

func TestSignUpValidation(t *testing.T) {
	auth, _, _ := newAuth(t, app.AuthConfig{})
	if _, err := auth.SignUp(ctx, "ada@example.com", "long enough"); err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, email, password string
		want                  error
	}{
		{"duplicate email", "ADA@example.com", "long enough", domain.ErrAlreadyExists},
		{"no at sign", "ada.example.com", "long enough", domain.ErrInvalidArgument},
		{"nothing before the at", "@example.com", "long enough", domain.ErrInvalidArgument},
		{"nothing after the at", "ada@", "long enough", domain.ErrInvalidArgument},
		{"space inside", "a da@example.com", "long enough", domain.ErrInvalidArgument},
		{"short password", "bob@example.com", "short", domain.ErrInvalidArgument},
		{"password beyond bcrypt's reach", "bob@example.com", strings.Repeat("x", 73), domain.ErrInvalidArgument},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := auth.SignUp(ctx, tt.email, tt.password); !errors.Is(err, tt.want) {
				t.Errorf("error = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestSignInFailuresLookAlike(t *testing.T) {
	auth, _, hasher := newAuth(t, app.AuthConfig{})
	if _, err := auth.SignUp(ctx, "ada@example.com", "correct horse"); err != nil {
		t.Fatal(err)
	}

	_, wrongPassword := auth.SignIn(ctx, "ada@example.com", "wrong")
	before := hasher.verifications
	_, unknownUser := auth.SignIn(ctx, "nobody@example.com", "wrong")

	if !errors.Is(wrongPassword, domain.ErrUnauthenticated) || !errors.Is(unknownUser, domain.ErrUnauthenticated) {
		t.Fatalf("errors = %v / %v", wrongPassword, unknownUser)
	}
	if wrongPassword.Error() != unknownUser.Error() {
		t.Errorf("the two failures are distinguishable: %q vs %q", wrongPassword, unknownUser)
	}
	// An unknown email must still cost one hash comparison, or response
	// time would reveal which emails are registered.
	if hasher.verifications != before+1 {
		t.Error("sign-in with an unknown email skipped the password hash work")
	}
}

func TestRefreshRotatesTokens(t *testing.T) {
	auth, clock, _ := newAuth(t, app.AuthConfig{AccessTTL: time.Hour, RefreshTTL: 24 * time.Hour})
	first, err := auth.SignUp(ctx, "ada@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}

	clock.Advance(2 * time.Hour)
	if _, err := auth.Authenticate(first.AccessToken); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("expired access token: %v", err)
	}

	second, err := auth.Refresh(ctx, first.RefreshToken)
	if err != nil {
		t.Fatal(err)
	}
	if second.RefreshToken == first.RefreshToken {
		t.Error("refresh did not rotate the refresh token")
	}
	if _, err := auth.Authenticate(second.AccessToken); err != nil {
		t.Errorf("new access token rejected: %v", err)
	}
	if _, err := auth.Refresh(ctx, first.RefreshToken); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("a used refresh token worked again: %v", err)
	}

	clock.Advance(25 * time.Hour)
	if _, err := auth.Refresh(ctx, second.RefreshToken); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("expired refresh token: %v", err)
	}
	if _, err := auth.Refresh(ctx, "made up"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("unknown refresh token: %v", err)
	}
}

func TestSignOutRevokesRefreshToken(t *testing.T) {
	auth, _, _ := newAuth(t, app.AuthConfig{})
	sess, err := auth.SignUp(ctx, "ada@example.com", "correct horse")
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.SignOut(ctx, sess.RefreshToken); err != nil {
		t.Fatal(err)
	}
	if _, err := auth.Refresh(ctx, sess.RefreshToken); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("refresh after sign-out: %v", err)
	}
	if err := auth.SignOut(ctx, sess.RefreshToken); err != nil {
		t.Errorf("signing out twice: %v", err)
	}
}

func TestAuthenticate(t *testing.T) {
	const key = "an-admin-key-of-decent-length"
	auth, _, _ := newAuth(t, app.AuthConfig{AdminKey: key})

	if p, err := auth.Authenticate(""); err != nil || p.UID != "" || p.Admin {
		t.Errorf("no token: %+v, %v; want the anonymous principal", p, err)
	}
	if p, err := auth.Authenticate(key); err != nil || !p.Admin {
		t.Errorf("admin key: %+v, %v", p, err)
	}
	if _, err := auth.Authenticate(key + "x"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("wrong key: %v", err)
	}
	if _, err := auth.Authenticate("garbage"); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("garbage token: %v", err)
	}
	if _, err := auth.User(ctx, app.Principal{}); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("User for anonymous: %v", err)
	}

	// With no admin key configured, nothing grants admin access; in
	// particular an empty bearer token must not match an empty key.
	noKey, _, _ := newAuth(t, app.AuthConfig{})
	if p, _ := noKey.Authenticate(""); p.Admin {
		t.Error("empty token became admin when no admin key is set")
	}
}

func TestSignUpCanBeDisabled(t *testing.T) {
	auth, _, _ := newAuth(t, app.AuthConfig{DisableSignup: true})
	if _, err := auth.SignUp(ctx, "ada@example.com", "correct horse"); !errors.Is(err, domain.ErrPermissionDenied) {
		t.Errorf("sign-up while disabled: %v", err)
	}
}
