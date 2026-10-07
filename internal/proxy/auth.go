package proxy

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/nebler/fern/internal/control"
	"github.com/nebler/fern/internal/task"
)

// ControlAuth carries the operator-facing Fern control password shared by the
// gateway surfaces that guard /fern routes.
type ControlAuth struct {
	// Password is the configured control password; empty disables control
	// authentication.
	Password string
}

// operatorAuth is the loopback operator realm: explicit Basic credentials for
// user "fern" rather than ambient browser cookies, so device CSRF tokens do not
// apply. Its audit credential identifier is the stable random value persisted
// by the control store — never anything derived from the control password — so
// no offline brute-force oracle of that secret reaches durable snapshots.
type operatorAuth struct {
	username, password [sha256.Size]byte
	enabled            bool
	credentialID       string
}

func newOperatorAuth(store *control.Store, auth ControlAuth) (*operatorAuth, error) {
	credentialID, err := store.EnsureOperatorCredentialID()
	if err != nil {
		return nil, err
	}
	return &operatorAuth{username: sha256.Sum256([]byte("fern")), password: sha256.Sum256([]byte(auth.Password)),
		enabled: auth.Password != "", credentialID: credentialID}, nil
}

func (auth *operatorAuth) authenticate(writer http.ResponseWriter, request *http.Request) (realm, *http.Request, func(), bool) {
	username, password, ok := request.BasicAuth()
	gotUsername, gotPassword := sha256.Sum256([]byte(username)), sha256.Sum256([]byte(password))
	if !auth.enabled || !ok || subtle.ConstantTimeCompare(gotUsername[:], auth.username[:]) != 1 ||
		subtle.ConstantTimeCompare(gotPassword[:], auth.password[:]) != 1 {
		writer.Header().Set("WWW-Authenticate", `Basic realm="fern-control"`)
		http.Error(writer, "unauthorized", http.StatusUnauthorized)
		return 0, nil, nil, false
	}
	actor := task.ActorSnapshot{
		Type: task.ActorOperator, ID: "local-operator", DisplayName: "Local operator",
		CredentialID: auth.credentialID, Authentication: "basic", RequestID: rand.Text(),
	}
	request = request.WithContext(task.WithActor(request.Context(), actor))
	stripCredentials(request)
	return operator, request, noRelease, true
}

// stripCredentials removes every client credential before dispatch so inner
// handlers can only trust the actor installed by the realm.
func stripCredentials(request *http.Request) {
	request.Header.Del("Authorization")
	request.Header.Del("Cookie")
}
