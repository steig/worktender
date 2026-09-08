package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// withInboxState points HERDR_PLUGIN_STATE_DIR at a disposable directory, the
// same isolation fakeSession gives the herdr-backed commands. Inherited, it
// would fall through to defaultStateDir and touch a developer's real
// ~/.local/state/herdr/plugins/steig.worktender.
func withInboxState(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HERDR_PLUGIN_STATE_DIR", dir)
	return dir
}

func TestValidThreadIDRejectsWhatWouldEscapeTheInboxDirectory(t *testing.T) {
	for _, tc := range []struct {
		name string
		id   string
		want string
	}{
		{"empty", "", "--thread is required"},
		{"dot", ".", "not a valid thread id"},
		{"dot dot", "..", "not a valid thread id"},
		{"forward slash", "169/../../etc/passwd", "contains"},
		{"backslash", `169\..\..\etc`, "contains"},
		{"embedded null", "169\x00", "contains"},
		{"space", "issue 169", "contains"},
		{"too long", strings.Repeat("a", inboxThreadIDLimit+1), "the limit is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validThreadID(tc.id)
			if err == nil {
				t.Fatalf("validThreadID(%q) = nil, want an error", tc.id)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should contain %q", err, tc.want)
			}
		})
	}
}

func TestValidThreadIDAcceptsTheDocumentedConventions(t *testing.T) {
	for _, id := range []string{"169", "issue-169", "epic.auth", "fix_the_thing", "A1"} {
		if err := validThreadID(id); err != nil {
			t.Errorf("validThreadID(%q) = %v, want nil", id, err)
		}
	}
}

// A thread id that survives validation must never resolve outside the inbox
// directory, however threadPath builds the filename from it.
func TestThreadPathNeverEscapesTheDirectoryItIsGiven(t *testing.T) {
	dir := t.TempDir()
	for _, id := range []string{"169", "issue-169", "a.b.c", "___"} {
		if err := validThreadID(id); err != nil {
			t.Fatalf("validThreadID(%q): %v", id, err)
		}
		path := threadPath(dir, id)
		if filepath.Dir(path) != dir {
			t.Errorf("threadPath(%q, %q) = %q, escaped %q", dir, id, path, dir)
		}
	}
}

func TestValidInboxTextRejectsWhatWouldBreakRendering(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value string
		want  string
	}{
		{"empty", "", "is required"},
		{"whitespace only", "   ", "is required"},
		{"not valid utf8", "ok \xff\xfe", "not valid UTF-8"},
		{"newline", "line one\nline two", "single line of plain text"},
		{"carriage return", "line one\rline two", "single line of plain text"},
		{"right-to-left override", "hidden \u202etext", "single line of plain text"},
		{"over cap", strings.Repeat("a", inboxNoteLimit+1), "the limit is"},
		{"over cap in runes not bytes", strings.Repeat("é", inboxNoteLimit+1), "the limit is"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := validInboxText("--note", tc.value, inboxNoteLimit)
			if err == nil {
				t.Fatalf("validInboxText(%q) = nil, want an error", tc.value)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q should contain %q", err, tc.want)
			}
		})
	}
}

func TestInboxPostRequiresAPluginStateDirectory(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", "")
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", "")

	var out bytes.Buffer
	err := inboxCommand([]string{"post", "--thread", "169", "--from", "a", "--note", "n"}, &out)
	if err == nil {
		t.Fatal("inbox post with no derivable state directory should fail, not silently pick one")
	}
	if exitCode(err) != exitEnvironment {
		t.Errorf("exit code = %d, want %d (exitEnvironment)", exitCode(err), exitEnvironment)
	}
}

// The core round trip: post a few messages, read them back in order, and see
// nothing that was never posted.
func TestInboxPostThenReadRoundTrips(t *testing.T) {
	withInboxState(t)

	for _, note := range []string{"first", "second", "third"} {
		var out bytes.Buffer
		if err := inboxCommand([]string{"post", "--thread", "169", "--from", "worker-a", "--note", note}, &out); err != nil {
			t.Fatalf("post %q: %v", note, err)
		}
	}

	var out bytes.Buffer
	if err := inboxCommand([]string{"read", "--thread", "169"}, &out); err != nil {
		t.Fatalf("read: %v", err)
	}

	got := out.String()
	lines := strings.Split(strings.TrimRight(got, "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("read %d lines, want 3:\n%s", len(lines), got)
	}
	for i, note := range []string{"first", "second", "third"} {
		if !strings.Contains(lines[i], note) {
			t.Errorf("line %d = %q, want it to contain %q (order matters)", i, lines[i], note)
		}
		if !strings.Contains(lines[i], "worker-a") {
			t.Errorf("line %d = %q, want it to name the author", i, lines[i])
		}
	}
}

// A thread nobody posted to is empty, not an error — this is the ordinary
// "anything about X" case a fresh worker asks.
func TestInboxReadOnAThreadThatNeverExistedIsEmptyNotAnError(t *testing.T) {
	withInboxState(t)

	var out bytes.Buffer
	if err := inboxCommand([]string{"read", "--thread", "never-posted"}, &out); err != nil {
		t.Fatalf("read: %v", err)
	}
	if !strings.Contains(out.String(), "no messages") {
		t.Errorf("output = %q, want it to say there are no messages", out.String())
	}

	var jsonOut bytes.Buffer
	if err := inboxCommand([]string{"read", "--thread", "never-posted", "--json"}, &jsonOut); err != nil {
		t.Fatalf("read --json: %v", err)
	}
	var doc inboxReadJSON
	if err := json.Unmarshal(jsonOut.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v; got %s", err, jsonOut.String())
	}
	if doc.Messages == nil {
		t.Error("Messages is null, want an empty array — a consumer should not need a second shape for \"nothing here\"")
	}
	if len(doc.Messages) != 0 {
		t.Errorf("Messages = %v, want empty", doc.Messages)
	}
}

// Threads are independent files: posting to one must never appear in another.
func TestInboxThreadsAreIsolatedFromEachOther(t *testing.T) {
	withInboxState(t)

	var out bytes.Buffer
	if err := inboxCommand([]string{"post", "--thread", "169", "--from", "a", "--note", "about issue 169"}, &out); err != nil {
		t.Fatal(err)
	}
	if err := inboxCommand([]string{"post", "--thread", "170", "--from", "b", "--note", "about issue 170"}, &out); err != nil {
		t.Fatal(err)
	}

	out.Reset()
	if err := inboxCommand([]string{"read", "--thread", "169"}, &out); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(out.String(), "170") {
		t.Errorf("thread 169 leaked thread 170's message:\n%s", out.String())
	}
}

// search is global: it finds a message regardless of which thread it was
// filed under, by content in either the note or the author.
func TestInboxSearchFindsAcrossThreadsByNoteOrAuthor(t *testing.T) {
	withInboxState(t)

	must := func(args ...string) {
		t.Helper()
		var out bytes.Buffer
		if err := inboxCommand(append([]string{"post"}, args...), &out); err != nil {
			t.Fatalf("post %v: %v", args, err)
		}
	}
	must("--thread", "169", "--from", "coordinator", "--note", "the inbox design landed")
	must("--thread", "42", "--from", "worker-inbox-agent", "--note", "unrelated work")
	must("--thread", "43", "--from", "someone-else", "--note", "also unrelated")

	var out bytes.Buffer
	if err := inboxCommand([]string{"search", "inbox"}, &out); err != nil {
		t.Fatalf("search: %v", err)
	}
	got := out.String()
	if !strings.Contains(got, "169") {
		t.Errorf("search %q should find the note match in thread 169, got:\n%s", "inbox", got)
	}
	if !strings.Contains(got, "42") {
		t.Errorf("search %q should find the author match in thread 42, got:\n%s", "inbox", got)
	}
	if strings.Contains(got, "43") {
		t.Errorf("search %q should not match thread 43, got:\n%s", "inbox", got)
	}

	var noMatch bytes.Buffer
	if err := inboxCommand([]string{"search", "nothing-posted-contains-this"}, &noMatch); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(noMatch.String(), "no matches") {
		t.Errorf("output = %q, want it to say no matches", noMatch.String())
	}
}

func TestInboxSearchJSONShapeIsNeverNull(t *testing.T) {
	withInboxState(t)

	var out bytes.Buffer
	if err := inboxCommand([]string{"search", "--json", "nothing-posted-contains-this"}, &out); err != nil {
		t.Fatal(err)
	}
	var doc inboxSearchJSON
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		t.Fatalf("unmarshal: %v; got %s", err, out.String())
	}
	if doc.Entries == nil {
		t.Error("Entries is null, want an empty array")
	}
}

func TestInboxPostJSONReportsWhatWasStored(t *testing.T) {
	withInboxState(t)

	var out bytes.Buffer
	if err := inboxCommand([]string{"post", "--thread", "169", "--from", "a", "--note", "n", "--json"}, &out); err != nil {
		t.Fatal(err)
	}
	var entry inboxEntry
	if err := json.Unmarshal(out.Bytes(), &entry); err != nil {
		t.Fatalf("unmarshal: %v; got %s", err, out.String())
	}
	if entry.Thread != "169" || entry.From != "a" || entry.Note != "n" || entry.At == "" {
		t.Errorf("entry = %+v, missing a field", entry)
	}
}

// The rendered line is the only place data ever reaches a terminal, so it is
// the one place safetext.Escape must actually run — belt to validInboxText's
// braces, for a message written before this defence existed or edited by
// hand. All three stored fields go through it: At is never sent through
// validInboxText (post generates it, nobody supplies it), so this escape is
// the only defence it has against a hand-edited file.
func TestRenderInboxLineEscapesHazardousRunes(t *testing.T) {
	for _, tc := range []struct {
		name string
		m    inboxMessage
	}{
		{"in note", inboxMessage{From: "a", Note: "hidden \u202etext", At: "2026-01-01T00:00:00Z"}},
		{"in from", inboxMessage{From: "hidden \u202etext", Note: "n", At: "2026-01-01T00:00:00Z"}},
		{"in at", inboxMessage{From: "a", Note: "n", At: "hidden \u202etext"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := renderInboxLine(tc.m)
			if strings.ContainsRune(got, '\u202e') {
				t.Errorf("rendered line still carries the raw override rune: %q", got)
			}
			if !strings.Contains(got, `\u{202E}`) {
				t.Errorf("rendered line should escape the override rune visibly, got %q", got)
			}
		})
	}
}

// appendInboxMessage's whole safety argument is that each append is one
// write(2) of a complete line. Many goroutines appending to the same thread
// concurrently must produce that many well-formed, non-interleaved lines.
func TestConcurrentAppendsProduceOneWellFormedLinePerWriter(t *testing.T) {
	dir := t.TempDir()
	const writers = 50

	var wg sync.WaitGroup
	for i := 0; i < writers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := inboxMessage{From: "writer", Note: strings.Repeat("x", 100), At: "2026-01-01T00:00:00Z"}
			if err := appendInboxMessage(dir, "concurrent", msg); err != nil {
				t.Errorf("append %d: %v", i, err)
			}
		}(i)
	}
	wg.Wait()

	messages, err := readThreadMessages(dir, "concurrent")
	if err != nil {
		t.Fatalf("read back: %v", err)
	}
	if len(messages) != writers {
		t.Fatalf("read back %d messages, want %d — a write interleaved with another", len(messages), writers)
	}
}

func TestDefaultStateDirFallsBackToXDGLayout(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_STATE_HOME", "")
	t.Setenv("HOME", home)

	got := defaultStateDir()
	want := filepath.Join(home, ".local", "state", "herdr", "plugins", "steig.worktender")
	if got != want {
		t.Errorf("defaultStateDir() = %q, want %q", got, want)
	}
}

func TestInboxUsesXDGStateHomeWhenSet(t *testing.T) {
	t.Setenv("HERDR_PLUGIN_STATE_DIR", "")
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)

	var out bytes.Buffer
	if err := inboxCommand([]string{"post", "--thread", "169", "--from", "a", "--note", "n"}, &out); err != nil {
		t.Fatalf("post: %v", err)
	}

	want := filepath.Join(xdg, "herdr", "plugins", "steig.worktender", "inbox", "169.ndjson")
	if _, err := os.Stat(want); err != nil {
		t.Errorf("expected a thread file at %s: %v", want, err)
	}
}
