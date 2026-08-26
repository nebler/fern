package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"
)

// ControlAuth carries the operator-facing Fern control password shared by the
// gateway surfaces that guard /fern routes.
type ControlAuth struct {
	// Password is the configured control password; empty disables control
	// authentication.
	Password string
}

type basicAuthenticator struct {
	username [sha256.Size]byte
	password [sha256.Size]byte
	realm    string
	enabled  bool
}

func newBasicAuthenticator(username, password, realm string) basicAuthenticator {
	return basicAuthenticator{
		username: sha256.Sum256([]byte(username)),
		password: sha256.Sum256([]byte(password)),
		realm:    realm,
		enabled:  password != "",
	}
}

func (auth basicAuthenticator) valid(request *http.Request) bool {
	if !auth.enabled {
		return false
	}
	username, password, ok := request.BasicAuth()
	gotUsername := sha256.Sum256([]byte(username))
	gotPassword := sha256.Sum256([]byte(password))
	return ok && subtle.ConstantTimeCompare(gotUsername[:], auth.username[:]) == 1 &&
		subtle.ConstantTimeCompare(gotPassword[:], auth.password[:]) == 1
}

func (auth basicAuthenticator) reject(writer http.ResponseWriter) {
	writer.Header().Set("WWW-Authenticate", `Basic realm="`+auth.realm+`"`)
	http.Error(writer, "unauthorized", http.StatusUnauthorized)
}
