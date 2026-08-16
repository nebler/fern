package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/nebler/fern/internal/runtime"
)

func requireServerAuth(next http.Handler, auth runtime.ServerAuth) http.Handler {
	type credentials struct {
		username [sha256.Size]byte
		password [sha256.Size]byte
	}
	var allowed []credentials
	if auth.Password != "" {
		allowed = append(allowed, credentials{
			username: sha256.Sum256([]byte("opencode")),
			password: sha256.Sum256([]byte(auth.Password)),
		})
	}
	if len(allowed) == 0 {
		return next
	}

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		gotUsername := sha256.Sum256([]byte(username))
		gotPassword := sha256.Sum256([]byte(password))
		valid := 0
		for _, want := range allowed {
			valid |= subtle.ConstantTimeCompare(gotUsername[:], want.username[:]) &
				subtle.ConstantTimeCompare(gotPassword[:], want.password[:])
		}
		if !ok || valid != 1 {
			writer.Header().Set("WWW-Authenticate", `Basic realm="opencode"`)
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}
