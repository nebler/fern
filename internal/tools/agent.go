package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/coder/websocket"

	"github.com/nebler/fern/internal/actor"
	"github.com/nebler/fern/internal/boxexec"
	"github.com/nebler/fern/internal/setup"
)

// noShell are Claude's tools that run shell commands. In an agent box they
// would run without the services; Fern's Bash runs them in the services' box.
var noShell = []string{"Bash", "BashOutput", "KillShell", "KillBash", "Monitor"}

// StartAgent passes on the client's first message, the command's request,
// and if it starts an agent (Claude, or OpenCode's server), gives the agent
// Fern's MCP server for environment id (its Bash) in place of its own shell.
// It has none on a later connection to a session.
func (s *Server) StartAgent(ctx context.Context, client, box *websocket.Conn, id string, ex *actor.Exec) error {
	typ, data, err := client.Read(ctx)
	if err != nil {
		return err
	}
	var req boxexec.Request
	if typ == websocket.MessageText && json.Unmarshal(data, &req) == nil && len(req.Argv) > 0 {
		agent := path.Base(req.Argv[0])
		if agent != "claude" && agent != "opencode" {
			// Any other command starts as it is, without a question to the box.
			return box.Write(ctx, typ, data)
		}
		url, token := s.mcpURL(ctx, ex)
		mcp := fernMCP{
			URL: url + "/mcp/" + id, Token: token,
			Cloudflare: s.cloudflareToken(ctx, id), AstroDocs: s.isAstro(ctx, id),
		}
		if agent == "claude" {
			err = mcp.claude(&req)
		} else {
			err = mcp.openCode(&req, ex.Addr)
		}
		if err != nil {
			return err
		}
		if data, err = json.Marshal(req); err != nil {
			return err
		}
	}
	return box.Write(ctx, typ, data)
}

// cloudflareMCP is Cloudflare's MCP server for its whole API, which takes an
// API token as the bearer token.
const cloudflareMCP = "https://mcp.cloudflare.com/mcp"

// cloudflareToken returns the Cloudflare token for an agent in environment
// id's agent box: the node's, if its repository deploys to Cloudflare, else
// "". The agent works without it, so a failure is logged, not returned.
func (s *Server) cloudflareToken(ctx context.Context, id string) string {
	if s.Cloudflare == nil {
		return ""
	}
	token, err := s.Cloudflare(ctx)
	if err != nil {
		s.log.WarnContext(ctx, "reading the Cloudflare token", slog.Any("error", err))
		return ""
	}
	if token == "" {
		return ""
	}
	env, err := s.store.Get(ctx, id)
	if err != nil {
		s.log.WarnContext(ctx, "finding the workspace for Cloudflare's MCP server", slog.String("env", id), slog.Any("error", err))
		return ""
	}
	// The workspace is the agent's: IsCloudflare keeps its reads inside it.
	is, err := setup.IsCloudflare(env.Workspace)
	if err != nil {
		s.log.WarnContext(ctx, "reading whether the repository deploys to Cloudflare", slog.String("env", id), slog.Any("error", err))
		return ""
	}
	if !is {
		return ""
	}
	return token
}

// astroDocsMCP is Astro's MCP server for its documentation, which takes no
// login.
const astroDocsMCP = "https://mcp.docs.astro.build/mcp"

// isAstro reports whether environment id's repository is an Astro site, for
// Astro's docs server. The agent works without it, so a failure is logged.
func (s *Server) isAstro(ctx context.Context, id string) bool {
	env, err := s.store.Get(ctx, id)
	if err != nil {
		s.log.WarnContext(ctx, "finding the workspace for Astro's docs server", slog.String("env", id), slog.Any("error", err))
		return false
	}
	// The workspace is the agent's: IsAstro keeps its reads inside it.
	is, err := setup.IsAstro(env.Workspace)
	if err != nil {
		s.log.WarnContext(ctx, "reading whether the repository is an Astro site", slog.String("env", id), slog.Any("error", err))
	}
	return is
}

// fernMCP is Fern's MCP server as an agent in an agent box reaches it, and
// those for the repository: Cloudflare's for one that deploys there,
// Astro's docs for an Astro site.
type fernMCP struct {
	URL   string
	Token string // the box's, which the server takes as the bearer token
	// Cloudflare is the Cloudflare API token for Cloudflare's MCP server; ""
	// adds no server.
	Cloudflare string
	AstroDocs  bool // adds Astro's docs server
}

// t3MCPPath is where T3's server has its MCP endpoint.
const t3MCPPath = "/mcp"

// claude adds the MCP server to Claude's arguments, takes away Claude's own
// shell tools, and ignores the settings and MCP servers in the workspace:
// the box writes those, and they run code in the agent box, next to
// Claude's login.
func (m fernMCP) claude(req *boxexec.Request) error {
	fern := map[string]any{"type": "http", "url": m.URL}
	if m.Token != "" {
		fern["headers"] = map[string]string{"Authorization": "Bearer " + m.Token}
	}
	servers := map[string]any{"fern": fern}
	if m.Cloudflare != "" {
		servers["cloudflare"] = map[string]any{
			"type":    "http",
			"url":     cloudflareMCP,
			"headers": map[string]string{"Authorization": "Bearer " + m.Cloudflare},
		}
	}
	if m.AstroDocs {
		servers["astro-docs"] = map[string]any{"type": "http", "url": astroDocsMCP}
	}
	cfg, err := json.Marshal(map[string]any{"mcpServers": servers})
	if err != nil {
		return err
	}
	req.Argv = append(req.Argv, "--mcp-config", string(cfg), "--disallowedTools", strings.Join(noShell, ","),
		"--setting-sources=user")
	return nil
}

// openCodeConfig is the variable OpenCode reads settings from, as JSON, after
// its settings files.
const openCodeConfig = "OPENCODE_CONFIG_CONTENT"

// openCode changes the request of OpenCode's server, which T3 calls over
// HTTP. The server listens on the box's address, which is at addr, as T3
// can't reach the box's loopback. Its settings get the MCP server and lose
// the shell tool; a rule also refuses the tool, should a later settings file
// bring it back. Any other opencode command is left as it is.
func (m fernMCP) openCode(req *boxexec.Request, addr string) error {
	if len(req.Argv) < 2 || req.Argv[1] != "serve" {
		return nil
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("the box's address: %w", err)
	}
	req.Argv = slices.DeleteFunc(slices.Clone(req.Argv), func(arg string) bool { return strings.HasPrefix(arg, "--hostname=") })
	req.Argv = append(req.Argv, "--hostname="+host)

	cfg := map[string]any{}
	req.Environ = slices.DeleteFunc(slices.Clone(req.Environ), func(v string) bool {
		content, ok := strings.CutPrefix(v, openCodeConfig+"=")
		if ok && json.Unmarshal([]byte(content), &cfg) != nil {
			cfg = map[string]any{} // not an object: OpenCode would ignore it too
		}
		return ok
	})
	mcp, _ := cfg["mcp"].(map[string]any)
	if mcp == nil {
		mcp = map[string]any{}
	}
	servers, _ := mcp["servers"].(map[string]any)
	if servers == nil {
		servers = map[string]any{}
	}
	// Not in Code Mode, where only scripts could call it, and without OAuth,
	// which OpenCode would try if the token were refused.
	fern := map[string]any{"type": "remote", "url": m.URL, "oauth": false, "codemode": false}
	if m.Token != "" {
		fern["headers"] = map[string]string{"Authorization": "Bearer " + m.Token}
	}
	servers["fern"] = fern
	if m.Cloudflare != "" {
		servers["cloudflare"] = map[string]any{
			"type": "remote", "url": cloudflareMCP, "headers": map[string]string{"Authorization": "Bearer " + m.Cloudflare},
			"oauth": false,
		}
	}
	if m.AstroDocs {
		servers["astro-docs"] = map[string]any{"type": "remote", "url": astroDocsMCP, "oauth": false}
	}
	mcp["servers"] = servers
	cfg["mcp"] = mcp
	plugins, _ := cfg["plugins"].([]any)
	cfg["plugins"] = append(plugins, "-opencode.tool.shell")
	policies, _ := cfg["policies"].([]any)
	cfg["policies"] = append([]any{map[string]string{"action": "tool.use", "resource": "shell:*", "effect": "deny"}}, policies...)
	content, err := json.Marshal(cfg)
	if err != nil {
		return err
	}
	req.Environ = append(req.Environ, openCodeConfig+"="+string(content))
	return nil
}

// mcpURL returns where Claude in the agent box of ex reaches Fern's MCP
// server, and the token it must send, if any: through the box's own relay,
// which carries a call across a restart of the daemon and adds the box's
// token itself, so the agent is given none. An agent box still running from
// before the relay has none, and one from before 2026-11-12 has a relay that
// adds no token: their agent gets the token in its arguments, as it did.
func (s *Server) mcpURL(ctx context.Context, ex *actor.Exec) (url, token string) {
	relay, auth := boxexec.HasMCPRelay(ctx, s.boxes, ex.Addr, ex.Token)
	if !relay {
		return s.agentMCP, ex.Token
	}
	url = "http://127.0.0.1:" + strconv.Itoa(boxexec.MCPRelayPort)
	if auth {
		return url, ""
	}
	return url, ex.Token
}
