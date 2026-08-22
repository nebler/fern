package githubapp

import "errors"

var (
	ErrInvalidPrivateKey     = errors.New("invalid GitHub App RSA private key")
	ErrInvalidManifest       = errors.New("invalid GitHub App manifest")
	ErrInvalidManifestCode   = errors.New("invalid GitHub App manifest code")
	ErrManifestCodeUsed      = errors.New("GitHub App manifest code already used")
	ErrInvalidAppCredentials = errors.New("GitHub returned invalid App credentials")
)
