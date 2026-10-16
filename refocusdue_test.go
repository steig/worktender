package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/steig/worktender/internal/herdrtest"
)

// dueGh fakes gh for the interval: `issue view … labels` answers the issue
// with labels, `issue view … body` the issue as refocus reads it, `api` the
// parent with parentExit. Every call's argv is logged, as refocusGh does, and
// answers are files the script cats, never echoed JSON.
func dueGh(t *testing.T, number int, labels []string, parentExit int) string {
	t.Helper()
	dir := t.TempDir()
	write := func(name string, v any) string {
		raw, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, raw, 0o600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	named := make([]map[string]string, len(labels))
	for i, l := range labels {
		named[i] = map[string]string{"name": l}
	}
	labelled := write("labels.json", map[string]any{"number": number, "title": "Interval refocus", "labels": named})
	issue := write("issue.json", map[string]any{"number": number, "title": "Interval refocus", "body": refocusBody})
	parent := write("parent.json", refocusParent)
	log := filepath.Join(dir, "argv.log")
	herdrtest.FakeGh(t, `printf '%s\n' "$*" >> '`+log+`'
case "$*" in
*number,title,labels*) cat '`+labelled+`' ;;
issue*) cat '`+issue+`' ;;
api*) cat '`+parent+`'; exit `+strconv.Itoa(parentExit)+` ;;
*) exit 9 ;;
esac`)
	return log
}

// dueWorld is a worker's worktree for #214 with plugin state somewhere
// disposable and a clock the test moves.
func dueWorld(t *testing.T, config string) (checkout string, clock *time.Time) {
	t.Helper()
	if config != "" {
		writeRoles(t, config)
	}
	t.Setenv("HERDR_PLUGIN_STATE_DIR", t.TempDir())
	repo := herdrtest.NewRepo(t)
	checkout = repo.AddWorktree("214-interval", "214-refocus-interval")
	now := time.Date(2026, 10, 16, 12, 0, 0, 0, time.UTC)
	clock = &now
	prev := refocusNow
	refocusNow = func() time.Time { return *clock }
	t.Cleanup(func() { refocusNow = prev })
	return checkout, clock
}

func refocusDue(t *testing.T, dir string) (string, error) {
	t.Helper()
	t.Chdir(dir)
	var out strings.Builder
	err := run([]string{"refocus", "--if-due"}, &out)
	return out.String(), err
}

func mustQuiet(t *testing.T, dir, when string) {
	t.Helper()
	out, err := refocusDue(t, dir)
	if err != nil || out != "" {
		t.Fatalf("%s: err %v, out %q", when, err, out)
	}
}

const intervalRoles = `{
  "labels": [ { "match": "role:ux", "role": "ux" } ],
  "roles": {
    "ux": { "refocus": { "turns": 2 } },
    "engineer": {}
  }
}`

func TestRefocusIfDueEveryNTurns(t *testing.T) {
	checkout, _ := dueWorld(t, intervalRoles)
	log := dueGh(t, 214, []string{"role:ux"}, 0)

	mustQuiet(t, checkout, "the brief")
	if calls := ghCalls(t, log); strings.Count(calls, "\n") != 1 || !strings.Contains(calls, "number,title,labels") {
		t.Fatalf("the first prompt should read the labels once, and only that:\n%s", calls)
	}
	mustQuiet(t, checkout, "turn 1")

	out, err := refocusDue(t, checkout)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Refocus: the goal chain") || !strings.Contains(out, "  issue: #214\n") {
		t.Fatalf("turn 2 should deliver the chain:\n%s", out)
	}
	mustQuiet(t, checkout, "turn 1 after a delivery")
	if calls := ghCalls(t, log); strings.Count(calls, "number,title,labels") != 1 {
		t.Errorf("the labels were read again:\n%s", calls)
	}
}

func TestRefocusIfDueEveryNMinutes(t *testing.T) {
	checkout, clock := dueWorld(t, `{"default_role": "analyst", "roles": {"analyst": {"refocus": {"minutes": 30}}}}`)
	dueGh(t, 214, nil, 0)

	mustQuiet(t, checkout, "the brief")
	*clock = clock.Add(10 * time.Minute)
	mustQuiet(t, checkout, "10 minutes in")
	*clock = clock.Add(21 * time.Minute)
	if out, err := refocusDue(t, checkout); err != nil || !strings.Contains(out, "Refocus:") {
		t.Fatalf("31 minutes in: err %v, out %q", err, out)
	}
	*clock = clock.Add(time.Minute)
	mustQuiet(t, checkout, "a minute after a delivery")
}

// Off unless a role asks: everyone's configuration until someone writes an
// interval costs no gh call and leaves no state.
func TestRefocusIfDueIsOffWithoutAnInterval(t *testing.T) {
	for name, config := range map[string]string{
		"no configuration":   "",
		"no role sets one":   `{"default_role": "engineer", "roles": {"engineer": {"brief": "code"}}}`,
		"zero is never, too": `{"default_role": "engineer", "roles": {"engineer": {"refocus": {"turns": 0}}}}`,
	} {
		t.Run(name, func(t *testing.T) {
			checkout, _ := dueWorld(t, config)
			log := dueGh(t, 214, nil, 0)
			for i := range 5 {
				mustQuiet(t, checkout, "prompt "+strconv.Itoa(i))
			}
			if calls := ghCalls(t, log); calls != "" {
				t.Errorf("gh was asked:\n%s", calls)
			}
			if _, err := os.Stat(filepath.Join(os.Getenv("HERDR_PLUGIN_STATE_DIR"), "refocus")); !os.IsNotExist(err) {
				t.Errorf("state was written: %v", err)
			}
		})
	}
}

// Another role's interval is not this worker's: its labels pick a role with
// none, and after the one label read nothing more is asked.
func TestRefocusIfDueFollowsTheWorkersRole(t *testing.T) {
	checkout, _ := dueWorld(t, intervalRoles)
	log := dueGh(t, 214, []string{"bug"}, 0)
	for i := range 6 {
		mustQuiet(t, checkout, "prompt "+strconv.Itoa(i))
	}
	if calls := ghCalls(t, log); strings.Count(calls, "\n") != 1 {
		t.Errorf("want only the label read:\n%s", calls)
	}
}

// A delivery gh could not make is tried again on the next prompt, not a whole
// interval later.
func TestRefocusIfDueRetriesAfterGhFails(t *testing.T) {
	checkout, _ := dueWorld(t, intervalRoles)
	dueGh(t, 214, []string{"role:ux"}, 0)
	mustQuiet(t, checkout, "the brief")
	mustQuiet(t, checkout, "turn 1")

	dueGh(t, 214, []string{"role:ux"}, 1) // the parent read now fails, as on bad credentials
	out, err := refocusDue(t, checkout)
	if err == nil || out != "" || exitCode(err) != exitEnvironment {
		t.Fatalf("err %v (exit %d), out %q", err, exitCode(err), out)
	}

	dueGh(t, 214, []string{"role:ux"}, 0)
	if out, err := refocusDue(t, checkout); err != nil || !strings.Contains(out, "Refocus:") {
		t.Fatalf("the retry: err %v, out %q", err, out)
	}
}

func TestRefocusIfDueIsSilentOutsideAWorker(t *testing.T) {
	dueWorld(t, intervalRoles)
	log := dueGh(t, 214, []string{"role:ux"}, 0)
	repo := herdrtest.NewRepo(t)
	mustQuiet(t, repo.Root, "the main checkout")
	if calls := ghCalls(t, log); calls != "" {
		t.Errorf("gh was asked:\n%s", calls)
	}
}

func TestRefocusRefusesUnknownArguments(t *testing.T) {
	for _, args := range [][]string{{"--every"}, {"--if-due", "--if-due"}} {
		err := run(append([]string{"refocus"}, args...), &strings.Builder{})
		if exitCode(err) != exitUsage {
			t.Errorf("%v: err %v", args, err)
		}
	}
}

// writeRecordedRole is what `start` leaves in a worktree's git dir.
func writeRecordedRole(t *testing.T, checkout, content string) {
	t.Helper()
	path, err := gitPathIn(checkout, startedRolePath)
	if err != nil {
		t.Fatal(err)
	}
	herdrtest.WriteFile(t, path, content)
}

// `start --role ux` on an issue whose labels pick no role records ux, and the
// interval is ux's.
func TestStartRecordsTheRoleItChose(t *testing.T) {
	repo := herdrtest.NewRepo(t)
	fakeStart(t, repo, "w9", "w9:p1", "working")
	checkout := repo.AddWorktree("42", "42-fix-the-thing")
	writeRoles(t, intervalRoles)
	herdrtest.FakeGh(t, herdrtest.CatScript(t, `{"number":42,"title":"Fix the thing","labels":[{"name":"bug"}]}`))

	var out strings.Builder
	if err := startCommand([]string{"42", "--role", "ux"}, &out); err != nil {
		t.Fatalf("start: %v\n%s", err, out.String())
	}
	if got := recordedRole(checkout); got != "ux" {
		t.Errorf("recorded role %q, want ux", got)
	}
	if strings.Contains(out.String(), "not recorded") {
		t.Errorf("start said it could not record the role:\n%s", out.String())
	}
}

// A recorded role wins over the labels, and costs no gh call to settle: here
// the labels pick no role at all, and the worker still gets ux's interval.
func TestRefocusIfDuePrefersTheRecordedRole(t *testing.T) {
	checkout, _ := dueWorld(t, intervalRoles)
	log := dueGh(t, 214, []string{"bug"}, 0)
	writeRecordedRole(t, checkout, "ux\n")

	mustQuiet(t, checkout, "the brief")
	if calls := ghCalls(t, log); calls != "" {
		t.Fatalf("the labels were read although start recorded the role:\n%s", calls)
	}
	mustQuiet(t, checkout, "turn 1")
	if out, err := refocusDue(t, checkout); err != nil || !strings.Contains(out, "Refocus:") {
		t.Fatalf("turn 2 under the recorded role: err %v, out %q", err, out)
	}
	if calls := ghCalls(t, log); strings.Contains(calls, "number,title,labels") {
		t.Errorf("the labels were read:\n%s", calls)
	}
}

// With no usable record — a worktree from before start kept one, or an empty
// file — the labels decide, as they did.
func TestRefocusIfDueFallsBackToTheLabels(t *testing.T) {
	for name, record := range map[string]string{"no record": "", "an empty record": "\n"} {
		t.Run(name, func(t *testing.T) {
			checkout, _ := dueWorld(t, intervalRoles)
			log := dueGh(t, 214, []string{"role:ux"}, 0)
			if record != "" {
				writeRecordedRole(t, checkout, record)
			}
			mustQuiet(t, checkout, "the brief")
			if calls := ghCalls(t, log); !strings.Contains(calls, "number,title,labels") {
				t.Fatalf("the labels were not read:\n%s", calls)
			}
			mustQuiet(t, checkout, "turn 1")
			if out, err := refocusDue(t, checkout); err != nil || !strings.Contains(out, "Refocus:") {
				t.Fatalf("turn 2 under the labels' role: err %v, out %q", err, out)
			}
		})
	}
}
