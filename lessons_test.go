package main

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/steig/worktender/internal/herdrtest"
)

// lessonsAt fixes the clock lessons are dated by, and who approves them.
func lessonsAt(t *testing.T, day string) {
	t.Helper()
	at, err := time.Parse(lessonDate, day)
	if err != nil {
		t.Fatal(err)
	}
	prevNow, prevWho := ledgerNow, lessonApprover
	ledgerNow = func() time.Time { return at.Add(9 * time.Hour) }
	lessonApprover = func() (string, error) { return "octo", nil }
	t.Cleanup(func() { ledgerNow, lessonApprover = prevNow, prevWho })
}

// lessonsConfig gives the test a config directory of its own and returns the
// lessons directory inside it.
func lessonsConfig(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	return filepath.Join(dir, "worktender", "lessons")
}

func readLessons(t *testing.T, path string) string {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func writeLedger(t *testing.T, lines ...string) {
	t.Helper()
	path := withLedger(t)
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
}

// liveLesson is a block of a live file, valid unless the test breaks it.
func liveLesson(id, text, expires string) string {
	return "## " + id + "\nprovenance: https://github.com/o/r/pull/1\nadded: 2026-09-01\nexpires: " + expires +
		"\napproved-by: octo\n\n" + text + "\n\n"
}

// The cycle a lesson comes from is a dispatch that said done, was not, and
// then was — by a second done, or by a done after a feedback round. A
// dispatch whose last word was the feedback round is not fixed yet.
func TestFailedThenFixed(t *testing.T) {
	start := `{"v":1,"event":"start","at":"2026-10-01T10:00:00Z","repository":"/r","branch":"b","issue":1,"role":"eng"}`
	done := func(at string) string {
		return `{"v":1,"event":"report","at":"` + at + `","repository":"/r","branch":"b","status":"done","pr":7}`
	}
	feedback := func(at string) string {
		return `{"v":1,"event":"feedback","at":"` + at + `","repository":"/r","branch":"b","pr":7}`
	}
	for _, tc := range []struct {
		name  string
		lines []string
		want  bool
		pivot string
	}{
		{"done once", []string{start, done("2026-10-01T10:30:00Z")}, false, ""},
		{"done twice", []string{start, done("2026-10-01T10:30:00Z"), done("2026-10-01T11:00:00Z")}, true, "2026-10-01T10:30:00Z"},
		{"feedback then done", []string{start, feedback("2026-10-01T10:20:00Z"), done("2026-10-01T10:30:00Z")}, true, "2026-10-01T10:20:00Z"},
		{"done then feedback", []string{start, done("2026-10-01T10:30:00Z"), feedback("2026-10-01T10:40:00Z")}, false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			events, _, err := decodeLedger(strings.NewReader(strings.Join(tc.lines, "\n")))
			if err != nil {
				t.Fatal(err)
			}
			pivot, ok := failedThenFixed(attribute(events)[0])
			if ok != tc.want {
				t.Fatalf("failedThenFixed = %v, want %v", ok, tc.want)
			}
			if ok && pivot.Format(time.RFC3339) != tc.pivot {
				t.Errorf("pivot = %s, want %s", pivot.Format(time.RFC3339), tc.pivot)
			}
		})
	}
}

func TestLessonsProposeFindsAFailedThenFixedDispatch(t *testing.T) {
	dir := lessonsConfig(t)
	lessonsAt(t, "2026-10-16")
	writeLedger(t,
		`{"v":1,"event":"start","at":"2026-10-01T10:00:00Z","repository":"/r","branch":"42-a","issue":42,"origin":"git@github.com:o/repo.git","role":"eng"}`,
		`{"v":1,"event":"start","at":"2026-10-01T10:05:00Z","repository":"/r","branch":"43-b","issue":43,"origin":"git@github.com:o/repo.git","role":"eng"}`,
		`{"v":1,"event":"start","at":"2026-10-01T10:06:00Z","repository":"/r","branch":"44-c","issue":44,"origin":"git@github.com:o/repo.git"}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:20:00Z","repository":"/r","branch":"43-b","status":"done","pr":8}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:30:00Z","repository":"/r","branch":"42-a","status":"done","pr":7}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:31:00Z","repository":"/r","branch":"44-c","status":"done","pr":9}`,
		`{"v":1,"event":"report","at":"2026-10-01T10:32:00Z","repository":"/r","branch":"44-c","status":"done","pr":9}`,
		`{"v":1,"event":"report","at":"2026-10-01T11:00:00Z","repository":"/r","branch":"42-a","status":"done","pr":7}`,
	)
	// Only #42 is a candidate with a role, so gh is asked about PR 7 alone. A
	// headline carrying a hostile control rune proves the evidence is escaped.
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"url":"https://github.com/o/repo/pull/7","state":"MERGED","commits":[
{"oid":"aaaaaaaaaa","messageHeadline":"first try","committedDate":"2026-10-01T10:25:00Z"},
{"oid":"bbbbbbbbbb","messageHeadline":"run the gate before done\u202e","committedDate":"2026-10-01T10:45:00Z"}]}`))

	var out strings.Builder
	if err := run([]string{"lessons", "propose", "--write"}, &out); err != nil {
		t.Fatalf("propose: %v\n%s", err, out.String())
	}
	got := out.String()
	for _, want := range []string{"## repo-7\n", "provenance: https://github.com/o/repo/pull/7", "#42 on 42-a", "2 done reports",
		"| bbbbbbb run the gate before done\\u{202E}", lessonPlaceholder, "1 failed-then-fixed dispatch(es) started with no role"} {
		if !strings.Contains(got, want) {
			t.Errorf("propose output lacks %q:\n%s", want, got)
		}
	}
	if strings.Contains(got, "first try") || strings.Contains(got, "repo-8") {
		t.Errorf("propose reported a commit from the first attempt, or a dispatch done once:\n%s", got)
	}

	proposed := readLessons(t, filepath.Join(dir, "eng.proposed.md"))
	if !strings.Contains(proposed, "## repo-7\n") {
		t.Errorf("--write did not append the draft:\n%s", proposed)
	}
	if _, err := os.Stat(filepath.Join(dir, "eng.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("propose must never write the live file; stat: %v", err)
	}

	// Proposing again does not duplicate the draft.
	if err := run([]string{"lessons", "propose", "--write"}, &strings.Builder{}); err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(readLessons(t, filepath.Join(dir, "eng.proposed.md")), "## repo-7"); n != 1 {
		t.Errorf("the draft appears %d times after a second propose, want 1", n)
	}
}

func TestLessonsApproveStampsTheLesson(t *testing.T) {
	dir := lessonsConfig(t)
	lessonsAt(t, "2026-10-16")
	draft := renderLesson(lesson{ID: "repo-7", Provenance: "https://github.com/o/repo/pull/7",
		MotivatedBy: "ledger: #42", Text: lessonPlaceholder, Evidence: []string{"untrusted", "bbbbbbb fix"}})
	herdrtest.WriteFile(t, filepath.Join(dir, "eng.proposed.md"), "# Proposed\n\n"+draft)

	err := run([]string{"lessons", "approve", "repo-7", "--yes"}, &strings.Builder{})
	if err == nil || !strings.Contains(err.Error(), "no text") {
		t.Fatalf("approving a draft nobody wrote should be refused; got %v", err)
	}

	herdrtest.WriteFile(t, filepath.Join(dir, "eng.proposed.md"),
		"# Proposed\n\n"+strings.Replace(draft, lessonPlaceholder, "Run the gate before reporting done.", 1))
	prev := approveInput
	approveInput = strings.NewReader("yes\n")
	t.Cleanup(func() { approveInput = prev })

	var out strings.Builder
	if err := run([]string{"lessons", "approve", "repo-7"}, &out); err != nil {
		t.Fatalf("approve: %v\n%s", err, out.String())
	}
	live := readLessons(t, filepath.Join(dir, "eng.md"))
	for _, want := range []string{"## repo-7\n", "provenance: https://github.com/o/repo/pull/7", "added: 2026-10-16",
		"expires: 2026-12-15", "approved-by: octo", "Run the gate before reporting done."} {
		if !strings.Contains(live, want) {
			t.Errorf("live file lacks %q:\n%s", want, live)
		}
	}
	if strings.Contains(live, "| ") {
		t.Errorf("the untrusted evidence reached the live file:\n%s", live)
	}
	if strings.Contains(readLessons(t, filepath.Join(dir, "eng.proposed.md")), "repo-7") {
		t.Error("approve left the lesson in the proposals too")
	}
	if !strings.Contains(out.String(), "Every eng worker started from now until 2026-12-15") {
		t.Errorf("approve should say who reads it and until when:\n%s", out.String())
	}
}

func TestLessonsApproveNeedsAYes(t *testing.T) {
	dir := lessonsConfig(t)
	lessonsAt(t, "2026-10-16")
	herdrtest.WriteFile(t, filepath.Join(dir, "eng.proposed.md"),
		renderLesson(lesson{ID: "repo-7", Provenance: "https://github.com/o/repo/pull/7", Text: "Do the thing."}))
	prev := approveInput
	approveInput = strings.NewReader("no\n")
	t.Cleanup(func() { approveInput = prev })

	if err := run([]string{"lessons", "approve", "repo-7"}, &strings.Builder{}); exitCode(err) != exitUsage {
		t.Fatalf("approve without yes = %v, want a usage refusal", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "eng.md")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("a refused approval wrote the live file; stat: %v", err)
	}
}

// Removal is from the one file start reads, so it is removal from every
// future brief — in every role that holds the lesson, unless --role narrows it.
// Everything else in the file is left byte for byte.
func TestLessonsRemove(t *testing.T) {
	dir := lessonsConfig(t)
	lessonsAt(t, "2026-10-16")
	eng := "# Mine\n\nhand-written note\n\n" + liveLesson("keep", "Keep me.", "2026-12-01") + liveLesson("bad", "Wrong.", "2026-12-01")
	herdrtest.WriteFile(t, filepath.Join(dir, "eng.md"), eng)
	herdrtest.WriteFile(t, filepath.Join(dir, "ux.md"), liveLesson("bad", "Wrong.", "2026-12-01"))

	var out strings.Builder
	if err := run([]string{"lessons", "remove", "bad"}, &out); err != nil {
		t.Fatalf("remove: %v", err)
	}
	want := "# Mine\n\nhand-written note\n\n" + liveLesson("keep", "Keep me.", "2026-12-01")
	if got := readLessons(t, filepath.Join(dir, "eng.md")); got != want {
		t.Errorf("eng.md after remove:\n%q\nwant\n%q", got, want)
	}
	if got := readLessons(t, filepath.Join(dir, "ux.md")); strings.Contains(got, "bad") {
		t.Errorf("ux.md still holds the removed lesson:\n%s", got)
	}
	use, _, err := briefLessons("eng")
	if err != nil || len(use) != 1 || use[0].ID != "keep" {
		t.Errorf("briefLessons after remove = %+v, %v; want only keep", use, err)
	}

	if err := run([]string{"lessons", "remove", "bad"}, &strings.Builder{}); exitCode(err) != exitUsage {
		t.Errorf("removing a lesson that is not there = %v, want a usage refusal", err)
	}
}

// A brief gets at most lessonsPerBrief lessons, each clipped, and the brief
// itself only a count — so the longest brief start can type still fits in one
// pane read however many lessons a role has.
func TestLessonsInjectionIsBounded(t *testing.T) {
	dir := lessonsConfig(t)
	lessonsAt(t, "2026-10-16")
	var b strings.Builder
	for i := range 7 {
		b.WriteString(liveLesson("l"+string(rune('a'+i)), strings.Repeat("long ", 200), "2026-12-01"))
	}
	herdrtest.WriteFile(t, filepath.Join(dir, "eng.md"), b.String())

	use, skipped, err := briefLessons("eng")
	if err != nil {
		t.Fatal(err)
	}
	if len(use) != lessonsPerBrief || len(skipped) != 1 || !strings.Contains(skipped[0], "2 past the cap") {
		t.Errorf("got %d lessons and skipped %v; want %d and 2 past the cap", len(use), skipped, lessonsPerBrief)
	}
	if n := len([]rune(clipLesson(use[0].Text))); n != lessonTextMax {
		t.Errorf("a clipped lesson is %d runes, want %d", n, lessonTextMax)
	}

	installed := func(line string) string { return strings.ReplaceAll(line, selfPath(), installedSelfPath) }

	// An ordinary brief gets the pointer.
	var notes strings.Builder
	if line := withLessons(&notes, installed(briefFor(42, "42-fix-the-thing", "code", false)), lessonsPerBrief, false); !strings.Contains(line, "read the 5 lessons a person approved") {
		t.Errorf("an ordinary brief should point at the lessons:\n%s", line)
	}

	// The longest briefs have no room for it, and stay within the read rather
	// than break it; the caller is told instead.
	branch := issueBranch(issue{Number: 999999, Title: strings.Repeat("word ", 40)})
	for style := range briefStyles {
		notes.Reset()
		line := withLessons(&notes, installed(briefFor(999999, branch, style, true)), lessonsPerBrief, false)
		if len(line) > paneReadChunk {
			t.Errorf("the longest %s brief with lessons is %d bytes and a pane read carries %d:\n%s", style, len(line), paneReadChunk, line)
		}
		if !strings.Contains(line, "lessons") && !strings.Contains(notes.String(), "no room left") {
			t.Errorf("the %s brief dropped the pointer without telling the caller", style)
		}
	}

	// A ready issue's brief carries nothing more; its pointer goes in the
	// approved copy (TestStartPointsAReadyWorkerAtItsLessons).
	ready := installed(briefWithChecks(999999, branch, "analysis", true, 9))
	if withLessons(&notes, ready, lessonsPerBrief, true) != ready {
		t.Error("a ready issue's brief must not grow a lessons sentence")
	}
}

// End to end: start in a role pins that role's live lessons into the worktree
// and points the brief at them. The expired lesson is skipped, and the brief
// carries none of any lesson's text.
func TestStartSkipsAnExpiredLesson(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	checkout := repo.AddWorktree("42", "42-fix-the-thing")
	server := fakeStart(t, repo, "w9", "w9:p1", "working")
	writeRoles(t, `{"roles": {"eng": {}}}`)
	lessonsAt(t, "2026-10-16")
	dir := filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "worktender", "lessons")
	herdrtest.WriteFile(t, filepath.Join(dir, "eng.md"),
		liveLesson("old", "Stale advice.", "2026-10-16")+liveLesson("fresh", "Run the gate first.", "2026-10-17"))
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":42,"title":"Fix the thing"}`))

	var out strings.Builder
	if err := startCommand([]string{"42", "--role", "eng"}, &out); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	var text string
	for _, call := range server.Calls() {
		if call.Method == "pane.send_text" {
			text, _ = call.Params["text"].(string)
		}
	}
	if !strings.Contains(text, "read the 1 lesson a person approved") || strings.Contains(text, "Run the gate first") {
		t.Errorf("the brief should point at 1 pinned lesson and carry none of its text:\n%s", text)
	}

	raw, err := os.ReadFile(gitPath(t, checkout, pinnedLessonsPath))
	if err != nil {
		t.Fatalf("no pinned lessons: %v", err)
	}
	pinned := string(raw)
	if !strings.Contains(pinned, "Run the gate first.") || !strings.Contains(pinned, "https://github.com/o/r/pull/1") {
		t.Errorf("the pinned file lacks the live lesson or its provenance:\n%s", pinned)
	}
	if strings.Contains(pinned, "Stale advice") {
		t.Errorf("an expired lesson was pinned:\n%s", pinned)
	}
	if !strings.Contains(out.String(), "skipped 1 expired") {
		t.Errorf("start should say it skipped an expired lesson:\n%s", out.String())
	}
}

// A role with no lessons file starts exactly as before.
func TestStartWithoutLessonsIsUnchanged(t *testing.T) {
	lessonsConfig(t)
	if got := withLessons(io.Discard, "brief.", 0, false); got != "brief." {
		t.Errorf("withLessons with none = %q", got)
	}
	use, skipped, err := briefLessons("eng")
	if err != nil || use != nil || skipped != nil {
		t.Errorf("briefLessons with no file = %v, %v, %v", use, skipped, err)
	}
}

func TestDoctorWarnsAboutAnExpiredLesson(t *testing.T) {
	dir := lessonsConfig(t)
	lessonsAt(t, "2026-10-16")
	if c := lessonsCheck(); c.state != stateOff {
		t.Errorf("no lessons: state %s, want off", c.state)
	}
	herdrtest.WriteFile(t, filepath.Join(dir, "eng.md"), liveLesson("old", "Stale.", "2026-10-01")+liveLesson("ok", "Fine.", "2026-12-01"))
	c := lessonsCheck()
	if c.state != stateWarn || !strings.Contains(c.note, "eng/old expired 2026-10-01") || !strings.HasPrefix(c.value, "1 live") {
		t.Errorf("lessonsCheck = %+v; want a warning naming eng/old and 1 live", c)
	}
}

// A misspelt key is refused rather than ignored: an "expire:" that did
// nothing would be a lesson that never lapses.
func TestLessonsFileRefusesAnUnknownKey(t *testing.T) {
	if _, err := parseLessons("## a\nexpire: 2026-12-01\n\ntext\n"); err == nil || !strings.Contains(err.Error(), `unknown key "expire"`) {
		t.Errorf("parseLessons = %v, want an unknown-key refusal", err)
	}
}

func gitPath(t *testing.T, checkout, rel string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", checkout, "rev-parse", "--path-format=absolute", "--git-path", rel).Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// A ready issue's brief is already at the edge of one pane read, so its worker
// is pointed at the lessons from the foot of the approved copy instead.
func TestStartPointsAReadyWorkerAtItsLessons(t *testing.T) {
	r := newReadyStart(t)
	newFakeGitHub(t, readyMeta, snapshotJSON(t, readyTitle, readyBody, comment{"steig", approvalOf(t, readyTitle, readyBody)}),
		map[string]string{"steig": "admin"})
	writeRoles(t, `{"default_role": "eng", "roles": {"eng": {}}}`)
	lessonsAt(t, "2026-10-16")
	herdrtest.WriteFile(t, filepath.Join(os.Getenv("XDG_CONFIG_HOME"), "worktender", "lessons", "eng.md"),
		liveLesson("fresh", "Run the gate first.", "2026-12-01"))

	if err := r.run(); err != nil {
		t.Fatalf("start: %v", err)
	}
	if text := briefSent(r.server); strings.Contains(text, "lesson") {
		t.Errorf("a ready brief has no room for lessons and should not mention them:\n%s", text)
	}
	if copy := pinnedCopy(t, r.checkout); !strings.HasSuffix(copy, "1 lesson a person approved, in lessons.md beside this file. Read them before you plan.\n") {
		t.Errorf("the approved copy should end by pointing at the lessons:\n%s", copy)
	}
	if !strings.Contains(readLessons(t, gitPath(t, r.checkout, pinnedLessonsPath)), "Run the gate first.") {
		t.Error("the lesson was not pinned beside the approved copy")
	}
}
