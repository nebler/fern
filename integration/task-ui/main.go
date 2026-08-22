package main

import (
	"embed"
	"flag"
	"html/template"
	"io/fs"
	"log"
	"net/http"
	"os"
	"strings"
	"time"
)

//go:embed templates/*.html assets/*
var uiFiles embed.FS

type app struct {
	fixtures FixtureSet
	template *template.Template
	assets   http.Handler
}

type pageData struct {
	Workspace Workspace
	Tasks     []Task
	Task      *Task
	Now       time.Time
}

func main() {
	addr := flag.String("addr", "127.0.0.1:8787", "preview listen address")
	flag.Parse()

	handler, err := newHandler()
	if err != nil {
		log.Fatal(err)
	}
	server := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       30 * time.Second,
	}
	log.Printf("Fern task UI preview: http://%s", *addr)
	if err := server.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Fatal(err)
	}
}

func newHandler() (http.Handler, error) {
	fixtures, err := loadFixtures(fixtureFiles, "fixtures/tasks.json")
	if err != nil {
		return nil, err
	}
	a, err := buildApp(fixtures)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.Handle("GET /assets/", http.StripPrefix("/assets/", a.assets))
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) })
	mux.HandleFunc("GET /{$}", a.inbox)
	mux.HandleFunc("GET /tasks/{id}", a.detail)
	mux.HandleFunc("/", methodOrNotFound)
	return securityHeaders(mux), nil
}

func buildApp(fixtures FixtureSet) (*app, error) {
	funcs := template.FuncMap{
		"clock": func(t time.Time) string { return t.Format("3:04 PM") },
		"day":   func(t time.Time) string { return t.Format("Jan 2") },
		"short": func(value string) string {
			if len(value) <= 9 {
				return value
			}
			return value[:9]
		},
		"label":     func(value string) string { return strings.ReplaceAll(value, "_", " ") },
		"stateTone": stateTone,
	}
	tmpl, err := template.New("root").Funcs(funcs).ParseFS(uiFiles, "templates/*.html")
	if err != nil {
		return nil, err
	}
	assets, err := fs.Sub(uiFiles, "assets")
	if err != nil {
		return nil, err
	}
	return &app{fixtures: fixtures, template: tmpl, assets: http.FileServer(http.FS(assets))}, nil
}

func (a *app) inbox(w http.ResponseWriter, r *http.Request) {
	a.render(w, "inbox", pageData{Workspace: a.fixtures.Workspace, Tasks: a.fixtures.Tasks, Now: time.Now()})
}

func (a *app) detail(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	for i := range a.fixtures.Tasks {
		if a.fixtures.Tasks[i].ID == id {
			a.render(w, "detail", pageData{Workspace: a.fixtures.Workspace, Tasks: a.fixtures.Tasks, Task: &a.fixtures.Tasks[i], Now: time.Now()})
			return
		}
	}
	http.Error(w, "Task not found", http.StatusNotFound)
}

func (a *app) render(w http.ResponseWriter, name string, data pageData) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if err := a.template.ExecuteTemplate(w, name, data); err != nil {
		log.Printf("render %s: %v", name, err)
	}
}

func methodOrNotFound(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Read-only preview", http.StatusMethodNotAllowed)
		return
	}
	http.NotFound(w, r)
}

func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Security-Policy", "default-src 'self'; style-src 'self'; img-src 'self' data:; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		next.ServeHTTP(w, r)
	})
}

func stateTone(state string) string {
	switch state {
	case "completed", "succeeded", "published", "sealed", "applied":
		return "good"
	case "input_required", "pending", "decision_recorded":
		return "attention"
	case "uncertain", "reconciling", "cancel_requested":
		return "watch"
	case "recovery_required", "failed", "conflict", "rejected":
		return "danger"
	case "running", "delivering", "admitted", "requested", "preparing", "ready", "pushing", "opening_pr", "collecting":
		return "active"
	default:
		return "quiet"
	}
}

func init() {
	log.SetOutput(os.Stderr)
	log.SetFlags(0)
}
