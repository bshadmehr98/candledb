package jwtauth_test

import (
	"encoding/base64"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/bshadmehr98/candledb/internal/adapters/jwtauth"
	"github.com/bshadmehr98/candledb/internal/app"
	"github.com/bshadmehr98/candledb/internal/domain"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

const secret = "0123456789abcdef0123456789abcdef"

func TestIssueAndVerify(t *testing.T) {
	c := &clock{now: time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)}
	m, err := jwtauth.New([]byte(secret), "test", c)
	if err != nil {
		t.Fatal(err)
	}
	expires := c.now.Add(time.Hour)
	token, err := m.Issue(app.Claims{UID: "u1", Email: "ada@example.com", Expires: expires})
	if err != nil {
		t.Fatal(err)
	}

	claims, err := m.Verify(token)
	if err != nil {
		t.Fatal(err)
	}
	if claims.UID != "u1" || claims.Email != "ada@example.com" || !claims.Expires.Equal(expires) {
		t.Errorf("claims = %+v", claims)
	}

	c.now = c.now.Add(59 * time.Minute)
	if _, err := m.Verify(token); err != nil {
		t.Errorf("token rejected before its expiry: %v", err)
	}
	c.now = c.now.Add(2 * time.Minute)
	if _, err := m.Verify(token); !errors.Is(err, domain.ErrUnauthenticated) {
		t.Errorf("expired token: %v", err)
	}
}

func TestVerifyRejectsForgeries(t *testing.T) {
	c := &clock{now: time.Now()}
	m, _ := jwtauth.New([]byte(secret), "test", c)
	other, _ := jwtauth.New([]byte("another-secret-of-sufficient-length!"), "test", c)

	good, _ := m.Issue(app.Claims{UID: "u1", Expires: c.now.Add(time.Hour)})
	foreign, _ := other.Issue(app.Claims{UID: "u1", Expires: c.now.Add(time.Hour)})
	noSubject, _ := m.Issue(app.Claims{Expires: c.now.Add(time.Hour)})

	// The classic attack: a token whose header says "no signature needed".
	enc := base64.RawURLEncoding.EncodeToString
	unsigned := enc([]byte(`{"alg":"none","typ":"JWT"}`)) + "." + enc([]byte(`{"sub":"admin","exp":9999999999}`)) + "."

	parts := strings.Split(good, ".")
	tamperedPayload := parts[0] + "." + enc([]byte(`{"sub":"someone-else","exp":9999999999}`)) + "." + parts[2]

	for name, token := range map[string]string{
		"signed with another secret": foreign,
		"alg none":                   unsigned,
		"payload swapped":            tamperedPayload,
		"no subject":                 noSubject,
		"garbage":                    "abc.def.ghi",
		"empty":                      "",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := m.Verify(token); !errors.Is(err, domain.ErrUnauthenticated) {
				t.Errorf("Verify = %v, want ErrUnauthenticated", err)
			}
		})
	}
}

func TestShortSecretIsRefused(t *testing.T) {
	if _, err := jwtauth.New([]byte("too short"), "test", &clock{}); err == nil {
		t.Error("New accepted a 9-byte secret")
	}
}
