// Added in this fork. See "What this fork adds" in README.md.

// Package core drives herdr-core (hc), the terminal host behind the herdr
// desktop app, through its command line. The desktop app's sessions never
// reach the herdr TUI socket, so the agent_* tools cannot see them.
package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// AddressPattern is the only target form accepted: a local terminal address
// such as t1w. Names and machine/t7 forms are refused, so a call cannot reach
// another machine or a terminal picked by a guessable name.
var AddressPattern = regexp.MustCompile(`^t[0-9a-z]{1,8}$`)

const (
	// callTimeout bounds every hc call except wait, which gets its own wait
	// plus this margin.
	callTimeout = 10 * time.Second
	// sendTimeout bounds hc send, which waits for about 3 s of quiet and
	// then a few seconds for the agent to start working; it stays below the
	// roughly 40 s after which a remote MCP client gives up.
	sendTimeout = 28 * time.Second
	// maxCalls bounds the hc processes running at once; a further call
	// waits up to slotWait for one to finish and is refused after that.
	maxCalls = 4
	slotWait = 2 * time.Second
	// maxOutput caps what one hc call may write, so a huge history cannot
	// exhaust memory.
	maxOutput = 4 << 20
)

// Runner runs hc with the given arguments and returns its stdout, or an
// error. Tests replace it; New uses the hc binary.
type Runner func(ctx context.Context, args ...string) ([]byte, error)

// Client talks to one herdr-core through Runner.
type Client struct {
	run   Runner
	slots chan struct{}
}

// New returns a client for the hc binary at path. Every call first checks
// that a core listens on socket: hc starts a fresh core of its own when none
// answers (measured 2026-11-19, even for "hc server status"), and a call
// through this bridge must never start a server.
func New(path, socket string) *Client {
	return NewWithRunner(requireCore(socket, execRunner(path, socket)))
}

// DefaultSocket is where hc's default session listens: the HC_SESSION
// session, else "default", under ~/.local/state/herdr-core.
func DefaultSocket() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	session := os.Getenv("HC_SESSION")
	if session == "" {
		session = "default"
	}
	if strings.ContainsAny(session, `/\`) || session == "." || session == ".." {
		return "", fmt.Errorf("HC_SESSION %q is not a session name", session)
	}
	return filepath.Join(home, ".local", "state", "herdr-core", session, session+".sock"), nil
}

// requireCore runs next only while a core accepts connections on socket.
// A core that restarts between this check and the call can still let hc
// start one of its own; hc has no option to forbid that (0.1.21).
func requireCore(socket string, next Runner) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		dialer := net.Dialer{Timeout: 2 * time.Second}
		connection, err := dialer.DialContext(ctx, "unix", socket)
		if err != nil {
			return nil, fmt.Errorf("herdr-core is not running (no answer on %s); nothing was started or sent", socket)
		}
		_ = connection.Close()
		return next(ctx, args...)
	}
}

// NewWithRunner returns a client that calls run instead of a binary.
func NewWithRunner(run Runner) *Client {
	return &Client{run: run, slots: make(chan struct{}, maxCalls)}
}

// Error is an error hc reported itself, such as not_found or blocked.
type Error struct {
	Kind    string `json:"kind"`
	Message string `json:"message"`
	Hint    string `json:"hint"`
}

func (e *Error) Error() string {
	if e.Hint == "" {
		return fmt.Sprintf("%s: %s", e.Kind, e.Message)
	}
	return fmt.Sprintf("%s: %s (%s)", e.Kind, e.Message, e.Hint)
}

// Kind returns the hc error kind of err, or "".
func Kind(err error) string {
	var hcErr *Error
	if errors.As(err, &hcErr) {
		return hcErr.Kind
	}
	return ""
}

// Status is an agent's state as hc detects it. App is empty for a terminal
// that runs no agent.
type Status struct {
	App     string `json:"app"`
	State   string `json:"state"`
	Message string `json:"msg,omitempty"`
}

// Entry is one row of hc ls. Status is the bare state there.
type Entry struct {
	Address string `json:"address"`
	Agent   string `json:"agent"`
	Cwd     string `json:"cwd"`
	State   string `json:"state"`
	Status  string `json:"status"`
	Turn    int    `json:"turn"`
	Where   string `json:"where"`
}

// Terminal is the answer of hc get.
type Terminal struct {
	Address    string            `json:"address"`
	Cwd        string            `json:"cwd"`
	State      string            `json:"state"`
	Status     *Status           `json:"status"`
	Title      string            `json:"title"`
	Turn       int               `json:"turn"`
	Labels     map[string]string `json:"labels"`
	PID        int               `json:"pid"`
	Foreground *struct {
		PID  int      `json:"pid"`
		Name string   `json:"name"`
		Cwd  string   `json:"cwd"`
		Argv []string `json:"argv"`
	} `json:"foreground"`
}

// RunsClaude reports whether Claude Code is the terminal's foreground
// program. hc keeps the last status word after the agent exits into a
// shell nested in the terminal, so the status alone does not prove that a
// prompt reaches Claude rather than the shell.
func (t Terminal) RunsClaude() bool {
	return t.Foreground != nil && len(t.Foreground.Argv) > 0 && filepath.Base(t.Foreground.Argv[0]) == "claude"
}

// AgentState returns the agent's state, or "" when no agent runs.
func (t Terminal) AgentState() string {
	if t.Status == nil || t.Status.App == "" {
		return ""
	}
	return t.Status.State
}

// Screen is the answer of hc read.
type Screen struct {
	Text     string `json:"text"`
	Complete bool   `json:"complete"`
	How      string `json:"how"`
	Why      string `json:"why"`
}

// WaitResult is the answer of hc wait. Status is the bare state. Ended says
// that the wait's condition was met, not that the terminal ended (measured
// 2026-11-19: ended true, reason "status", status "blocked" on a live agent).
type WaitResult struct {
	Reason   string `json:"reason"`
	Status   string `json:"status"`
	Ended    bool   `json:"ended"`
	Turn     int    `json:"turn"`
	ExitCode *int   `json:"exit_code"`
	Signal   any    `json:"signal"`
}

// Exited reports whether the terminal's program has ended (measured
// 2026-11-19: reason "exit" with exit_code 0, or with signal 1 after a kill).
func (r WaitResult) Exited() bool {
	return r.Reason == "exit" || r.ExitCode != nil || r.Signal != nil
}

// SendResult is the answer of hc send. Status is the agent's status word
// when the text went in, nil when it never reported one; Started says
// whether it went working within a few seconds, nil when nobody can tell.
type SendResult struct {
	Status  *string `json:"status"`
	Started *bool   `json:"started"`
	Turn    int     `json:"turn"`
}

// List returns the live terminals.
func (c *Client) List(ctx context.Context) ([]Entry, error) {
	var answer struct {
		Terminals []Entry `json:"terminals"`
	}
	if err := c.call(ctx, callTimeout, &answer, "ls", "--json"); err != nil {
		return nil, err
	}
	return answer.Terminals, nil
}

// Get returns one terminal.
func (c *Client) Get(ctx context.Context, address string) (Terminal, error) {
	var terminal Terminal
	if err := checkAddress(address); err != nil {
		return terminal, err
	}
	err := c.call(ctx, callTimeout, &terminal, "get", "--json", "--", address)
	return terminal, err
}

// Read returns the screen plus lines of history.
func (c *Client) Read(ctx context.Context, address string, lines int) (Screen, error) {
	var screen Screen
	if err := checkAddress(address); err != nil {
		return screen, err
	}
	if lines < 0 {
		lines = 0
	}
	err := c.call(ctx, callTimeout, &screen, "read", "--json", "--lines", strconv.Itoa(lines), "--", address)
	return screen, err
}

// Wait waits up to wait for the terminal to stop working.
func (c *Client) Wait(ctx context.Context, address string, wait time.Duration) (WaitResult, error) {
	var result WaitResult
	if err := checkAddress(address); err != nil {
		return result, err
	}
	seconds := strconv.FormatFloat(wait.Seconds(), 'f', 3, 64)
	err := c.call(ctx, wait+callTimeout, &result, "wait", "--json", "--timeout", seconds, "--", address)
	return result, err
}

// Send pastes text and presses Enter. With onlyIfIdle hc types only while the
// agent is idle. hc itself never types into a blocked agent without --force,
// which this client never passes.
func (c *Client) Send(ctx context.Context, address, text string, onlyIfIdle bool) (SendResult, error) {
	var result SendResult
	if err := checkAddress(address); err != nil {
		return result, err
	}
	args := []string{"send", "--json"}
	if onlyIfIdle {
		args = append(args, "--if", "idle")
	}
	args = append(args, "--", address, text)
	err := c.call(ctx, sendTimeout, &result, args...)
	return result, err
}

func checkAddress(address string) error {
	if !AddressPattern.MatchString(address) {
		return fmt.Errorf("target must be a terminal address such as t1w, not %q", address)
	}
	return nil
}

func (c *Client) call(ctx context.Context, timeout time.Duration, into any, args ...string) error {
	if c.slots != nil {
		slotCtx, stop := context.WithTimeout(ctx, slotWait)
		select {
		case c.slots <- struct{}{}:
			stop()
			defer func() { <-c.slots }()
		case <-slotCtx.Done():
			stop()
			return fmt.Errorf("%d herdr-core calls are already running; try again in a moment", maxCalls)
		}
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	out, err := c.run(ctx, args...)
	if err != nil {
		return err
	}
	if into == nil {
		return nil
	}
	if err := json.Unmarshal(out, into); err != nil {
		return fmt.Errorf("hc %s answered with something that is not its JSON: %w", args[0], err)
	}
	return nil
}

// execRunner runs the hc binary. hc prints its own errors as JSON on stderr
// and exits non-zero; those become *Error.
func execRunner(path, socket string) Runner {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		command := exec.CommandContext(ctx, path, args...)
		// Inherited HC_* variables (terminal id, scope, terminal socket)
		// would make hc treat the bridge as a terminal of its own. HC_SOCKET
		// pins hc to the socket requireCore checked (measured 2026-11-19: it
		// overrides HOME and HC_SESSION), so the check and the call cannot
		// disagree about which core they mean.
		command.Env = withoutPrefix(os.Environ(), "HC_")
		if socket != "" {
			command.Env = append(command.Env, "HC_SOCKET="+socket)
		}
		// A child that keeps the output open must not hold the call past
		// its deadline.
		command.WaitDelay = time.Second
		var stdout, stderr limitedBuffer
		stdout.limit, stderr.limit = maxOutput, 64<<10
		command.Stdout, command.Stderr = &stdout, &stderr
		runErr := command.Run()
		if stdout.overflow {
			return nil, fmt.Errorf("hc %s wrote more than %d bytes", args[0], maxOutput)
		}
		if runErr == nil {
			return stdout.Bytes(), nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("hc %s did not answer in time: %w", args[0], ctx.Err())
		}
		var answer struct {
			Error *Error `json:"error"`
		}
		for _, stream := range [][]byte{stderr.Bytes(), stdout.Bytes()} {
			if json.Unmarshal(bytes.TrimSpace(stream), &answer) == nil && answer.Error != nil && answer.Error.Kind != "" {
				return nil, answer.Error
			}
		}
		detail := strings.TrimSpace(stderr.String())
		if len(detail) > 300 {
			detail = detail[:300]
		}
		return nil, fmt.Errorf("hc %s failed: %v %s", args[0], runErr, detail)
	}
}

func withoutPrefix(environment []string, prefix string) []string {
	kept := environment[:0:0]
	for _, entry := range environment {
		if !strings.HasPrefix(entry, prefix) {
			kept = append(kept, entry)
		}
	}
	return kept
}

// limitedBuffer keeps at most limit bytes and records an overflow instead of
// growing without bound.
type limitedBuffer struct {
	bytes.Buffer
	limit    int
	overflow bool
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	if room := b.limit - b.Len(); len(p) > room {
		b.overflow = true
		if room > 0 {
			b.Buffer.Write(p[:room])
		}
		return len(p), nil
	}
	return b.Buffer.Write(p)
}

var _ io.Writer = (*limitedBuffer)(nil)
