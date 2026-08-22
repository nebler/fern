package githubapp

import (
	"bytes"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
)

const maxPrivateKeyPEMBytes = 64 << 10

// ParseRSAPrivateKeyPEM parses an unencrypted PKCS#1 or PKCS#8 RSA private key.
func ParseRSAPrivateKeyPEM(value []byte) (*rsa.PrivateKey, error) {
	if len(value) == 0 || len(value) > maxPrivateKeyPEMBytes {
		return nil, ErrInvalidPrivateKey
	}
	value = bytes.TrimSpace(value)
	if !bytes.HasPrefix(value, []byte("-----BEGIN ")) {
		return nil, ErrInvalidPrivateKey
	}
	block, rest := pem.Decode(value)
	if block == nil || len(bytes.TrimSpace(rest)) != 0 || len(block.Headers) != 0 {
		return nil, ErrInvalidPrivateKey
	}

	var key *rsa.PrivateKey
	switch block.Type {
	case "RSA PRIVATE KEY":
		parsed, err := x509.ParsePKCS1PrivateKey(block.Bytes)
		if err != nil {
			return nil, ErrInvalidPrivateKey
		}
		key = parsed
	case "PRIVATE KEY":
		parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
		if err != nil {
			return nil, ErrInvalidPrivateKey
		}
		var ok bool
		key, ok = parsed.(*rsa.PrivateKey)
		if !ok {
			return nil, ErrInvalidPrivateKey
		}
	default:
		return nil, ErrInvalidPrivateKey
	}
	if key.N == nil || key.N.BitLen() < 2048 || key.Validate() != nil {
		return nil, ErrInvalidPrivateKey
	}
	return key, nil
}
