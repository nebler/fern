package githubapp

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestCredentialStoreCreatesPrivateDirectoryAndRoundTrips(t *testing.T) {
	t.Parallel()
	directory := filepath.Join(t.TempDir(), "credentials")
	store, err := NewCredentialStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Lstat(directory)
	if err != nil {
		t.Fatal(err)
	}
	if !info.IsDir() || info.Mode().Perm() != 0o700 {
		t.Fatalf("directory mode = %v", info.Mode())
	}

	want := testStoredCredentials(t, 123)
	if err := store.Save(want); err != nil {
		t.Fatal(err)
	}
	fileInfo, err := os.Lstat(filepath.Join(directory, credentialFileName))
	if err != nil {
		t.Fatal(err)
	}
	if !fileInfo.Mode().IsRegular() || fileInfo.Mode().Perm() != 0o600 {
		t.Fatalf("credential mode = %v", fileInfo.Mode())
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	assertStoredCredentials(t, got, want)
}

func TestStoredCredentialsRoundTripInMemory(t *testing.T) {
	t.Parallel()
	want := testStoredCredentials(t, 123)
	payload, err := marshalStoredCredentials(want)
	if err != nil {
		t.Fatal(err)
	}
	got, err := parseStoredCredentials(payload)
	if err != nil {
		t.Fatal(err)
	}
	assertStoredCredentials(t, got, want)
	payload = append(payload, []byte(`{"tamper":true}`)...)
	if _, err := parseStoredCredentials(payload); !errors.Is(err, ErrStoredCredentialsInvalid) {
		t.Fatalf("tampered candidate error = %v", err)
	}
}

func TestCredentialStoreRejectsUnsafeDirectoryAndTypes(t *testing.T) {
	t.Parallel()
	t.Run("directory permissions", func(t *testing.T) {
		directory := filepath.Join(t.TempDir(), "credentials")
		if err := os.Mkdir(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := NewCredentialStore(directory); !errors.Is(err, ErrCredentialStoreSecurity) {
			t.Fatalf("error = %v", err)
		}
	})
	t.Run("credential is directory", func(t *testing.T) {
		store, directory := newTestCredentialStore(t)
		if err := os.Mkdir(filepath.Join(directory, credentialFileName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); err == nil {
			t.Fatal("loaded a directory as credentials")
		}
	})
}

func TestCredentialStoreRejectsSymlinks(t *testing.T) {
	t.Parallel()
	parent := t.TempDir()
	realDirectory := filepath.Join(parent, "real")
	if err := os.Mkdir(realDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(parent, "link")
	if err := os.Symlink(realDirectory, link); err != nil {
		t.Fatal(err)
	}
	if _, err := NewCredentialStore(link); !errors.Is(err, ErrCredentialStoreSecurity) {
		t.Fatalf("error = %v", err)
	}
}

func TestCredentialStoreStrictlyRejectsMalformedAndOversizedData(t *testing.T) {
	t.Parallel()
	privateKey := testPrivateKeyPEM(t)
	secret := "stored-secret-must-not-escape"
	valid := fmt.Sprintf(`{"version":2,"app_id":1,"private_key_pem":%q}`, privateKey)
	tests := []struct {
		name    string
		payload string
	}{
		{name: "malformed", payload: `{"version":` + secret},
		{name: "unknown field", payload: strings.TrimSuffix(valid, "}") + `,"unknown":"` + secret + `"}`},
		{name: "trailing data", payload: valid + ` {"secret":"` + secret + `"}`},
		{name: "duplicate field", payload: strings.Replace(valid, `"app_id":1`, `"app_id":1,"app_id":2`, 1)},
		{name: "unsupported version", payload: strings.Replace(valid, `"version":2`, `"version":1`, 1)},
		{name: "removed manifest field", payload: strings.Replace(valid, `"app_id":1`, `"app_id":1,"client_secret":"`+secret+`"`, 1)},
		{name: "missing field", payload: strings.Replace(valid, `"app_id":1,`, "", 1)},
		{name: "invalid app ID", payload: strings.Replace(valid, `"app_id":1`, `"app_id":0`, 1)},
		{name: "invalid key", payload: strings.Replace(valid, fmt.Sprintf("%q", privateKey), fmt.Sprintf("%q", secret), 1)},
		{name: "oversized", payload: strings.Repeat(secret, maxCredentialFileBytes/len(secret)+2)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, directory := newTestCredentialStore(t)
			if err := os.WriteFile(filepath.Join(directory, credentialFileName), []byte(test.payload), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := store.Load()
			if !errors.Is(err, ErrStoredCredentialsInvalid) || strings.Contains(err.Error(), secret) {
				t.Fatalf("error = %v", err)
			}
		})
	}
}

func TestCredentialStoreIgnoresInterruptedTempsAndReplacesAtomically(t *testing.T) {
	t.Parallel()
	store, directory := newTestCredentialStore(t)
	first := testStoredCredentials(t, 101)
	second := testStoredCredentials(t, 202)
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	interrupted := filepath.Join(directory, "."+credentialFileName+".interrupted.tmp")
	const interruptedContent = "partial-secret-content"
	if err := os.WriteFile(interrupted, []byte(interruptedContent), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := store.Load()
	if err != nil {
		t.Fatal(err)
	}
	assertStoredCredentials(t, got, first)
	invalid := second
	invalid.privateKeyPEM = []byte("invalid-private-key-must-not-escape")
	if err := store.Save(invalid); !errors.Is(err, ErrStoredCredentialsInvalid) || strings.Contains(err.Error(), "invalid-private-key-must-not-escape") {
		t.Fatalf("invalid replacement error = %v", err)
	}
	got, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	assertStoredCredentials(t, got, first)

	if err := store.Save(second); err != nil {
		t.Fatal(err)
	}
	got, err = store.Load()
	if err != nil {
		t.Fatal(err)
	}
	assertStoredCredentials(t, got, second)
	partial, err := os.ReadFile(interrupted)
	if err != nil || string(partial) != interruptedContent {
		t.Fatalf("interrupted temp = %q, error = %v", partial, err)
	}
}

func TestCredentialStoreConcurrentReplacementNeverLoadsPartialState(t *testing.T) {
	t.Parallel()
	store, _ := newTestCredentialStore(t)
	first := testStoredCredentials(t, 1)
	second := testStoredCredentials(t, 2)
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}

	var wait sync.WaitGroup
	errorsSeen := make(chan error, 2)
	wait.Add(2)
	go func() {
		defer wait.Done()
		for i := range 50 {
			credentials := first
			if i%2 != 0 {
				credentials = second
			}
			if err := store.Save(credentials); err != nil {
				errorsSeen <- err
				return
			}
		}
	}()
	go func() {
		defer wait.Done()
		for range 100 {
			credentials, err := store.Load()
			if err != nil {
				errorsSeen <- err
				return
			}
			if credentials.AppID() == first.AppID() {
				if string(credentials.privateKeyPEM) != string(first.privateKeyPEM) {
					errorsSeen <- errors.New("loaded partial first credentials")
					return
				}
			} else if credentials.AppID() == second.AppID() {
				if string(credentials.privateKeyPEM) != string(second.privateKeyPEM) {
					errorsSeen <- errors.New("loaded partial second credentials")
					return
				}
			} else {
				errorsSeen <- errors.New("loaded unknown credentials")
				return
			}
		}
	}()
	wait.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		t.Fatal(err)
	}
}

func TestCredentialStoreIOErrorsKeepTheirCause(t *testing.T) {
	t.Parallel()
	store, directory := newTestCredentialStore(t)
	if err := os.Mkdir(filepath.Join(directory, credentialFileName), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, credentialFileName, "child"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	err := store.Save(testStoredCredentials(t, 9))
	if !errors.Is(err, ErrCredentialStoreIO) || err.Error() == ErrCredentialStoreIO.Error() {
		t.Fatalf("error = %v, want wrapped ErrCredentialStoreIO with cause", err)
	}
}

func TestAppCredentialsFormattingRedactsKey(t *testing.T) {
	t.Parallel()
	credentials := testStoredCredentials(t, 7)
	for _, formatted := range []string{fmt.Sprint(credentials), fmt.Sprintf("%#v", credentials), fmt.Sprintf("%+v", credentials)} {
		if strings.Contains(formatted, "PRIVATE KEY") || !strings.Contains(formatted, "redacted") {
			t.Fatalf("formatted credentials = %q", formatted)
		}
	}
	if _, err := NewAppCredentials(0, credentials.privateKeyPEM); err == nil {
		t.Fatal("accepted a non-positive App ID")
	}
	if _, err := NewAppCredentials(7, []byte("not a key")); !errors.Is(err, ErrInvalidPrivateKey) {
		t.Fatalf("invalid key error = %v", err)
	}
}

func newTestCredentialStore(t *testing.T) (*CredentialStore, string) {
	t.Helper()
	directory := filepath.Join(t.TempDir(), "credentials")
	store, err := NewCredentialStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	return store, directory
}

func testStoredCredentials(t *testing.T, appID int64) AppCredentials {
	t.Helper()
	credentials, err := NewAppCredentials(appID, testPrivateKeyPEM(t))
	if err != nil {
		t.Fatal(err)
	}
	return credentials
}

func testPrivateKeyPEM(t *testing.T) []byte {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

func assertStoredCredentials(t *testing.T, got, want AppCredentials) {
	t.Helper()
	if got.AppID() != want.AppID() || string(got.privateKeyPEM) != string(want.privateKeyPEM) || got.PrivateKey() == nil {
		t.Fatal("stored credentials did not round trip")
	}
}
