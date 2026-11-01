package setup_test

import (
	"errors"
	"os/exec"
	"testing"

	"github.com/google/go-cmp/cmp"
	"sigs.k8s.io/yaml"

	"github.com/nebler/fern/internal/setup"
)

// guessed is a guessed services.yaml, parsed.
type guessed struct {
	Services map[string]struct {
		Run   string `json:"run"`
		Stop  string `json:"stop"`
		Ready string `json:"ready"`
		Port  int    `json:"port"`
	} `json:"services"`
}

func TestMakeGuess(t *testing.T) {
	tests := []struct {
		name      string
		files     map[string]string
		wantRun   string // the web service's
		wantPort  int
		wantTools string
		wantDB    bool
	}{
		{
			name: "vite with npm",
			files: map[string]string{
				"package.json":      `{"scripts": {"dev": "vite"}, "devDependencies": {"vite": "^7"}}`,
				"package-lock.json": `{}`,
			},
			wantRun:   "npm ci --prefer-offline && exec npm run dev -- --host 0.0.0.0 --port $PORT --strictPort",
			wantPort:  5173,
			wantTools: "# Fern's guess, from package.json.\n[tools]\nnode = \"lts\"\n",
		},
		{
			name: "next with pnpm, versions from packageManager and .nvmrc",
			files: map[string]string{
				"package.json":   `{"scripts": {"dev": "next dev"}, "dependencies": {"next": "16"}, "packageManager": "pnpm@10.4.1+sha512.abc"}`,
				"pnpm-lock.yaml": "lockfileVersion: '9.0'\n",
				".nvmrc":         "v22.11.0\n",
			},
			wantRun:   "pnpm install --frozen-lockfile --prefer-offline && exec pnpm run dev -H 0.0.0.0 -p $PORT",
			wantPort:  3000,
			wantTools: "# Fern's guess, from package.json.\n[tools]\nnode = \"22.11.0\"\npnpm = \"10.4.1\"\n",
		},
		{
			name: "bun without a lockfile, node from engines",
			files: map[string]string{
				"package.json": `{"scripts": {"dev": "bun --watch server.ts"}, "packageManager": "bun@1.3.0", "engines": {"node": ">=20.9"}}`,
			},
			wantRun:   "bun install && exec bun run dev",
			wantPort:  3000,
			wantTools: "# Fern's guess, from package.json.\n[tools]\nnode = \"20\"\nbun = \"1.3.0\"\n",
		},
		{
			name: "yarn through corepack, astro",
			files: map[string]string{
				"package.json": `{"scripts": {"dev": "astro dev"}, "dependencies": {"astro": "5"}}`,
				"yarn.lock":    "# yarn\n",
			},
			wantRun:   "corepack yarn install && exec corepack yarn run dev --host 0.0.0.0 --port $PORT",
			wantPort:  4321,
			wantTools: "# Fern's guess, from package.json.\n[tools]\nnode = \"lts\"\n",
		},
		{
			name: "postgres from docker-compose",
			files: map[string]string{
				"package.json": `{"scripts": {"dev": "next dev"}, "dependencies": {"next": "16"}}`,
				"docker-compose.yml": "services:\n  db:\n    image: docker.io/library/postgres:16\n" +
					"    environment:\n      POSTGRES_USER: app\n      POSTGRES_PASSWORD: ${DB_PASSWORD:-secret}\n",
			},
			wantRun:   "export DATABASE_URL=${DATABASE_URL:-postgres://app:secret@localhost:5432/app}; npm install --prefer-offline && exec npm run dev -- -H 0.0.0.0 -p $PORT",
			wantPort:  3000,
			wantTools: "# Fern's guess, from package.json.\n[tools]\nnode = \"lts\"\n",
			wantDB:    true,
		},
		{
			name: "pinned tools are the repository's",
			files: map[string]string{
				"package.json": `{"scripts": {"dev": "vite"}, "devDependencies": {"vite": "7"}}`,
				"mise.toml":    "[tools]\nnode = \"24\"\n",
			},
			wantRun:  "npm install --prefer-offline && exec npm run dev -- --host 0.0.0.0 --port $PORT --strictPort",
			wantPort: 5173,
		},
		{
			name: "a version that isn't one",
			files: map[string]string{
				"package.json": `{"scripts": {"dev": "vite"}, "packageManager": "pnpm@1\"\n[x]"}`,
				".nvmrc":       "lts/iron\n",
			},
			wantRun:   "pnpm install --prefer-offline && exec pnpm run dev",
			wantPort:  3000,
			wantTools: "# Fern's guess, from package.json.\n[tools]\nnode = \"lts\"\npnpm = \"latest\"\n",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, data := range tt.files {
				write(t, dir, name, data)
			}
			g, err := setup.MakeGuess(dir)
			if err != nil {
				t.Fatalf("MakeGuess() error: %v", err)
			}
			var got guessed
			if err := yaml.UnmarshalStrict(g.Services, &got); err != nil {
				t.Fatalf("the guessed services.yaml doesn't parse: %v\n%s", err, g.Services)
			}
			web := got.Services["web"]
			if web.Run != tt.wantRun || web.Port != tt.wantPort {
				t.Errorf("web = run %q port %d, want run %q port %d", web.Run, web.Port, tt.wantRun, tt.wantPort)
			}
			// Every command the guess runs is shell that parses.
			for name, svc := range got.Services {
				for _, script := range []string{svc.Run, svc.Stop, svc.Ready} {
					if out, err := exec.CommandContext(t.Context(), "sh", "-n", "-c", script).CombinedOutput(); err != nil {
						t.Errorf("service %s: sh -n: %v: %s\n%s", name, err, out, script)
					}
				}
			}
			if _, ok := got.Services["postgres"]; ok != tt.wantDB {
				t.Errorf("a postgres service: %t, want %t", ok, tt.wantDB)
			}
			if diff := cmp.Diff(tt.wantTools, string(g.Tools)); diff != "" {
				t.Errorf("tools (-want +got):\n%s", diff)
			}
		})
	}
}

// There is nothing to guess for a repository that writes its own
// services.yaml or isn't a Node project with a dev script.
func TestMakeGuessNone(t *testing.T) {
	for name, files := range map[string]map[string]string{
		"its own services.yaml": {"services.yaml": "services: {}\n", "package.json": `{"scripts": {"dev": "vite"}}`},
		"no dev script":         {"package.json": `{"scripts": {"build": "tsc"}}`},
		"not node":              {"go.mod": "module x\n"},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			for n, data := range files {
				write(t, dir, n, data)
			}
			g, err := setup.MakeGuess(dir)
			if err != nil || g.Services != nil || g.Tools != nil {
				t.Errorf("MakeGuess() = %q, %q, %v; want nothing", g.Services, g.Tools, err)
			}
		})
	}
}

// A guess makes a golden key, which its files change, and a default
// preview; a repository with nothing to guess still has no key.
func TestGuessKeyAndPreview(t *testing.T) {
	dir := t.TempDir()
	write(t, dir, "package.json", `{"scripts": {"dev": "vite"}, "devDependencies": {"vite": "7"}}`)
	before := key(t, dir, "box:1")
	write(t, dir, ".nvmrc", "22\n")
	if got := key(t, dir, "box:1"); got == before {
		t.Errorf("Key() after .nvmrc changed = %q, want it to change", got)
	}
	if port, err := setup.PreviewPort(dir); err != nil || port != 5173 {
		t.Errorf("PreviewPort() = %d, %v; want 5173", port, err)
	}
	tools, err := setup.ToolsKey(dir)
	if err != nil || tools == "" {
		t.Errorf("ToolsKey() = %q, %v; want the guessed tools' key", tools, err)
	}

	write(t, dir, "package.json", `{"scripts": {"build": "tsc"}}`)
	if _, err := setup.Key(dir, "box:1"); !errors.Is(err, setup.ErrNoContract) {
		t.Errorf("Key() without a dev script error = %v, want ErrNoContract", err)
	}
}
