package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/nebler/fern/internal/runtime"
)

type ControlAuth struct {
	Password string
}

func (auth ControlAuth) Apply(request interface{ SetBasicAuth(string, string) }) {
	if auth.Password != "" {
		request.SetBasicAuth("fern", auth.Password)
	}
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

func requireServerAuth(next http.Handler, auth runtime.ServerAuth) http.Handler {
	allowed := newBasicAuthenticator("opencode", auth.Password, "opencode")
	if !allowed.enabled {
		return next
	}
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !allowed.valid(request) {
			allowed.reject(writer)
			return
		}
		next.ServeHTTP(writer, request)
	})
}
