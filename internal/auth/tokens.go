package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

const (
	AccessTTL  = 15 * time.Minute
	RefreshTTL = 30 * 24 * time.Hour
)

type Claims struct {
	Role string `json:"role"`
	jwt.RegisteredClaims
}

type Tokens struct {
	secret []byte
	now    func() time.Time
}

func NewTokens(secret []byte) *Tokens {
	return &Tokens{secret: secret, now: time.Now}
}

// IssueAccess returns a short-lived signed access token for the user.
func (t *Tokens) IssueAccess(userID, role string) (string, time.Time, error) {
	exp := t.now().Add(AccessTTL)
	claims := Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			IssuedAt:  jwt.NewNumericDate(t.now()),
			ExpiresAt: jwt.NewNumericDate(exp),
			Audience:  jwt.ClaimStrings{"access"},
		},
	}
	s, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	return s, exp, err
}

// ParseAccess validates an access token and returns its claims.
func (t *Tokens) ParseAccess(token string) (*Claims, error) {
	var c Claims
	_, err := jwt.ParseWithClaims(token, &c, func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithAudience("access"),
		jwt.WithTimeFunc(t.now),
		jwt.WithExpirationRequired(),
	)
	if err != nil {
		return nil, err
	}
	if c.Subject == "" {
		return nil, errors.New("token has no subject")
	}
	return &c, nil
}

// NewRefreshToken returns an opaque random token and the hash to store in the database.
func NewRefreshToken() (token, hash string) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	token = base64.RawURLEncoding.EncodeToString(b)
	return token, HashRefreshToken(token)
}

func HashRefreshToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
