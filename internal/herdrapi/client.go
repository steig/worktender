// Package herdrapi talks to a running herdr over its local IPC endpoint.
//
// The wire protocol is newline-delimited JSON: one request object per line, one
// response object per line. Each call opens a short-lived connection, writes a
// request, and reads a single response. herdr injects HERDR_SOCKET_PATH into
// every plugin command, so this works whenever herdr is the one running us.
//
// Everything goes over the socket; nothing shells out to the herdr CLI.
package herdrapi

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

//go:generate go run ../gen schema.json types_gen.go

// defaultCallTimeout bounds an ordinary request/response exchange. A plugin
// action runs unattended, so a wedged server must fail rather than hang.
const defaultCallTimeout = 5 * time.Second

// callMargin is how much longer than herdr's own wait the client will hold on.
//
// Some requests ask herdr to wait, and a client deadline shorter than the wait
// it just requested would abort the call before herdr could possibly answer,
// turning "still starting" into a hard failure on exactly the slow case the
// wait exists for.
const callMargin = 10 * time.Second

// deadlineFor is how long to wait for a reply to a request that asked herdr to
// wait serverTimeoutMS milliseconds. Zero means the request has no server-side
// wait and gets the ordinary timeout.
func deadlineFor(serverTimeoutMS int) time.Duration {
	if serverTimeoutMS <= 0 {
		return defaultCallTimeout
	}
	return time.Duration(serverTimeoutMS)*time.Millisecond + callMargin
}

// Client is a connection factory for one herdr IPC endpoint. It holds no
// connection of its own; every call dials afresh.
type Client struct {
	socketPath string
}

// SocketEnv is the environment variable herdr injects into plugin commands and
// into the panes it opens, naming the endpoint of the session that started them.
const SocketEnv = "HERDR_SOCKET_PATH"

// New builds a Client from HERDR_SOCKET_PATH. It fails when the process is not
// running inside herdr.
//
// This answers "did herdr start us", which is not the same question as "is
// herdr running" — it never dials. Anything deciding whether herdr is *there*
// wants Probe.
func New() (*Client, error) {
	path := os.Getenv(SocketEnv)
	if path == "" {
		return nil, errors.New(SocketEnv + " is not set; are you running inside herdr?")
	}
	return &Client{socketPath: path}, nil
}

// DefaultSocketPath is where herdr's default session listens when nothing named
// an endpoint: $XDG_CONFIG_HOME/herdr/herdr.sock, falling back to
// ~/.config/herdr. Empty when the layout is not known — see herdrHome.
//
// This is the one place worktender knows herdr's file layout rather than its
// wire protocol.
func DefaultSocketPath() string {
	home := herdrHome()
	if home == "" {
		return ""
	}
	return filepath.Join(home, "herdr.sock")
}

// SocketPath is the endpoint herdr named, or the default session's when it
// named none. Empty when neither can be determined.
//
// It answers "which one endpoint" and is therefore not enough on its own to
// conclude that herdr is absent — see SessionSocketPaths and Probe.
func SocketPath() string {
	if path := os.Getenv(SocketEnv); path != "" {
		return path
	}
	return DefaultSocketPath()
}

// SessionSocketPaths is every endpoint a herdr on this machine could be
// listening on: the default session's, then one per named session, sorted.
//
// Named sessions are real and they are not at the default path. Verified
// against herdr 0.7.5: `herdr --session work status` reports
// $XDG_CONFIG_HOME/herdr/sessions/work/herdr.sock, alongside the default
// session's socket in the same tree.
//
// Enumerating them is what stops "the endpoint I know about is missing" from
// being read as "herdr is not running" — the same inference error as trusting
// the environment variable, one level down. A plain shell beside
// `herdr --session work`, with no default session, dials the default path,
// gets ENOENT, and would otherwise call that proof and let prune-apply remove a
// checkout that session's agent is standing in.
//
// Only session sockets, not every socket in the tree: herdr keeps a
// herdr-client.sock beside the server's, and a plugin checkout can hold
// unrelated sockets of its own — git's fsmonitor daemon leaves one. Treating
// those as evidence of a running herdr would make worktender refuse for ever on
// a machine that has none.
func SessionSocketPaths() []string {
	home := herdrHome()
	if home == "" {
		return nil
	}

	paths := []string{filepath.Join(home, "herdr.sock")}
	entries, err := os.ReadDir(filepath.Join(home, "sessions"))
	if err != nil {
		// No named sessions is the ordinary case and reads as an unreadable
		// directory; either way there is nothing more to enumerate.
		return paths
	}
	for _, e := range entries {
		if e.IsDir() {
			paths = append(paths, filepath.Join(home, "sessions", e.Name(), "herdr.sock"))
		}
	}
	sort.Strings(paths[1:])
	return paths
}

// ErrNoHerdr reports that nothing is listening on herdr's endpoint.
//
// It is deliberately a different fact from New's error. New says herdr did not
// start this process, which is the ordinary state of a plain shell and says
// nothing about whether herdr is running. This says the socket was dialled and
// answered definitively: there is no herdr.
var ErrNoHerdr = errors.New("herdr is not running")

// ErrHerdrUnknown reports a dial that failed without establishing anything —
// a timeout, a permission denial, an endpoint that cannot be addressed.
//
// Kept apart from ErrNoHerdr because only one of them may be degraded on. The
// degraded path disarms the guard sparing a checkout an agent is standing in,
// and what licenses that is proof of absence, not failure to find. A herdr
// whose accept queue is briefly full would otherwise read as gone, and
// `prune-apply` would remove the ground out from under a live agent — the same
// hole as trusting the environment variable, one layer down. So this is fatal
// for every caller, degradable or not.
var ErrHerdrUnknown = errors.New("cannot tell whether herdr is running")

// absent reports whether a dial failure proves nothing is there.
//
// Exactly one errno is proof: ENOENT. There is no socket at the path, so
// nothing ever bound one — and a running herdr always has its socket on disk.
// That is the ordinary state of a machine that does not run herdr, which is the
// case this whole path exists for.
//
// Everything else is ErrHerdrUnknown, and ECONNREFUSED most deliberately of
// all. It is tempting — a socket file with nobody accepting reads as a herdr
// that exited — and it is wrong, because it is not the only thing that produces
// it. Measured on both platforms, dialling:
//
//	                          darwin        linux
//	no socket at the path     ENOENT        ENOENT
//	a directory or a file     ENOTSOCK      ECONNREFUSED
//	a socket, no listener     ECONNREFUSED  ECONNREFUSED
//	a LIVE listener, queue full   ECONNREFUSED  timeout
//
// The last row is the one that settles it. On darwin a herdr that is running,
// listening and merely backed up refuses the connection, and no cheaper check
// tells that apart from a socket nobody is behind. Treating ECONNREFUSED as
// proof would let a busy herdr read as gone — and proof of absence is what
// re-enables removing a checkout an agent is standing in. Retrying does not
// help: a herdr under sustained load stays refused.
//
// ENOENT alone also happens to be the only classification that gives the same
// answer on both platforms for every row above, so the destructive path cannot
// unlock on one and not the other.
//
// What this costs: a herdr killed without cleaning up leaves its socket behind,
// and worktender then refuses rather than degrading. That is the right way
// round — refusing prints an error a person can act on, degrading wrongly
// deletes an agent's checkout — and Probe says how to clear it.
func absent(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

// Probe establishes whether herdr is actually there, by dialling and hanging up.
//
// This is the check that has to stand behind any decision to degrade. Reading
// SocketEnv cannot: the variable is unset in the user's own terminal while herdr
// runs behind it holding workspaces and agents, and it is left set in a shell
// that outlived the herdr that exported it. Both readings are wrong, and the
// second is wrong in the direction that removes a checkout.
//
// A connect, not a request: it separates running from not-running, which is the
// only distinction the caller is making, and it costs a syscall pair on a live
// socket and an immediate ENOENT on a dead one. dialHerdr's 2s timeout only
// applies to a socket that exists with a listener too backed up to accept, and
// no cheaper check tells that apart from a healthy one — which is why that case
// comes back as ErrHerdrUnknown rather than as absence.
//
// When herdr named an endpoint, that endpoint is the whole question: herdr told
// us where it is, so a live one is it and a broken one is a broken herdr. The
// single exception is ENOENT, which means the variable is stale rather than
// authoritative — a shell outliving the session that exported it — and a stale
// name is no evidence about the machine, so the search falls through to it.
//
// With nothing named, absence has to be established against every endpoint a
// herdr could be listening on, not just the default one. See SessionSocketPaths.
func Probe() (*Client, error) {
	if named := os.Getenv(SocketEnv); named != "" {
		conn, err := dialHerdr(named)
		if err == nil {
			_ = conn.Close()
			return &Client{socketPath: named}, nil
		}
		if !absent(err) {
			// The endpoint is there and would not talk. Naming the way out
			// matters here in a way it does not for a timeout: the likeliest
			// cause is a herdr that died without unlinking its socket, and that
			// state persists until somebody clears it.
			return nil, fmt.Errorf("%w: %w; if herdr is not running, remove %s and try again",
				ErrHerdrUnknown, err, named)
		}
	}
	return probeSessions()
}

// probeSessions dials every endpoint a herdr could be on and insists the answer
// be unambiguous.
//
// Absence is proven only by having looked everywhere and found nothing at all:
// at least one endpoint examined, and every one of them missing. Nothing to
// examine is not an empty search — it is not knowing where to look, which is
// windows, and it refuses.
//
// Two live sessions refuse as well, and that is the point rather than an
// awkwardness. Guessing between them is not a smaller error than guessing that
// herdr is absent: connecting to the wrong session lists the wrong workspaces,
// so the checkout an agent is standing in reads as held by nobody and
// prune-apply removes it. Same failure, different door.
func probeSessions() (*Client, error) {
	paths := SessionSocketPaths()
	if len(paths) == 0 {
		return nil, fmt.Errorf("%w: no %s is set and herdr's endpoint cannot be located on this platform",
			ErrHerdrUnknown, SocketEnv)
	}

	var (
		live      []string
		firstErr  error
		allAbsent = true
	)
	for _, path := range paths {
		conn, err := dialHerdr(path)
		if err == nil {
			_ = conn.Close()
			live = append(live, path)
			continue
		}
		if !absent(err) {
			allAbsent = false
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	switch {
	case len(live) == 1:
		return &Client{socketPath: live[0]}, nil
	case len(live) > 1:
		return nil, fmt.Errorf("%w: %d herdr sessions are running (%s); set %s to say which one",
			ErrHerdrUnknown, len(live), strings.Join(live, ", "), SocketEnv)
	case !allAbsent:
		return nil, fmt.Errorf("%w: %w; if herdr is not running, remove the stale socket and try again",
			ErrHerdrUnknown, firstErr)
	}
	return nil, fmt.Errorf("%w: nothing is listening on %s", ErrNoHerdr, strings.Join(paths, " or "))
}

// NewWithSocket points a Client at an explicit endpoint. Tests use it to talk
// to a fake server.
func NewWithSocket(path string) *Client {
	return &Client{socketPath: path}
}

// request is one JSON-RPC-style message sent to herdr.
type request struct {
	ID     string         `json:"id"`
	Method string         `json:"method"`
	Params map[string]any `json:"params"`
}

// Error is the code and human message herdr returns on failure.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string { return fmt.Sprintf("herdr: %s (%s)", e.Message, e.Code) }

// response is one JSON line returned by herdr. Exactly one of Result or Error
// is populated.
type response struct {
	ID     string          `json:"id"`
	Result json.RawMessage `json:"result"`
	Error  *Error          `json:"error"`
}

// deadliner is implemented by connections that support timeouts. The Windows
// endpoint is a file handle and may not, so the deadline is best-effort.
type deadliner interface{ SetDeadline(time.Time) error }

// call sends one request over a fresh connection and decodes the result into
// out, which may be nil when the payload does not matter.
func (c *Client) call(method string, params map[string]any, out any) error {
	return c.callWithin(method, params, out, defaultCallTimeout)
}

// callWithin is call with an explicit deadline, for requests that ask herdr to
// wait longer than an ordinary exchange.
func (c *Client) callWithin(method string, params map[string]any, out any, timeout time.Duration) error {
	conn, err := dialHerdr(c.socketPath)
	if err != nil {
		return fmt.Errorf("connect herdr IPC endpoint: %w", err)
	}
	defer conn.Close()

	if d, ok := conn.(deadliner); ok {
		_ = d.SetDeadline(time.Now().Add(timeout))
	}

	if params == nil {
		params = map[string]any{}
	}
	// Encode appends a trailing newline, which is exactly herdr's framing.
	if err := json.NewEncoder(conn).Encode(request{ID: "worktender", Method: method, Params: params}); err != nil {
		return fmt.Errorf("write %s request: %w", method, err)
	}

	var resp response
	if err := json.NewDecoder(bufio.NewReader(conn)).Decode(&resp); err != nil {
		return fmt.Errorf("read %s response: %w", method, err)
	}
	if resp.Error != nil {
		return resp.Error
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(resp.Result, out); err != nil {
		return fmt.Errorf("decode %s result: %w", method, err)
	}
	return nil
}

// WorktreeList returns every git worktree herdr knows about for the repository
// containing cwd. An empty cwd lets herdr pick the focused workspace's repo.
func (c *Client) WorktreeList(cwd string) (*WorktreeListResponse, error) {
	params := map[string]any{}
	if cwd != "" {
		params["cwd"] = cwd
	}
	var out WorktreeListResponse
	if err := c.call("worktree.list", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WorkspaceList returns every open workspace, including its agent status.
func (c *Client) WorkspaceList() (*WorkspaceListResponse, error) {
	var out WorkspaceListResponse
	if err := c.call("workspace.list", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// AgentList returns every agent herdr is tracking, across all workspaces. The
// pane ids are what identify a workspace as staffed.
func (c *Client) AgentList() (*AgentListResponse, error) {
	var out AgentListResponse
	if err := c.call("agent.list", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PaneList returns the panes of one workspace, in herdr's order.
func (c *Client) PaneList(workspaceID string) (*PaneListResponse, error) {
	params := map[string]any{}
	if workspaceID != "" {
		params["workspace_id"] = workspaceID
	}
	var out PaneListResponse
	if err := c.call("pane.list", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PluginList returns every installed plugin, with the commit herdr recorded when
// it installed each one.
//
// That commit is why this exists. herdr records it at install time and never
// re-reads the checkout, so it is the only way to see that `plugin list` is
// naming a commit an in-place update has since replaced. The manifest VERSION
// beside it is re-read, so the two can disagree — measured against 0.7.5.
func (c *Client) PluginList() (*PluginListResponse, error) {
	var out PluginListResponse
	if err := c.call("plugin.list", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// WorktreeOpen opens an existing checkout into a herdr workspace. focus is
// deliberately a parameter: adopting a batch of worktrees must not yank the
// user between workspaces.
func (c *Client) WorktreeOpen(cwd, path, label string, focus bool) error {
	return c.call("worktree.open", map[string]any{
		"cwd": cwd, "path": path, "label": label, "focus": focus,
	}, nil)
}

// WorktreeCreate makes a new checkout and opens it as a workspace in one call,
// answering with the workspace and its root pane — so a caller that is about to
// staff the pane does not have to go and look it up.
//
// An empty base lets herdr pick; callers that care pass gitx.BaseRef. focus is a
// parameter for the reason it is on WorktreeOpen: starting work in the
// background must not yank the user out of what they are doing.
func (c *Client) WorktreeCreate(cwd, branch, base, label string, focus bool) (*WorkspaceCreatedResponse, error) {
	params := map[string]any{"cwd": cwd, "branch": branch, "label": label, "focus": focus}
	if base != "" {
		params["base"] = base
	}
	var out WorkspaceCreatedResponse
	if err := c.call("worktree.create", params, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PaneSendText types text into a pane. It does NOT submit it — see PaneSendKeys.
//
// This rather than agent.prompt, which blocks against an agent herdr still has
// as launch_pending — a state a live agent can sit in indefinitely, so the
// prompt never lands. Typing at the pane does not consult agent state at all.
//
// A trailing newline is not a submit and cannot be relied on as one. Measured
// against herdr 0.7.5 and Claude Code: a payload of any size arrives at the TUI
// as one burst, the TUI reads a burst as a paste, and a newline inside a paste
// is inserted in the composer as a literal line break. The text sits there
// unsent while this call returns ok — ok for the bytes herdr delivered, not for
// a prompt the agent received.
//
// Text should still be ONE line, because that is the property the caller
// controls: whether a newline submits depends on how the TUI classified the
// burst, and untrusted text does not get to make that choice either way.
func (c *Client) PaneSendText(paneID, text string) error {
	return c.call("pane.send_text", map[string]any{
		"pane_id": paneID, "text": text,
	}, nil)
}

// PaneSendKeys delivers key events to a pane — "enter" being the one that
// submits what PaneSendText typed.
//
// A key is not text: it is acted on rather than inserted, which is the entire
// difference between this and ending the text with a newline. What it is not is
// guaranteed to be acted on. A TUI that has not finished starting drops the key
// outright — measured against Claude Code 2.1.220, which renders a paste into
// its composer seconds before it will submit one — so a caller that needs the
// key to have had an effect has to watch for the effect. See submitBrief.
func (c *Client) PaneSendKeys(paneID string, keys []string) error {
	return c.call("pane.send_keys", map[string]any{
		"pane_id": paneID, "keys": keys,
	}, nil)
}

// WorktreeRemove removes the worktree held open by a workspace, closing the
// workspace with it.
//
// force is what lets herdr tear down a workspace that still has panes, but it
// also bypasses git's own refusal to delete a dirty checkout — so callers must
// re-check for uncommitted changes immediately before calling this.
func (c *Client) WorktreeRemove(workspaceID string, force bool) error {
	return c.call("worktree.remove", map[string]any{
		"workspace_id": workspaceID, "force": force,
	}, nil)
}

// AgentGet resolves one agent target — a name or a pane id, whichever the
// caller has — to the agent herdr is tracking. It fails with agent_not_found
// when the target names nothing, which is also what a pane whose agent has
// exited looks like.
func (c *Client) AgentGet(target string) (*AgentInfoResponse, error) {
	var out AgentInfoResponse
	if err := c.call("agent.get", map[string]any{"target": target}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PaneRead returns a snapshot of a pane's terminal output.
//
// ReadSourceRecentUnwrapped is the source worth reaching for when the text is
// going to be PARSED: the other sources return the buffer as the terminal wrapped
// it, so a line longer than the pane is wide arrives split, and a parser looking
// for whole lines sees two halves of one.
func (c *Client) PaneRead(paneID string, source ReadSource) (*PaneReadResponse, error) {
	var out PaneReadResponse
	if err := c.call("pane.read", map[string]any{"pane_id": paneID, "source": source}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PaneGet returns one pane, including the metadata tokens attached to it.
//
// Unlike AgentGet it does not require the pane to have an agent, which is why
// it is the read side of the metadata channel: a report has to be confirmable
// in a pane herdr is not tracking an agent for.
func (c *Client) PaneGet(paneID string) (*PaneInfoResponse, error) {
	var out PaneInfoResponse
	if err := c.call("pane.get", map[string]any{"pane_id": paneID}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// PaneReportMetadata attaches metadata tokens to a pane. A nil value clears its
// key; any other value must be a string.
//
// Measured against herdr 0.7.5, because the schema states none of it:
//
//   - One token map per pane, shared by every writer. `source` is provenance,
//     not a namespace, so callers namespace their own keys or they collide.
//   - A write merges; the only way to retire a key is to send it as null.
//   - A value over 80 runes is cut, and control characters are stripped, both
//     silently — the call still returns ok.
//   - `seq` rejects an out-of-order write and also returns ok. Not sent.
//
// So a caller needing guaranteed delivery has to read the tokens back and
// compare them.
func (c *Client) PaneReportMetadata(paneID, source string, tokens map[string]any) error {
	return c.call("pane.report_metadata", map[string]any{
		"pane_id": paneID, "source": source, "tokens": tokens,
	}, nil)
}

// AgentStart starts an agent in a pane. timeoutMS is herdr's own launch bound;
// zero leaves herdr's default in place.
//
// IT DOES NOT COVER PANE AVAILABILITY, whatever this comment used to say.
// Measured against protocol 17: agent.start against a pane running anything but
// its shell answers agent_pane_busy in 1.6-3.0ms, identically with timeoutMS
// unset, 1000, 60000 and 120000. So a caller staffing a worktree seconds old —
// still in direnv, nix or a login banner — gets an immediate refusal and has to
// do its own waiting. execute.staff is where that happens.
func (c *Client) AgentStart(name, kind, paneID string, args []string, timeoutMS int) error {
	params := map[string]any{"name": name, "kind": kind, "pane_id": paneID}
	if len(args) > 0 {
		params["args"] = args
	}
	if timeoutMS > 0 {
		params["timeout_ms"] = timeoutMS
	}
	// herdr may hold this request open for timeoutMS while it waits for the
	// pane, so the client has to outlast the wait it just asked for.
	return c.callWithin("agent.start", params, nil, deadlineFor(timeoutMS))
}
