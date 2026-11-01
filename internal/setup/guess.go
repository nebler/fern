package setup

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"regexp"
	"slices"
	"strings"

	"sigs.k8s.io/yaml"
)

// Guess is what Fern makes of a repository that has no services.yaml but
// is a Node project with a dev script: a services.yaml that runs the dev
// server (and Postgres, if docker-compose runs one), and a mise config with
// the Node and package manager to run it, which the box image doesn't have.
// A repository that writes its own services.yaml gets no guess.
type Guess struct {
	Services []byte   // a services.yaml; nil if there is nothing to run
	Tools    []byte   // a mise config for Node and the package manager
	Inputs   []string // the files the guess read, for the golden key
}

// guessInputs are the files a guess reads, at the repository's root.
var guessInputs = []string{
	"package.json", ".nvmrc", ".node-version",
	"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml",
}

// framework is a dev server's: how it is told its address, and the port it
// uses when it isn't.
type framework struct {
	pkg  string // a dependency that names it
	args string // the dev script's flags for host and port
	port int
}

// frameworks, in order: the first whose package the project depends on.
var frameworks = []framework{
	{"next", "-H 0.0.0.0 -p $PORT", 3000},
	{"nuxt", "--host 0.0.0.0 --port $PORT", 3000},
	{"astro", "--host 0.0.0.0 --port $PORT", 4321},
	// Vite and the frameworks that run it.
	{"vite", "--host 0.0.0.0 --port $PORT --strictPort", 5173},
	{"@tanstack/react-start", "--host 0.0.0.0 --port $PORT --strictPort", 3000},
	{"@sveltejs/kit", "--host 0.0.0.0 --port $PORT --strictPort", 5173},
	{"@react-router/dev", "--host 0.0.0.0 --port $PORT", 5173},
}

// packageManager is how a project installs and runs its scripts.
type packageManager struct {
	name     string
	lock     string // the lockfile that names it
	install  string // with its lockfile
	fresh    string // without one
	run      string // runs a script; args follow
	dashdash bool   // args to the script need "--"
	tool     bool   // mise installs it; npm comes with Node, yarn with corepack
}

var packageManagers = []packageManager{
	{"pnpm", "pnpm-lock.yaml", "pnpm install --frozen-lockfile --prefer-offline", "pnpm install --prefer-offline", "pnpm run", false, true},
	{"bun", "bun.lock", "bun install --frozen-lockfile", "bun install", "bun run", false, true},
	{"bun", "bun.lockb", "bun install --frozen-lockfile", "bun install", "bun run", false, true},
	{"yarn", "yarn.lock", "corepack yarn install", "corepack yarn install", "corepack yarn run", false, false},
	{"npm", "package-lock.json", "npm ci --prefer-offline", "npm install --prefer-offline", "npm run", true, false},
}

// version is what may go into the guessed mise config: a version, a range's
// first number or "lts", never TOML of the repository's making.
var versionRE = regexp.MustCompile(`^[0-9A-Za-z.\-]{1,32}$`)

// firstNumber finds a range's first number: ">=20" is 20.
var firstNumber = regexp.MustCompile(`[0-9]+`)

// MakeGuess returns the guess for the repository checked out at dir, or a
// zero Guess if it has a services.yaml or isn't a Node project with a dev
// script. The files are the agent's, read as root: readFile keeps to dir.
func MakeGuess(dir string) (Guess, error) {
	if _, err := readFile(dir, "services.yaml"); !errors.Is(err, fs.ErrNotExist) {
		return Guess{}, err // nil when the file is there
	}
	data, err := readFile(dir, "package.json")
	if errors.Is(err, fs.ErrNotExist) {
		return Guess{}, nil
	}
	if err != nil {
		return Guess{}, err
	}
	var pkg struct {
		Scripts         map[string]string `json:"scripts"`
		Dependencies    map[string]string `json:"dependencies"`
		DevDependencies map[string]string `json:"devDependencies"`
		PackageManager  string            `json:"packageManager"`
		Engines         struct {
			Node string `json:"node"`
		} `json:"engines"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return Guess{}, fmt.Errorf("package.json: %w", err)
	}
	if pkg.Scripts["dev"] == "" {
		return Guess{}, nil
	}

	pm, locked := pickPackageManager(dir, pkg.PackageManager)
	fw := framework{port: 3000}
	for _, f := range frameworks {
		if pkg.Dependencies[f.pkg] != "" || pkg.DevDependencies[f.pkg] != "" {
			fw = f
			break
		}
	}
	install := pm.fresh
	if locked {
		install = pm.install
	}
	run := pm.run + " dev"
	if fw.args != "" {
		if pm.dashdash {
			run += " --"
		}
		run += " " + fw.args
	}

	pg, err := composePostgres(dir)
	if err != nil {
		return Guess{}, err
	}
	web := install + " && exec " + run
	var services strings.Builder
	services.WriteString("# Fern's guess, from package.json: this repository has no services.yaml.\nservices:\n")
	if pg != nil {
		url := fmt.Sprintf("postgres://%s:%s@localhost:5432/%s", pg.user, pg.password, pg.db)
		web = fmt.Sprintf("export DATABASE_URL=${DATABASE_URL:-%s}; %s", url, web)
		fmt.Fprintf(&services, "  postgres:\n    run: %s\n    stop: pg_ctl stop -D .fern/pg -m fast\n    ready: test -e .fern/pg-ready && pg_isready -q -h 127.0.0.1\n",
			quote(postgresRun(pg)))
	}
	// A golden build's first start installs the dependencies; a thread's
	// only finds them installed. Ready when it listens: a first page can
	// take longer to render than a check may wait.
	fmt.Fprintf(&services, "  web:\n    run: %s\n    ready: %s\n    port: %d\n",
		quote(web), quote(`bash -c 'exec 3<>/dev/tcp/127.0.0.1/$PORT'`), fw.port)

	tools := fmt.Sprintf("# Fern's guess, from package.json.\n[tools]\nnode = %q\n", nodeVersion(dir, pkg.Engines.Node))
	if pm.tool {
		v := "latest"
		if name, ver, ok := strings.Cut(pkg.PackageManager, "@"); ok && name == pm.name {
			ver, _, _ = strings.Cut(ver, "+") // a corepack hash
			if versionRE.MatchString(ver) {
				v = ver
			}
		}
		tools += fmt.Sprintf("%s = %q\n", pm.name, v)
	}
	g := Guess{Services: []byte(services.String()), Tools: []byte(tools), Inputs: guessInputs}
	// A repository that pins its own tools keeps them.
	if has, err := hasToolFiles(dir); err != nil || has {
		g.Tools = nil
		return g, err
	}
	return g, nil
}

// pickPackageManager returns the package manager field names
// (package.json's field, as "pnpm@10.4.1"), else the one whose lockfile dir
// has, else npm; and whether its lockfile is there.
func pickPackageManager(dir, field string) (packageManager, bool) {
	name, _, _ := strings.Cut(field, "@")
	for _, pm := range packageManagers {
		if _, err := readFile(dir, pm.lock); err == nil && (name == "" || name == pm.name) {
			return pm, true
		}
	}
	for _, pm := range packageManagers {
		if pm.name == name {
			return pm, false
		}
	}
	return packageManagers[len(packageManagers)-1], false
}

// nodeVersion returns the Node version the repository asks for: .nvmrc or
// .node-version, else the first number of package.json's engines.node
// (">=20" is 20), else the current LTS.
func nodeVersion(dir, engines string) string {
	for _, p := range []string{".nvmrc", ".node-version"} {
		data, err := readFile(dir, p)
		if err != nil {
			continue
		}
		v := strings.TrimPrefix(strings.TrimSpace(string(data)), "v")
		if strings.HasPrefix(v, "lts") || v == "node" {
			return "lts"
		}
		if versionRE.MatchString(v) {
			return v
		}
	}
	if m := firstNumber.FindString(engines); m != "" {
		return m
	}
	return "lts"
}

// postgres is the database a docker-compose file runs.
type postgres struct{ user, password, db string }

// composePostgres returns the Postgres the repository's docker-compose file
// runs, with the role, password and database it creates, or nil.
func composePostgres(dir string) (*postgres, error) {
	for _, p := range guessInputs[3:] {
		data, err := readFile(dir, p)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		var f struct {
			Services map[string]struct {
				Image       string `json:"image"`
				Environment any    `json:"environment"`
			} `json:"services"`
		}
		if err := yaml.Unmarshal(data, &f); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		for _, name := range slices.Sorted(maps.Keys(f.Services)) {
			svc := f.Services[name]
			image := svc.Image[strings.LastIndex(svc.Image, "/")+1:]
			if !strings.HasPrefix(image, "postgres") && !strings.HasPrefix(image, "postgis") {
				continue
			}
			env := composeEnv(svc.Environment)
			pg := &postgres{user: env["POSTGRES_USER"], password: env["POSTGRES_PASSWORD"], db: env["POSTGRES_DB"]}
			if pg.user == "" {
				pg.user = "postgres"
			}
			if pg.password == "" {
				pg.password = "postgres"
			}
			if pg.db == "" {
				pg.db = pg.user // as the postgres image does
			}
			return pg, nil
		}
	}
	return nil, nil
}

// identifier is what may go into the guessed Postgres's SQL and URL.
var identifier = regexp.MustCompile(`^[A-Za-z0-9_]{1,63}$`)

// composeEnv returns a compose service's environment, a map or a list of
// NAME=VALUE, with ${NAME:-default} as its default; values that couldn't be
// a plain name are dropped.
func composeEnv(v any) map[string]string {
	env := make(map[string]string)
	set := func(k, val string) {
		if strings.HasPrefix(val, "${") {
			_, def, ok := strings.Cut(strings.TrimSuffix(val, "}"), ":-")
			if !ok {
				return
			}
			val = def
		}
		if identifier.MatchString(val) {
			env[k] = val
		}
	}
	switch e := v.(type) {
	case map[string]any:
		for k, val := range e {
			if s, ok := val.(string); ok {
				set(k, s)
			}
		}
	case []any:
		for _, item := range e {
			if s, ok := item.(string); ok {
				if k, val, ok := strings.Cut(s, "="); ok {
					set(k, val)
				}
			}
		}
	}
	return env
}

// postgresRun runs Postgres with its data in the environment (.fern/pg), on
// localhost:5432 and the image's socket, and once creates the role and the
// database the compose file's do; trust auth takes any password.
func postgresRun(pg *postgres) string {
	return strings.Join([]string{
		`set -e`,
		`PGDATA=.fern/pg`,
		`if [ ! -s $PGDATA/PG_VERSION ]; then rm -rf $PGDATA; mkdir -p .fern; initdb --no-sync -D $PGDATA -U postgres --auth=trust >/dev/null; fi`,
		`rm -f $PGDATA/postmaster.pid`,
		`postgres -D $PGDATA -k /tmp -c listen_addresses=127.0.0.1 &`,
		`if [ ! -e .fern/pg-ready ]; then until pg_isready -q -h 127.0.0.1; do sleep 0.2; done`,
		fmt.Sprintf(`psql -q -h 127.0.0.1 -U postgres -c "DO \$\$BEGIN IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = '%[1]s') THEN CREATE ROLE %[1]s LOGIN SUPERUSER; END IF; END\$\$"`, pg.user),
		fmt.Sprintf(`(psql -q -h 127.0.0.1 -U postgres -tAc "SELECT 1 FROM pg_database WHERE datname = '%[1]s'" | grep -q 1 || createdb -h 127.0.0.1 -U postgres -O %[2]s %[1]s)`, pg.db, pg.user),
		`touch .fern/pg-ready; fi`,
		`wait`,
	}, "\n")
}

// quote returns s as a YAML string.
func quote(s string) string {
	b, _ := json.Marshal(s) // a string always marshals
	return string(b)
}
