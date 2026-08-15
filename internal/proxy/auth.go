package proxy

import (
	"crypto/sha256"
	"crypto/subtle"
	"net/http"

	"github.com/nebler/fern/internal/runtime"
)

func requireServerAuth(next http.Handler, auth runtime.ServerAuth) http.Handler {
	if auth.Password == "" {
		return next
	}
	username := auth.Username
	if username == "" {
		username = "opencode"
	}
	wantUsername := sha256.Sum256([]byte(username))
	wantPassword := sha256.Sum256([]byte(auth.Password))

	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		username, password, ok := request.BasicAuth()
		gotUsername := sha256.Sum256([]byte(username))
		gotPassword := sha256.Sum256([]byte(password))
		valid := subtle.ConstantTimeCompare(gotUsername[:], wantUsername[:]) &
			subtle.ConstantTimeCompare(gotPassword[:], wantPassword[:])
		if !ok || valid != 1 {
			writer.Header().Set("WWW-Authenticate", `Basic realm="opencode"`)
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(writer, request)
	})
}
