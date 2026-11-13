package actor

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"github.com/nebler/fern/internal/runsc"
	"github.com/nebler/fern/internal/setup"
	"github.com/nebler/fern/internal/store"
)

// boxOf returns box id of env as the driver is to start it. This is where
// it is decided what each box runs and gets:
//
//   - a golden builder's box runs the services, started at once, and gets
//     no Claude: what it writes goes into every thread of the repository.
//     For its repository's setup script (forSetup) it runs none yet;
//   - with agent boxes, an agent box runs Claude and no services, and the
//     environment's own box the services and no Claude, so it can't plant
//     settings in Claude's folder;
//   - without, the environment's box runs both.
//
// A box with Claude's folder gets Claude's login, the node's skill packs
// and the notices, read now: a new login or pack needs no restart. A box
// that runs a repository's services or setup script gets the limits the
// repository asks for, up to the node's most; an agent box the defaults.
// What can't be read makes the box unfit to start (runsc.Box.Unfit), not
// the wake fail here: the driver keeps a box that runs already.
func (g *Group) boxOf(ctx context.Context, id string, env store.Env, forSetup bool) runsc.Box {
	box := runsc.Box{ID: id, Kind: runsc.Services, Env: env}
	switch {
	case store.IsBuilder(id):
		box.Kind, box.Env.Claude = runsc.Builder, ""
		if forSetup {
			box.Kind = runsc.Setup
		}
	case store.IsAgentBox(id):
		box.Kind = runsc.Agent
	case g.cfg.Agents:
		box.Env.Claude = ""
	}

	mcp := "http://" + net.JoinHostPort(runsc.DaemonHost, strconv.Itoa(g.cfg.MCPPort))
	vars := []string{"FERN_WORKSPACE=" + env.Workspace}
	switch box.Kind {
	case runsc.Services:
	case runsc.Agent:
		vars = append(vars, "FERN_SERVICES=off", "FERN_MCP="+mcp)
	case runsc.Builder:
		vars = append(vars, "FERN_ON_DEMAND=off")
	case runsc.Setup:
		vars = append(vars, "FERN_SERVICES=later")
	}
	if box.Env.Claude != "" {
		claude, err := g.claudeVars(ctx, env)
		if err != nil {
			box.Unfit = err
			return box
		}
		vars = append(vars, "CLAUDE_CONFIG_DIR="+box.Env.Claude)
		vars = append(vars, claude...)
	}
	if box.Kind == runsc.Agent && g.cfg.T3MCP != "" {
		vars = append(vars, "FERN_T3_ADDR="+g.cfg.T3MCP, "FERN_T3_MCP="+mcp+"/t3/"+env.ID)
	}
	if g.cfg.Preview != "" {
		// {port} is for the box to fill in.
		vars = append(vars, "FERN_PREVIEW=http://{port}-"+env.ID+"."+g.cfg.Preview+"/")
	}

	asked, err := setup.AskedLimits(env.Workspace)
	if box.Kind == runsc.Agent {
		// Claude's box doesn't depend on the repository's services.yaml, which
		// Claude may be there to mend. It learns what the services' box gets,
		// to say so when the repository asks for more (fern-init's hint).
		box.Limits = runsc.DefaultLimits()
		if err == nil {
			if services, err := g.cfg.limitsOf(asked); err == nil {
				vars = append(vars, boxLimitsVar(services))
			}
		}
	} else {
		if err == nil {
			box.Limits, err = g.cfg.limitsOf(asked)
		}
		if err != nil {
			box.Unfit = err
			return box
		}
	}
	box.Vars = vars
	return box
}

// claudeVars returns what a box with Claude's folder learns at its start
// beyond the folder: Claude's login, the skill packs the node turns on, and
// what has gone wrong on the node that env's person should hear of
// (notices), which fern-init puts at the top of Claude's hint.
func (g *Group) claudeVars(ctx context.Context, env store.Env) ([]string, error) {
	var vars []string
	if g.cfg.Login != nil {
		login, err := g.cfg.Login(ctx)
		if err != nil {
			return nil, fmt.Errorf("claude login: %w", err)
		}
		if login != "" {
			vars = append(vars, "CLAUDE_CODE_OAUTH_TOKEN="+login)
		}
	}
	if g.cfg.Skills != nil {
		skills, err := g.cfg.Skills(ctx)
		if err != nil {
			return nil, fmt.Errorf("claude's skills: %w", err)
		}
		if skills != "" {
			vars = append(vars, "FERN_SKILLS="+skills)
		}
	}
	if notices := g.reporter.notices(ctx, env); len(notices) > 0 {
		data, err := json.Marshal(notices)
		if err != nil {
			return nil, err
		}
		vars = append(vars, "FERN_NOTICES="+string(data))
	}
	return vars, nil
}

// limitsOf returns the limits of a box that runs a repository's services:
// the node's, or what the repository asked for, up to the node's most. The
// workspace's services.yaml is the agent's to write, so it must not be able
// to take the whole node. A golden builder's boxes have the same: a setup
// script builds what a thread would, and one sized for the repository's box
// doesn't fit a smaller one.
func (c Config) limitsOf(asked setup.Limits) (runsc.Limits, error) {
	lim := c.Limits
	if lim == (runsc.Limits{}) {
		lim = runsc.DefaultLimits()
	}
	most := c.MaxLimits
	if most == (runsc.Limits{}) {
		most = lim
	}
	if asked.CPUs != 0 {
		lim.CPUs = min(asked.CPUs, most.CPUs)
	}
	if asked.Memory != 0 {
		lim.Memory = min(asked.Memory<<20, most.Memory)
	}
	if asked.Pids != 0 {
		lim.Pids = min(asked.Pids, most.Pids)
	}
	if err := lim.Check(); err != nil {
		return runsc.Limits{}, fmt.Errorf("services.yaml: limits: %w", err)
	}
	return lim, nil
}

// boxLimitsVar tells an agent box's fern-init what its environment's own box
// gets: CPUs, MiB of memory and processes, as FERN_BOX_LIMITS=2:2048:4096.
func boxLimitsVar(lim runsc.Limits) string {
	return fmt.Sprintf("FERN_BOX_LIMITS=%g:%d:%d", lim.CPUs, lim.Memory>>20, lim.Pids)
}
