package auth

import (
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

const (
	secret = "test-secret-that-is-at-least-32-bytes-long"
	other  = "another-secret-that-is-also-32-bytes-long!"
)

func courierVerifier(t *testing.T) *Verifier {
	t.Helper()
	v, err := NewVerifier(secret, RoleCourier, events.PrefixCourier)
	if err != nil {
		t.Fatal(err)
	}
	return v
}

func issue(t *testing.T, key, role, sub string, exp time.Time) string {
	t.Helper()
	tok, err := Issue(key, role, sub, exp)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestVerifyAcceptsAValidToken(t *testing.T) {
	courier := events.NewID(events.PrefixCourier)
	exp := time.Now().Add(time.Hour).Truncate(time.Second)
	got, err := courierVerifier(t).Verify(issue(t, secret, RoleCourier, courier, exp))
	if err != nil {
		t.Fatal(err)
	}
	if got.Subject != courier || !got.ExpiresAt.Equal(exp) {
		t.Fatalf("got %+v", got)
	}
}

func TestVerifyRejects(t *testing.T) {
	courier := events.NewID(events.PrefixCourier)
	later := time.Now().Add(time.Hour)
	signWith := func(method jwt.SigningMethod, key any, claims jwt.Claims) string {
		s, err := jwt.NewWithClaims(method, claims).SignedString(key)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	valid := claims{Role: RoleCourier, RegisteredClaims: jwt.RegisteredClaims{Subject: courier, ExpiresAt: jwt.NewNumericDate(later)}}
	noExp := claims{Role: RoleCourier, RegisteredClaims: jwt.RegisteredClaims{Subject: courier}}
	cases := map[string]string{
		"garbage":       "not.a.token",
		"wrong secret":  issue(t, other, RoleCourier, courier, later),
		"expired":       issue(t, secret, RoleCourier, courier, time.Now().Add(-time.Minute)),
		"wrong role":    issue(t, secret, RoleTracking, courier, later),
		"wrong subject": issue(t, secret, RoleCourier, events.NewID(events.PrefixOrder), later),
		"HS384":         signWith(jwt.SigningMethodHS384, []byte(secret), valid),
		"alg none":      signWith(jwt.SigningMethodNone, jwt.UnsafeAllowNoneSignatureType, valid),
		"no exp":        signWith(jwt.SigningMethodHS256, []byte(secret), noExp),
	}
	v := courierVerifier(t)
	for name, tok := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := v.Verify(tok); !errors.Is(err, ErrInvalidToken) {
				t.Fatalf("got %v, want ErrInvalidToken", err)
			}
		})
	}
}

func TestTrackingVerifier(t *testing.T) {
	v, err := NewVerifier(secret, RoleTracking, events.PrefixOrder)
	if err != nil {
		t.Fatal(err)
	}
	order := events.NewID(events.PrefixOrder)
	if got, err := v.Verify(issue(t, secret, RoleTracking, order, time.Now().Add(time.Minute))); err != nil || got.Subject != order {
		t.Fatalf("got %+v, %v", got, err)
	}
	if _, err := v.Verify(issue(t, secret, RoleCourier, order, time.Now().Add(time.Minute))); err == nil {
		t.Fatal("a courier token must not open a tracking stream")
	}
}

func TestNewVerifierRejectsShortSecrets(t *testing.T) {
	if _, err := NewVerifier(strings.Repeat("x", MinSecretBytes-1), RoleCourier, events.PrefixCourier); err == nil {
		t.Fatal("expected an error for a short secret")
	}
}

func TestBearerToken(t *testing.T) {
	cases := map[string]struct {
		header string
		want   string
		ok     bool
	}{
		"bearer":       {"Bearer abc.def.ghi", "abc.def.ghi", true},
		"lower case":   {"bearer abc", "abc", true},
		"basic":        {"Basic dXNlcjpwYXNz", "", false},
		"missing":      {"", "", false},
		"empty token":  {"Bearer   ", "", false},
		"no separator": {"Bearerabc", "", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest("GET", "/", nil)
			if tc.header != "" {
				r.Header.Set("Authorization", tc.header)
			}
			got, ok := BearerToken(r)
			if got != tc.want || ok != tc.ok {
				t.Fatalf("got %q, %v", got, ok)
			}
		})
	}
}
