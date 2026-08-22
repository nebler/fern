package githubapp

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestJWTSignerCreatesBoundedRS256Claims(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	signer, err := NewJWTSigner(123456, key)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, time.August, 22, 12, 0, 0, 0, time.UTC)
	token, err := signer.AppToken(now)
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		t.Fatalf("JWT has %d parts", len(parts))
	}

	decode := func(part string) []byte {
		t.Helper()
		value, err := base64.RawURLEncoding.DecodeString(part)
		if err != nil {
			t.Fatal(err)
		}
		return value
	}
	var header map[string]any
	if err := json.Unmarshal(decode(parts[0]), &header); err != nil {
		t.Fatal(err)
	}
	if len(header) != 2 || header["alg"] != "RS256" || header["typ"] != "JWT" {
		t.Fatalf("header = %#v", header)
	}
	var claims map[string]json.RawMessage
	if err := json.Unmarshal(decode(parts[1]), &claims); err != nil {
		t.Fatal(err)
	}
	if len(claims) != 3 {
		t.Fatalf("claim count = %d, claims = %v", len(claims), claims)
	}
	var issuedAt, expiresAt int64
	var issuer string
	if err := json.Unmarshal(claims["iat"], &issuedAt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(claims["exp"], &expiresAt); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(claims["iss"], &issuer); err != nil {
		t.Fatal(err)
	}
	if issuer != strconv.FormatInt(123456, 10) {
		t.Fatalf("issuer = %q", issuer)
	}
	if issuedAt != now.Add(-time.Minute).Unix() || expiresAt != now.Add(9*time.Minute).Unix() {
		t.Fatalf("claim window = %s through %s", time.Unix(issuedAt, 0), time.Unix(expiresAt, 0))
	}
	if expiresAt-issuedAt > int64((10*time.Minute)/time.Second) || expiresAt-now.Unix() > int64((10*time.Minute)/time.Second) {
		t.Fatal("JWT claims exceed GitHub's ten-minute bound")
	}

	digest := sha256.Sum256([]byte(parts[0] + "." + parts[1]))
	if err := rsa.VerifyPKCS1v15(&key.PublicKey, crypto.SHA256, digest[:], decode(parts[2])); err != nil {
		t.Fatalf("verify RS256 signature: %v", err)
	}
}

func TestJWTSignerRejectsInvalidConfiguration(t *testing.T) {
	t.Parallel()
	weakKey, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name  string
		appID int64
		key   *rsa.PrivateKey
	}{
		{name: "missing app ID", key: weakKey},
		{name: "negative app ID", appID: -1, key: weakKey},
		{name: "missing key", appID: 1},
		{name: "weak key", appID: 1, key: weakKey},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewJWTSigner(test.appID, test.key); !errors.Is(err, ErrInvalidConfiguration) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestJWTSignerRejectsInvalidTimeWithoutLeakingKeyDetails(t *testing.T) {
	t.Parallel()
	signer := &JWTSigner{appID: 1, key: nil}
	_, err := signer.AppToken(time.Time{})
	if !errors.Is(err, ErrInvalidConfiguration) || strings.Contains(err.Error(), "private") {
		t.Fatalf("error = %v", err)
	}
}
