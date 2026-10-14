// Package auth verifies the two kinds of JWT the location path accepts (contracts/README.md,
// "Authentication"): courier tokens and tracking-link tokens. Both are HS256 tokens issued by the
// orders service with a shared secret; this package only verifies them.
package auth

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/codewithmuh/tasuki-delivery-platform/services/fleet/internal/events"
)

// Roles carried in the "role" claim.
const (
	RoleCourier  = "courier"  // sub = courier ID, signed with COURIER_TOKEN_SECRET
	RoleTracking = "tracking" // sub = order ID, signed with TRACKING_TOKEN_SECRET
)

// MinSecretBytes is the shortest secret accepted: HS256 needs at least 256 bits of key.
const MinSecretBytes = 32

// ClockSkewLeeway tolerates small clock differences between the issuer (orders) and this
// service; orders verifies courier tokens with the same 10 s.
const ClockSkewLeeway = 10 * time.Second

// ErrInvalidToken wraps every verification failure, so handlers can answer 401 with one check.
var ErrInvalidToken = errors.New("invalid token")

// claims is the token body: the registered claims (sub, exp, ...) plus Tasuki's "role".
type claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

// Token is what a verified token proves.
type Token struct {
	Subject   string    // courier ID or order ID
	ExpiresAt time.Time // streams end when the token expires
}

// Verifier checks tokens for one role.
type Verifier struct {
	secret        []byte
	role          string
	subjectPrefix string
	parser        *jwt.Parser
}

// NewVerifier returns a Verifier for tokens with the given role whose subject is an ID with the
// given prefix (events.PrefixCourier or events.PrefixOrder).
func NewVerifier(secret, role, subjectPrefix string) (*Verifier, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("the %s token secret must be at least %d bytes", role, MinSecretBytes)
	}
	return &Verifier{
		secret:        []byte(secret),
		role:          role,
		subjectPrefix: subjectPrefix,
		parser: jwt.NewParser(
			// Only HS256: this blocks "alg: none" and algorithm-confusion attacks.
			jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
			jwt.WithExpirationRequired(), // a token without exp would be valid forever
			jwt.WithLeeway(ClockSkewLeeway),
		),
	}, nil
}

// Verify checks the signature, expiry, role and subject of raw and returns what it proves.
func (v *Verifier) Verify(raw string) (Token, error) {
	var c claims
	if _, err := v.parser.ParseWithClaims(raw, &c, func(*jwt.Token) (any, error) { return v.secret, nil }); err != nil {
		return Token{}, fmt.Errorf("%w: %w", ErrInvalidToken, err)
	}
	if c.Role != v.role {
		return Token{}, fmt.Errorf("%w: role %q, want %q", ErrInvalidToken, c.Role, v.role)
	}
	if !events.ValidID(v.subjectPrefix, c.Subject) {
		return Token{}, fmt.Errorf("%w: subject is not a %s_ ID", ErrInvalidToken, v.subjectPrefix)
	}
	return Token{Subject: c.Subject, ExpiresAt: c.ExpiresAt.Time}, nil
}

// BearerToken extracts the token from an "Authorization: Bearer <token>" header.
func BearerToken(r *http.Request) (string, bool) {
	scheme, token, ok := strings.Cut(r.Header.Get("Authorization"), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") || strings.TrimSpace(token) == "" {
		return "", false
	}
	return strings.TrimSpace(token), true
}

// Issue signs a token. In the running system the orders service issues tokens; this exists for
// tests and local tooling, and uses exactly the claims Verify expects.
func Issue(secret, role, subject string, expiresAt time.Time) (string, error) {
	t := jwt.NewWithClaims(jwt.SigningMethodHS256, claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   subject,
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			IssuedAt:  jwt.NewNumericDate(time.Now()),
		},
	})
	signed, err := t.SignedString([]byte(secret))
	if err != nil {
		return "", fmt.Errorf("sign token: %w", err)
	}
	return signed, nil
}
