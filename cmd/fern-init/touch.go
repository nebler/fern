package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"os"
	"syscall"
	"time"

	"github.com/nebler/fern/internal/boxexec"
)

// maxTouchBody bounds a touch request: MaxTouch paths of a few hundred bytes.
const maxTouchBody = 8 << 20

// touch sets the times of the files a request names, relative to dir, to
// what they are (boxexec.TouchPath). The file stays as it is; a watcher in
// this box hears an attribute change, which dev servers take as a change.
// Paths that leave dir, or aren't regular files, are passed over.
func touch(token, dir string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte("Bearer "+token)) != 1 {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var req boxexec.TouchRequest
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTouchBody)).Decode(&req); err != nil || len(req.Paths) > boxexec.MaxTouch {
			http.Error(w, "bad touch request", http.StatusBadRequest)
			return
		}
		root, err := os.OpenRoot(dir)
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		defer func() { _ = root.Close() }()
		for _, p := range req.Paths {
			fi, err := root.Lstat(p)
			if err != nil || !fi.Mode().IsRegular() {
				continue // gone since, or a folder or link
			}
			st, ok := fi.Sys().(*syscall.Stat_t)
			if !ok {
				continue
			}
			_ = root.Chtimes(p, time.Unix(st.Atim.Unix()), time.Unix(st.Mtim.Unix()))
		}
		w.WriteHeader(http.StatusNoContent)
	}
}
