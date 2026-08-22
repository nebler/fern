package githubapp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"testing"
)

func TestParseRSAPrivateKeyPEMAcceptsPKCS1AndPKCS8(t *testing.T) {
	t.Parallel()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	pkcs8, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name      string
		blockType string
		der       []byte
	}{
		{name: "PKCS1", blockType: "RSA PRIVATE KEY", der: x509.MarshalPKCS1PrivateKey(key)},
		{name: "PKCS8", blockType: "PRIVATE KEY", der: pkcs8},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			parsed, err := ParseRSAPrivateKeyPEM(pem.EncodeToMemory(&pem.Block{Type: test.blockType, Bytes: test.der}))
			if err != nil {
				t.Fatal(err)
			}
			if parsed.N.Cmp(key.N) != 0 || parsed.N.BitLen() < 2048 {
				t.Fatal("parsed a different RSA key")
			}
		})
	}
}

func TestParseRSAPrivateKeyPEMRejectsInvalidAndWeakKeys(t *testing.T) {
	t.Parallel()
	weak, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ecdsaDER, err := x509.MarshalPKCS8PrivateKey(ecdsaKey)
	if err != nil {
		t.Fatal(err)
	}
	weakPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(weak)})
	tests := [][]byte{
		nil,
		[]byte("not PEM"),
		weakPEM,
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: ecdsaDER}),
		append(append([]byte(nil), weakPEM...), weakPEM...),
		append([]byte("untrusted prefix\n"), weakPEM...),
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: []byte("secret details")}),
	}
	for _, value := range tests {
		if _, err := ParseRSAPrivateKeyPEM(value); !errors.Is(err, ErrInvalidPrivateKey) || err.Error() != ErrInvalidPrivateKey.Error() {
			t.Fatalf("error = %v", err)
		}
	}
}
