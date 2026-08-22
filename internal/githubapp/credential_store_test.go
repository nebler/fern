package githubapp

import (
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

	want := testStoredCredentials(t, 123, "first")
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

func TestCredentialStoreRejectsUnsafePermissionsAndTypes(t *testing.T) {
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
	t.Run("directory permissions changed after open", func(t *testing.T) {
		store, directory := newTestCredentialStore(t)
		if err := os.Chmod(directory, 0o755); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); !errors.Is(err, ErrCredentialStoreSecurity) {
			t.Fatalf("load error = %v", err)
		}
		if err := store.Save(testStoredCredentials(t, 2, "replacement")); !errors.Is(err, ErrCredentialStoreSecurity) {
			t.Fatalf("save error = %v", err)
		}
	})
	t.Run("credential permissions", func(t *testing.T) {
		store, directory := newTestCredentialStore(t)
		if err := store.Save(testStoredCredentials(t, 1, "private")); err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(filepath.Join(directory, credentialFileName), 0o640); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); !errors.Is(err, ErrCredentialStoreSecurity) {
			t.Fatalf("load error = %v", err)
		}
		if err := store.Save(testStoredCredentials(t, 2, "replacement")); !errors.Is(err, ErrCredentialStoreSecurity) {
			t.Fatalf("save error = %v", err)
		}
	})
	t.Run("credential is directory", func(t *testing.T) {
		store, directory := newTestCredentialStore(t)
		if err := os.Mkdir(filepath.Join(directory, credentialFileName), 0o700); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); !errors.Is(err, ErrCredentialStoreSecurity) {
			t.Fatalf("error = %v", err)
		}
	})
}

func TestCredentialStoreRejectsSymlinks(t *testing.T) {
	t.Parallel()
	t.Run("directory", func(t *testing.T) {
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
	})
	t.Run("credential", func(t *testing.T) {
		store, directory := newTestCredentialStore(t)
		external := filepath.Join(t.TempDir(), "external-secret")
		const content = "host-file-content-must-not-escape"
		if err := os.WriteFile(external, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(external, filepath.Join(directory, credentialFileName)); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Load(); !errors.Is(err, ErrCredentialStoreSecurity) || strings.Contains(err.Error(), content) {
			t.Fatalf("load error = %v", err)
		}
		if err := store.Save(testStoredCredentials(t, 2, "replacement")); !errors.Is(err, ErrCredentialStoreSecurity) {
			t.Fatalf("save error = %v", err)
		}
		got, err := os.ReadFile(external)
		if err != nil || string(got) != content {
			t.Fatalf("external file = %q, error = %v", got, err)
		}
	})
}

func TestCredentialStoreStrictlyRejectsMalformedAndOversizedData(t *testing.T) {
	t.Parallel()
	privateKey := testPrivateKeyPEM(t)
	secret := "stored-secret-must-not-escape"
	valid := fmt.Sprintf(`{"version":1,"app_id":1,"client_id":"client","client_secret":%q,"webhook_secret":"","private_key_pem":%q}`, secret, privateKey)
	tests := []struct {
		name    string
		payload string
	}{
		{name: "malformed", payload: `{"version":` + secret},
		{name: "unknown field", payload: strings.TrimSuffix(valid, "}") + `,"unknown":"` + secret + `"}`},
		{name: "trailing data", payload: valid + ` {"secret":"` + secret + `"}`},
		{name: "duplicate field", payload: strings.Replace(valid, `"app_id":1`, `"app_id":1,"app_id":2`, 1)},
		{name: "unsupported version", payload: strings.Replace(valid, `"version":1`, `"version":2`, 1)},
		{name: "missing field", payload: strings.Replace(valid, `,"webhook_secret":""`, "", 1)},
		{name: "invalid app ID", payload: strings.Replace(valid, `"app_id":1`, `"app_id":0`, 1)},
		{name: "invalid secret", payload: strings.Replace(valid, `"client_id":"client"`, `"client_id":"bad secret"`, 1)},
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
	first := testStoredCredentials(t, 101, "first")
	second := testStoredCredentials(t, 202, "second")
	if err := store.Save(first); err != nil {
		t.Fatal(err)
	}
	interrupted := filepath.Join(directory, credentialTemporaryStem+"interrupted.tmp")
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
	first := testStoredCredentials(t, 1, "first")
	second := testStoredCredentials(t, 2, "second")
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
				if credentials.ClientSecret() != first.ClientSecret() {
					errorsSeen <- errors.New("loaded partial first credentials")
					return
				}
			} else if credentials.AppID() == second.AppID() {
				if credentials.ClientSecret() != second.ClientSecret() {
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

func TestOpenOldCredentialGenerationRemainsAValidSnapshot(t *testing.T) {
	store, directory := newTestCredentialStore(t)
	if err := store.Save(testStoredCredentials(t, 1, "first")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, credentialFileName)
	oldGeneration, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer oldGeneration.Close()
	replacement := filepath.Join(directory, "replacement")
	if err := os.WriteFile(replacement, []byte("replacement"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(replacement, path); err != nil {
		t.Fatal(err)
	}
	if err := requirePrivateRegularFile(oldGeneration); err != nil {
		t.Fatalf("open atomically replaced generation rejected: %v", err)
	}
}

func TestCredentialStoreErrorsAndFormattingAreRedacted(t *testing.T) {
	t.Parallel()
	store, directory := newTestCredentialStore(t)
	secretPath := filepath.Join(directory, "path-must-not-be-formatted")
	store.directory = secretPath
	formatted := fmt.Sprintf("%v %#v", store, store)
	if strings.Contains(formatted, secretPath) || strings.Contains(formatted, "path-must-not-be-formatted") {
		t.Fatalf("formatted store exposes its path: %s", formatted)
	}
	if _, err := store.Load(); err == nil || strings.Contains(err.Error(), secretPath) {
		t.Fatalf("error = %v", err)
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

func testStoredCredentials(t *testing.T, appID int64, label string) AppCredentials {
	t.Helper()
	privateKeyPEM := testPrivateKeyPEM(t)
	key, err := ParseRSAPrivateKeyPEM(privateKeyPEM)
	if err != nil {
		t.Fatal(err)
	}
	return AppCredentials{
		appID:         appID,
		clientID:      "client-" + label,
		clientSecret:  "client-secret-" + label,
		webhookSecret: "webhook-secret-" + label,
		privateKeyPEM: privateKeyPEM,
		privateKey:    key,
	}
}

func assertStoredCredentials(t *testing.T, got, want AppCredentials) {
	t.Helper()
	if got.AppID() != want.AppID() || got.ClientID() != want.ClientID() || got.ClientSecret() != want.ClientSecret() || got.WebhookSecret() != want.WebhookSecret() || string(got.PrivateKeyPEM()) != string(want.PrivateKeyPEM()) || got.PrivateKey() == nil {
		t.Fatal("stored credentials did not round trip")
	}
}
