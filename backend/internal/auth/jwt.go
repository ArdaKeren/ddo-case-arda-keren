package auth

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

type Claims struct {
	Role Role `json:"role"`
	jwt.RegisteredClaims
}

type TokenIssuer struct {
	secret []byte
	issuer string
	ttl    time.Duration
}

func NewTokenIssuer(secret []byte, issuer string, ttl time.Duration) *TokenIssuer {
	return &TokenIssuer{secret: secret, issuer: issuer, ttl: ttl}
}

func (t *TokenIssuer) Issue(userID string, role Role) (token string, expiresAt time.Time, err error) {
	now := time.Now()
	expiresAt = now.Add(t.ttl)
	jti := make([]byte, 16)
	if _, err = rand.Read(jti); err != nil {
		return "", time.Time{}, err
	}
	claims := Claims{
		Role: role,
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   userID,
			Issuer:    t.issuer,
			IssuedAt:  jwt.NewNumericDate(now),
			NotBefore: jwt.NewNumericDate(now),
			ExpiresAt: jwt.NewNumericDate(expiresAt),
			ID:        hex.EncodeToString(jti),
		},
	}
	token, err = jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(t.secret)
	return token, expiresAt, err
}

// Parse validates signature, algorithm, issuer and time claims.
func (t *TokenIssuer) Parse(token string) (*Claims, error) {
	claims := &Claims{}
	_, err := jwt.ParseWithClaims(token, claims,
		func(*jwt.Token) (any, error) { return t.secret, nil },
		jwt.WithValidMethods([]string{jwt.SigningMethodHS256.Alg()}),
		jwt.WithIssuer(t.issuer),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(30*time.Second),
	)
	if err != nil {
		return nil, err
	}
	if claims.Subject == "" || !claims.Role.Valid() {
		return nil, errors.New("token missing subject or role")
	}
	return claims, nil
}
