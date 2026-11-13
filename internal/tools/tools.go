// Package tools serves the tools Fern gives an agent in an agent box, as an
// MCP server per environment: Bash and its background commands, which run in
// the environment's own box, where the services are; commit; the forks; and
// cloudflare_connect. It also points an agent at them as it starts
// (StartAgent).
package tools

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/commit"
	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/store"
)

// Store reads environments' records.
type Store interface {
	Get(ctx context.Context, id string) (store.Env, error)
}

// Driver reports the state of boxes, and the token a running box was
// started with.
type Driver interface {
	States(ctx context.Context) (map[string]runsc.State, error)
	Token(id string) (string, error)
}

// Server serves the tools. Its commands run through the environments'
// actors.
type Server struct {
	store  Store
	driver Driver
	envs   *actor.Group
	log    *slog.Logger
	// agentMCP is where agent boxes reach MCPHandler, such as
	// http://fern-daemon:7071.
	agentMCP string
	// boxes asks boxes' fern-init for their jobs and their relay. A box that
	// vanished can leave an address nothing answers; the same short connect
	// timeout as readiness gets to the next wake without waiting a second on
	// that stale address.
	boxes *http.Client
	jobs  *jobs    // Claude's background commands
	calls sync.Map // call key to its running Bash command, a *call
	// schemas keeps the tools' JSON schemas: an MCP server is made for every
	// request, and working them out again each time was most of a call's
	// cost in the daemon (BenchmarkMCPCall).
	schemas *mcp.SchemaCache

	// Cloudflare returns the node's Cloudflare API token, "" while there is
	// none. With one, an agent in the agent box of a repository that deploys
	// to Cloudflare (setup.IsCloudflare) gets Cloudflare's MCP server, which
	// acts on that account. The token goes to the agent box only: never to
	// the box that runs the repository's code, nor into a workspace. May be
	// nil.
	Cloudflare func(ctx context.Context) (string, error)
	// T3 is T3's server on the node, such as http://127.0.0.1:30773; ""
	// for none. T3 gives its Claude an MCP server there, on the node's
	// loopback, which a box can't reach: an agent box's fern-init listens
	// at that address in the box and passes the calls to t3Handler's route.
	T3 string
	// GitHub sets repositories' Actions secrets (cloudflare_connect). May be
	// nil: no GitHub App.
	GitHub Secrets
	// Git makes commits in threads' worktrees, for Claude, whose box can't
	// (the commit tool). May be nil: no tool.
	Git *commit.Committer

	// Stopping is closed when the daemon begins to stop. May be nil. Then the
	// commands running in boxes are left to run: the next daemon's clients
	// take them up (boxexec's sessions).
	Stopping <-chan struct{}
}

// stopping reports whether the daemon has begun to stop.
func (s *Server) stopping() bool {
	select {
	case <-s.Stopping:
		return true
	default:
		return false
	}
}

// New returns a Server for the environments in st, whose boxes drv runs and
// whose commands envs counts. agentMCP is where agent boxes reach
// MCPHandler.
func New(st Store, drv Driver, envs *actor.Group, agentMCP string, log *slog.Logger) *Server {
	s := &Server{
		store: st, driver: drv, envs: envs, log: log, agentMCP: agentMCP,
		boxes:   &http.Client{Transport: &http.Transport{DialContext: (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext}},
		schemas: mcp.NewSchemaCache(),
	}
	s.jobs = &jobs{envs: envs, driver: drv, boxes: s.boxes, log: log, stopping: s.stopping}
	return s
}
