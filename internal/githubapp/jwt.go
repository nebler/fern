package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"strconv"
	"time"
)

const (
	jwtBackdate = time.Minute
	jwtValidity = 9 * time.Minute
)

// AppTokenSource allows the installation client to remain independent of key
// storage. Implementations must return a short-lived RS256 GitHub App JWT.
type AppTokenSource interface {
	AppToken(now time.Time) (string, error)
}

// JWTSigner signs bounded GitHub App authentication claims with RS256.
type JWTSigner struct {
	appID int64
	key   *rsa.PrivateKey
}

func NewJWTSigner(appID int64, key *rsa.PrivateKey) (*JWTSigner, error) {
	if appID <= 0 || key == nil || key.N == nil || key.N.BitLen() < 2048 || key.Validate() != nil {
		return nil, ErrInvalidConfiguration
	}
	return &JWTSigner{appID: appID, key: key}, nil
}

func (signer *JWTSigner) AppToken(now time.Time) (string, error) {
	if signer == nil || signer.appID <= 0 || signer.key == nil || now.IsZero() || now.Unix() <= 0 {
		return "", ErrInvalidConfiguration
	}
	header, err := json.Marshal(struct {
		Algorithm string `json:"alg"`
		Type      string `json:"typ"`
	}{Algorithm: "RS256", Type: "JWT"})
	if err != nil {
		return "", ErrSigningFailed
	}
	claims, err := json.Marshal(struct {
		IssuedAt  int64  `json:"iat"`
		ExpiresAt int64  `json:"exp"`
		Issuer    string `json:"iss"`
	}{
		IssuedAt:  now.Add(-jwtBackdate).Unix(),
		ExpiresAt: now.Add(jwtValidity).Unix(),
		Issuer:    strconv.FormatInt(signer.appID, 10),
	})
	if err != nil {
		return "", ErrSigningFailed
	}

	encoding := base64.RawURLEncoding
	signingInput := encoding.EncodeToString(header) + "." + encoding.EncodeToString(claims)
	digest := sha256.Sum256([]byte(signingInput))
	signature, err := rsa.SignPKCS1v15(rand.Reader, signer.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", ErrSigningFailed
	}
	return signingInput + "." + encoding.EncodeToString(signature), nil
}
