package hibernate

import (
	"encoding/json"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

// replayed are the control requests from Claude's SDK that set up a
// session: a restarted Claude gets the newest of each, initialize first, so
// it has the same hooks, MCP servers, model and modes as the one it
// replaces. The others (interrupt, stop_task, get_context_usage …) only
// matter to the process they went to.
var replayed = []string{
	"initialize",
	"mcp_set_servers",
	"set_model",
	"set_permission_mode",
	"set_max_thinking_tokens",
	"apply_flag_settings",
	"update_settings",
	"set_mcp_permission_mode_override",
	"set_cwd",
	"channel_enable",
	"set_chrome_browser_hints",
}

// message holds the fields of a stream-json line that stream reads.
type message struct {
	Type      string `json:"type"`
	Subtype   string `json:"subtype"`
	SessionID string `json:"session_id"`
	RequestID string `json:"request_id"`
	Request   *struct {
		Subtype string `json:"subtype"`
	} `json:"request"`
	Response *struct {
		RequestID string `json:"request_id"`
	} `json:"response"`
	TaskID string            `json:"task_id"`
	Tasks  []json.RawMessage `json:"tasks"`
}

// stream follows Claude's stream-json conversation with its SDK, to tell
// when Claude has nothing to do and how to start it again where it was. In
// reads the SDK's lines (Claude's stdin), Out Claude's (its stdout). It is
// safe for concurrent use: the two directions run in their own goroutines.
type stream struct {
	mu       sync.Mutex
	session  string              // from Claude's init message
	busy     bool                // a user message went in and no result came out since
	toClaude map[string]struct{} // the SDK's control requests that Claude hasn't answered
	toSDK    map[string]struct{} // Claude's control requests that the SDK hasn't answered
	tasks    int                 // background tasks, from Claude's newest background_tasks_changed
	running  map[string]struct{} // tasks started and not notified as ended
	setup    map[string][]byte   // the newest line of each replayed subtype
	swallow  map[string]struct{} // replayed requests whose answers the SDK mustn't see
	lastLine time.Time           // when the newest line went either way
}

func newStream() *stream {
	return &stream{
		toClaude: map[string]struct{}{},
		toSDK:    map[string]struct{}{},
		running:  map[string]struct{}{},
		setup:    map[string][]byte{},
		swallow:  map[string]struct{}{},
	}
}

// In reads a line the SDK sent to Claude.
func (s *stream) In(line []byte) {
	var m message
	ok := json.Unmarshal(line, &m) == nil
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastLine = time.Now()
	if !ok {
		return
	}
	switch m.Type {
	case "user":
		s.busy = true
	case "control_request":
		s.toClaude[m.RequestID] = struct{}{}
		if m.Request != nil && slices.Contains(replayed, m.Request.Subtype) {
			s.setup[m.Request.Subtype] = slices.Clone(line)
		}
	case "control_cancel_request":
		delete(s.toClaude, m.RequestID)
	case "control_response":
		if m.Response != nil {
			delete(s.toSDK, m.Response.RequestID)
		}
	}
}

// Out reads a line Claude sent to the SDK and says whether the SDK may see
// it: answers to replayed requests are kept from it.
func (s *stream) Out(line []byte) bool {
	var m message
	ok := json.Unmarshal(line, &m) == nil
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastLine = time.Now()
	if !ok {
		return true
	}
	switch m.Type {
	case "system":
		switch m.Subtype {
		case "init":
			s.session = m.SessionID
		case "background_tasks_changed":
			s.tasks = len(m.Tasks)
		case "task_started":
			s.running[m.TaskID] = struct{}{}
		case "task_notification":
			delete(s.running, m.TaskID)
		}
	case "result":
		s.busy = false
	case "control_request":
		s.toSDK[m.RequestID] = struct{}{}
	case "control_response":
		if m.Response == nil {
			break
		}
		id := m.Response.RequestID
		delete(s.toClaude, id)
		if _, ok := s.swallow[id]; ok {
			delete(s.swallow, id)
			return false
		}
	}
	return true
}

// idleSince says whether Claude has nothing to do, and since when: it knows
// its session, has finished its turn, waits for no answer and asks none,
// and runs no background task.
func (s *stream) idleSince() (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	idle := s.session != "" && !s.busy && len(s.toClaude) == 0 && len(s.toSDK) == 0 &&
		s.tasks == 0 && len(s.running) == 0
	return s.lastLine, idle
}

// restart returns the argv that starts Claude again in its session, and the
// setup lines to send it first; their answers are kept from the SDK. It
// forgets what the stopped Claude was running: a stop ends its tasks.
func (s *stream) restart(argv []string) ([]string, [][]byte) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.tasks = 0
	clear(s.running)
	clear(s.toClaude)
	clear(s.toSDK)
	var lines [][]byte
	for _, sub := range replayed {
		line, ok := s.setup[sub]
		if !ok {
			continue
		}
		var m message
		if json.Unmarshal(line, &m) == nil {
			s.swallow[m.RequestID] = struct{}{}
			s.toClaude[m.RequestID] = struct{}{}
		}
		lines = append(lines, line)
	}
	return resumeArgs(argv, s.session), lines
}

// sessionFlags are Claude's flags that pick or fork a session; a restart
// replaces them with --resume of the session it was in. The bool says
// whether the flag takes a value.
var sessionFlags = map[string]bool{
	"--session-id":        true,
	"--resume":            true,
	"-r":                  true,
	"--resume-session-at": true,
	"--resume-drops-turn": true,
	"--fork-session":      false,
	"--continue":          false,
	"-c":                  false,
}

func resumeArgs(argv []string, session string) []string {
	out := make([]string, 0, len(argv)+1)
	for i := 0; i < len(argv); i++ {
		name, _, hasValue := strings.Cut(argv[i], "=")
		takesValue, ok := sessionFlags[name]
		if i == 0 || !ok {
			out = append(out, argv[i])
			continue
		}
		if takesValue && !hasValue {
			i++ // its value
		}
	}
	return append(out, "--resume="+session)
}

// isStreamingClaude says whether argv runs Claude with stream-json in and
// out, as its SDK does: only such a Claude can be stopped and resumed
// unseen.
func isStreamingClaude(argv []string) bool {
	if len(argv) == 0 || filepath.Base(argv[0]) != "claude" {
		return false
	}
	return hasFlag(argv, "--input-format", "stream-json") && hasFlag(argv, "--output-format", "stream-json")
}

func hasFlag(argv []string, name, value string) bool {
	for i, a := range argv {
		if a == name+"="+value || (a == name && i+1 < len(argv) && argv[i+1] == value) {
			return true
		}
	}
	return false
}
