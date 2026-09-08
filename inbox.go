package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/steig/worktender/internal/jsonout"
	"github.com/steig/worktender/internal/safetext"
)

// The inbox is a global, threaded, append-only log for agent-to-agent
// messages that need to survive past a live SendMessage — a coordinator or a
// freshly spun-up worker asking "anything about X" later, even if it was not
// the original recipient and was not running when the message was sent.
//
// It is deliberately NOT wired to any live messaging transport. worktender has
// no hook into that (herdr/tmux-level pane messaging, not something this
// plugin intercepts), and auto-logging every message would be a privacy/scope
// decision this plugin should not make silently. An agent that wants a
// message durable calls both its live channel and `inbox post` — composable,
// not automatic.
//
// Storage is one NDJSON file per thread, under this plugin's state directory:
// <state dir>/inbox/<thread-id>.ndjson. Thread ids are caller-supplied and
// freeform — an issue number or a task slug is the convention, but nothing
// here enforces or interprets one, the same posture `report` already takes
// toward its note.
//
// Retention: none. Append-only, unbounded. That is a known gap, not an
// oversight — see the issue this shipped against for why.

const inboxUsage = "usage: worktender inbox <post|read|search>"

func inboxCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return usagef("%s", inboxUsage)
	}
	switch args[0] {
	case "post":
		return inboxPostCommand(args[1:], out)
	case "read":
		return inboxReadCommand(args[1:], out)
	case "search":
		return inboxSearchCommand(args[1:], out)
	default:
		return usagef("unknown inbox subcommand %q; %s", args[0], inboxUsage)
	}
}

// inboxThreadIDLimit and inboxFromLimit are conservative, arbitrary caps —
// there is no format they have to fit, unlike a report's note, which chunks
// into herdr pane-metadata slots. They exist only so one caller cannot make a
// thread file, or an agent name, unreasonably large.
const inboxThreadIDLimit = 100

// inboxFromLimit bounds who a message is from.
const inboxFromLimit = 100

// inboxNoteLimit is deliberately larger than a report's 200-character cap: a
// report's limit comes from herdr's pane-metadata token size, which does not
// apply here. It still needs *a* bound, so one caller cannot balloon a shared
// file — and a message everyone else reading the thread has to scroll past —
// without limit. 4000 runes is enough for a real paragraph of context and
// nowhere near enough to be a file dump.
const inboxNoteLimit = 4000

const (
	inboxPostUsage   = "usage: worktender inbox post --thread <id> --from <name> --note <text> [--json]"
	inboxReadUsage   = "usage: worktender inbox read --thread <id> [--json]"
	inboxSearchUsage = "usage: worktender inbox search [--json] <query>"
)

func inboxPostCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("inbox post", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	thread := fs.String("thread", "", "thread id to post to, e.g. an issue number or task slug")
	from := fs.String("from", "", "who is posting")
	note := fs.String("note", "", fmt.Sprintf("at most %d characters", inboxNoteLimit))
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, inboxPostUsage)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), inboxPostUsage)
	}
	if err := validThreadID(*thread); err != nil {
		return err
	}
	if err := validInboxText("--from", *from, inboxFromLimit); err != nil {
		return err
	}
	if err := validInboxText("--note", *note, inboxNoteLimit); err != nil {
		return err
	}

	dir, err := inboxDir()
	if err != nil {
		return err
	}

	msg := inboxMessage{From: *from, Note: *note, At: time.Now().UTC().Format(time.RFC3339)}
	if err := appendInboxMessage(dir, *thread, msg); err != nil {
		return err
	}

	if *asJSON {
		return jsonout.Write(out, inboxEntry{Thread: *thread, From: msg.From, Note: msg.Note, At: msg.At})
	}
	fmt.Fprintf(out, "posted to thread %s: %s\n", *thread, renderInboxLine(msg))
	return nil
}

func inboxReadCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("inbox read", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	thread := fs.String("thread", "", "thread id to read")
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, inboxReadUsage)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), inboxReadUsage)
	}
	if err := validThreadID(*thread); err != nil {
		return err
	}

	dir, err := inboxDir()
	if err != nil {
		return err
	}
	messages, err := readThreadMessages(dir, *thread)
	if err != nil {
		return err
	}

	if *asJSON {
		if messages == nil {
			messages = []inboxMessage{}
		}
		return jsonout.Write(out, inboxReadJSON{Thread: *thread, Messages: messages})
	}
	if len(messages) == 0 {
		fmt.Fprintf(out, "no messages in thread %s\n", *thread)
		return nil
	}
	for _, m := range messages {
		fmt.Fprintln(out, renderInboxLine(m))
	}
	return nil
}

func inboxSearchCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("inbox search", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	asJSON := jsonFlag(fs)

	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, inboxSearchUsage)
	}
	if fs.NArg() != 1 {
		return usagef("inbox search takes exactly one query argument; %s", inboxSearchUsage)
	}
	query := fs.Arg(0)
	if query == "" {
		return usagef("query must not be empty; %s", inboxSearchUsage)
	}

	dir, err := inboxDir()
	if err != nil {
		return err
	}
	entries, err := searchInbox(dir, query)
	if err != nil {
		return err
	}

	if *asJSON {
		if entries == nil {
			entries = []inboxEntry{}
		}
		return jsonout.Write(out, inboxSearchJSON{Query: query, Entries: entries})
	}
	if len(entries) == 0 {
		fmt.Fprintf(out, "no matches for %q\n", query)
		return nil
	}
	for _, e := range entries {
		fmt.Fprintf(out, "%s: %s\n", e.Thread, renderInboxLine(inboxMessage{From: e.From, Note: e.Note, At: e.At}))
	}
	return nil
}

// renderInboxLine is the one place a stored message becomes a line of
// terminal text. safetext.Escape rather than a rejection at this end: the
// unsafe class is refused at write time (validInboxText), so this is a second
// line of defence against a hand-edited or pre-this-change file, the same
// posture the listings take toward a name already on disk.
func renderInboxLine(m inboxMessage) string {
	return fmt.Sprintf("[%s] %s: %s", safetext.Escape(m.At), safetext.Escape(m.From), safetext.Escape(m.Note))
}

// validThreadID keeps a caller-supplied id inside the inbox directory no
// matter what. The charset excludes every path separator on unix and windows
// alike, so appending ".ndjson" can never resolve outside <inbox dir>/ —
// there is no character in the allowed set that could ask for a parent
// directory. "." and "." alone are refused anyway, for a reader's sake rather
// than a filesystem's.
func validThreadID(id string) error {
	if id == "" {
		return usagef("--thread is required; %s", inboxUsage)
	}
	if n := utf8.RuneCountInString(id); n > inboxThreadIDLimit {
		return fmt.Errorf("--thread is %d characters; the limit is %d", n, inboxThreadIDLimit)
	}
	if id == "." || id == ".." {
		return fmt.Errorf("--thread %q is not a valid thread id", id)
	}
	for _, r := range id {
		if !isThreadIDRune(r) {
			return fmt.Errorf("--thread %q contains %U; a thread id is letters, digits, '-', '_' and '.' only", id, r)
		}
	}
	return nil
}

func isThreadIDRune(r rune) bool {
	switch {
	case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		return true
	case r == '-' || r == '_' || r == '.':
		return true
	}
	return false
}

// validInboxText applies the note's safety rules to any single-line inbox
// field. Rejected rather than truncated or escaped, for the reason reportNote
// rejects: truncation invisibly loses the end of a message, and only the
// author can re-summarise. The unsafe class matches reportNote's — a newline
// opens a line of its own past the rendering above, a bidi override renders as
// something other than what it is.
func validInboxText(flagName, value string, limit int) error {
	if strings.TrimSpace(value) == "" {
		return usagef("%s is required; %s", flagName, inboxUsage)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s is not valid UTF-8", flagName)
	}
	for i, r := range value {
		if safetext.IsUnsafe(r) {
			return fmt.Errorf("%s contains %U at byte %d; it must be a single line of plain text", flagName, r, i)
		}
	}
	if n := utf8.RuneCountInString(value); n > limit {
		return fmt.Errorf("%s is %d characters; the limit is %d", flagName, n, limit)
	}
	return nil
}

// inboxDir resolves and creates <plugin state dir>/inbox.
//
// Unlike the repository lock, which is fine proceeding unserialised when the
// state directory is unusable, the inbox has no in-memory fallback to degrade
// to — durability at a shared location is the entire feature — so an unusable
// state directory is an error here.
func inboxDir() (string, error) {
	base := stateDir()
	if base == "" {
		base = defaultStateDir()
	}
	if base == "" {
		return "", withCode(exitEnvironment, errors.New(
			"no plugin state directory: HERDR_PLUGIN_STATE_DIR is unset and no default location could be derived"))
	}

	dir := filepath.Join(base, "inbox")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", withCode(exitEnvironment, fmt.Errorf("create inbox directory: %w", err))
	}
	return dir, nil
}

// defaultStateDir stands in for HERDR_PLUGIN_STATE_DIR when it is unset.
//
// That variable is only ever injected into the four registration points that
// have one — a plugin action, an event hook, startup (see stateDir and
// docs/reference.md) — never into an agent's own pane. Verified live: a pane
// herdr starts carries HERDR_PANE_ID and HERDR_SOCKET_PATH, nothing about
// plugin state. Without this fallback, `inbox` would only ever work from
// those four entry points and never from the workers it exists for, which
// call it directly from their own shell the way they call `report`.
//
// $XDG_STATE_HOME/herdr/plugins/steig.worktender, falling back to
// ~/.local/state/herdr/plugins/steig.worktender — the layout
// internal/herdrapi's herdrHome assumes for herdr's own config directory, one
// level namespaced further for a plugin's state. Empty when there is no home
// directory to derive it from, which inboxDir treats as "cannot tell" and
// refuses rather than guesses.
func defaultStateDir() string {
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(base, "herdr", "plugins", "steig.worktender")
}

func threadPath(dir, thread string) string {
	return filepath.Join(dir, thread+".ndjson")
}

// appendInboxMessage adds one line to a thread's file, creating it on the
// first message.
//
// The write is a single write(2) of the whole encoded line, to a file opened
// with O_APPEND — the ordinary way independent processes append to one local
// file without interleaving each other's writes. That is a property of local
// filesystems; it is not verified here for a network filesystem, and NDJSON's
// self-delimiting lines are the fallback if it is ever violated — a torn write
// breaks one line, not the file.
func appendInboxMessage(dir, thread string, msg inboxMessage) error {
	line, err := encodeInboxLine(msg)
	if err != nil {
		return err
	}

	file, err := os.OpenFile(threadPath(dir, thread), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open thread %q: %w", thread, err)
	}
	defer file.Close()

	if _, err := file.Write(line); err != nil {
		return fmt.Errorf("write to thread %q: %w", thread, err)
	}
	return nil
}

// encodeInboxLine renders one NDJSON line. HTML escaping is off for the same
// reason jsonout.Write turns it off: this is data an agent will grep and hand
// back, not a page, and `&` silently becoming `&` on disk would make
// `inbox search` miss a literal `&` a caller typed.
func encodeInboxLine(msg inboxMessage) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(msg); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// readThreadMessages reads a thread in order. A thread nobody has posted to
// yet is empty, not an error — an agent asking "anything about X" for a
// thread that never existed is the ordinary case this exists to answer.
func readThreadMessages(dir, thread string) ([]inboxMessage, error) {
	raw, err := os.ReadFile(threadPath(dir, thread))
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read thread %q: %w", thread, err)
	}
	return decodeInboxLines(thread, raw)
}

func decodeInboxLines(thread string, raw []byte) ([]inboxMessage, error) {
	var messages []inboxMessage
	scanner := bufio.NewScanner(bytes.NewReader(raw))
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		var msg inboxMessage
		if err := json.Unmarshal(line, &msg); err != nil {
			return nil, fmt.Errorf("thread %q is corrupt: %w", thread, err)
		}
		messages = append(messages, msg)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read thread %q: %w", thread, err)
	}
	return messages, nil
}

// inboxThreads lists every thread that has ever been posted to, oldest name
// first by nothing more meaningful than sort order — os.ReadDir already
// returns filesystem entries sorted by name, so this is just naming what
// falls out of that rather than promising an ordering of its own.
func inboxThreads(dir string) ([]string, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("list inbox: %w", err)
	}

	var threads []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if name, ok := strings.CutSuffix(e.Name(), ".ndjson"); ok {
			threads = append(threads, name)
		}
	}
	sort.Strings(threads)
	return threads, nil
}

// searchInbox is a plain substring match across every thread's from and note
// fields, across every thread there is — there is no index, and none is
// warranted until real growth makes one worth it.
func searchInbox(dir, query string) ([]inboxEntry, error) {
	threads, err := inboxThreads(dir)
	if err != nil {
		return nil, err
	}

	var entries []inboxEntry
	for _, thread := range threads {
		messages, err := readThreadMessages(dir, thread)
		if err != nil {
			return nil, err
		}
		for _, m := range messages {
			if strings.Contains(m.From, query) || strings.Contains(m.Note, query) {
				entries = append(entries, inboxEntry{Thread: thread, From: m.From, Note: m.Note, At: m.At})
			}
		}
	}
	return entries, nil
}
