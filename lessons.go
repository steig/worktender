package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/steig/worktender/internal/reconcile"
	"github.com/steig/worktender/internal/roles"
	"github.com/steig/worktender/internal/safetext"
)

// lessons carries what a failed-then-fixed dispatch taught into the brief of
// the next worker in the same role — and is built around the fact that a wrong
// lesson spreads exactly as easily as a right one (#205).
//
// So nothing here learns anything by itself. The ledger can say that a
// dispatch needed a second attempt, and gh can say which commits the second
// attempt added; neither can say what the lesson was. `propose` finds the
// cycles and prints the evidence as a draft, and a person writes the sentence.
// The draft lives in <role>.proposed.md, which no brief ever reads, and only
// `approve` — a person confirming exactly what will be read — moves it into
// <role>.md, the one file start does read.
//
// Every live lesson carries the four things the guardrails ask for:
//
//   - provenance: the pull request it came from, and the ledger cycle that
//     made it a candidate, so a worker or a reviewer can go and look;
//   - approved-by: whose say-so put it there;
//   - expires: lessons lapse, by default after 60 days, so a rule nobody
//     re-confirms does not settle into permanent ceremony;
//   - one home: a lesson exists in one file, read at brief time. Removing it
//     from that file removes it from every worker started afterwards, with
//     nothing cached anywhere to resurrect it.
//
// The files sit beside config.json, under the user's config directory, for the
// reason the role configuration does: a file a repository could ship would be
// a repository writing into its own workers' prompts.

const lessonsUsage = "usage: worktender lessons <propose|approve|list|remove> ...\n" +
	"  worktender lessons propose [--role <role>] [--since DURATION] [--write]\n" +
	"  worktender lessons approve <id> [--role <role>] [--days N] [--yes]\n" +
	"  worktender lessons list [--role <role>]\n" +
	"  worktender lessons remove <id> [--role <role>]"

const (
	// lessonDefaultDays is how long an approved lesson lives unless --days
	// says otherwise.
	lessonDefaultDays = 60

	// lessonsPerBrief and lessonTextMax bound what a role's lessons can add to
	// a worker's first read. A role that has accumulated twenty lessons has a
	// prompt problem, not a lessons problem, and the cap is what makes that
	// visible rather than quietly handing a worker twenty rules.
	lessonsPerBrief = 5
	lessonTextMax   = 400

	// lessonPlaceholder is the text a draft carries until a person writes the
	// real one. approve refuses a lesson that still says it.
	lessonPlaceholder = "TODO: write the lesson here, in a sentence or two a future worker can act on."

	// pinnedLessonsPath is where start writes a role's lessons for the worker,
	// beside the pinned issue and for the same reasons: in the git dir, so it
	// is never committed, and per worktree, so it goes when the worktree does.
	pinnedLessonsPath = "worktender/lessons.md"
)

// lessonDate is the one date format lesson files use. Days, not instants: a
// lesson expires on a date a person can read and reason about.
const lessonDate = "2006-01-02"

// lessonID and lessonRole are what an id and a role may be called here. A
// role becomes a file name, so it is checked before it reaches a path.
var (
	lessonID   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
	lessonRole = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)
)

// lesson is one entry of a lessons file.
type lesson struct {
	ID          string
	Text        string
	Provenance  string
	MotivatedBy string
	Added       string
	Expires     string
	ApprovedBy  string
	// Evidence is the "| " lines a draft carries: what propose found on
	// GitHub. Never part of the text, never read by a worker.
	Evidence []string
	// raw is the block as it stood in the file, so rewriting a file to drop
	// one lesson keeps every other byte a person wrote.
	raw string
}

// lessonFile is a parsed lessons file: whatever came before the first lesson,
// and the lessons in order.
type lessonFile struct {
	Path     string
	Preamble string
	Lessons  []lesson
}

func (f *lessonFile) find(id string) int {
	for i, l := range f.Lessons {
		if l.ID == id {
			return i
		}
	}
	return -1
}

// render writes the file back: the preamble, then each lesson exactly as it
// was read.
func (f *lessonFile) render() string {
	var b strings.Builder
	b.WriteString(f.Preamble)
	for _, l := range f.Lessons {
		b.WriteString(l.raw)
	}
	return b.String()
}

// lessonsDir is where the files live: beside the role configuration.
func lessonsDir() string {
	p := roles.Path()
	if p == "" {
		return ""
	}
	return filepath.Join(filepath.Dir(p), "lessons")
}

// lessonsPath is a role's live file, or its proposals when proposed is true.
func lessonsPath(role string, proposed bool) (string, error) {
	if !lessonRole.MatchString(role) {
		return "", usagef("role %q cannot name a lessons file; want letters, digits, - and _", role)
	}
	dir := lessonsDir()
	if dir == "" {
		return "", codef(exitEnvironment, "no config directory: neither XDG_CONFIG_HOME nor a home directory is set")
	}
	name := role + ".md"
	if proposed {
		name = role + ".proposed.md"
	}
	return filepath.Join(dir, name), nil
}

// readLessonFile reads and parses one file. A missing file is an empty one.
func readLessonFile(path string) (*lessonFile, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &lessonFile{Path: path}, nil
	}
	if err != nil {
		return nil, withCode(exitEnvironment, fmt.Errorf("read %s: %w", path, err))
	}
	f, err := parseLessons(string(data))
	if err != nil {
		return nil, withCode(exitUsage, fmt.Errorf("%s: %w", path, err))
	}
	f.Path = path
	return f, nil
}

// parseLessons reads the format render writes:
//
//	## <id>
//	provenance: <url>
//	motivated-by: <what in the ledger made it a candidate>
//	added: 2026-10-16
//	expires: 2026-12-15
//	approved-by: <login>
//
//	<the lesson, as many lines as it takes>
//
// The header is every line up to the first blank one; an unknown key there is
// refused rather than ignored, for the reason config.json refuses one: a
// misspelt "expire:" that silently did nothing is a lesson that never lapses.
// Lines beginning "| " are evidence and never part of the text.
func parseLessons(data string) (*lessonFile, error) {
	data = strings.ReplaceAll(data, "\r\n", "\n")
	f := &lessonFile{}
	lines := strings.SplitAfter(data, "\n")

	i := 0
	var pre strings.Builder
	for ; i < len(lines) && !strings.HasPrefix(lines[i], "## "); i++ {
		pre.WriteString(lines[i])
	}
	f.Preamble = pre.String()

	for i < len(lines) {
		start := i
		l := lesson{ID: strings.TrimSpace(strings.TrimPrefix(lines[i], "## "))}
		if !lessonID.MatchString(l.ID) {
			return nil, fmt.Errorf("line %d: %q is not a lesson id; want lowercase letters, digits and -", i+1, l.ID)
		}
		if f.find(l.ID) >= 0 {
			return nil, fmt.Errorf("line %d: lesson %s appears twice", i+1, l.ID)
		}
		i++
		for ; i < len(lines) && strings.TrimSpace(lines[i]) != "" && !strings.HasPrefix(lines[i], "## "); i++ {
			key, value, ok := strings.Cut(strings.TrimSpace(lines[i]), ":")
			if !ok {
				return nil, fmt.Errorf("line %d: want `key: value` in the header of %s, or a blank line before its text", i+1, l.ID)
			}
			value = strings.TrimSpace(value)
			switch key {
			case "provenance":
				l.Provenance = value
			case "motivated-by":
				l.MotivatedBy = value
			case "added":
				l.Added = value
			case "expires":
				l.Expires = value
			case "approved-by":
				l.ApprovedBy = value
			default:
				return nil, fmt.Errorf("line %d: unknown key %q in %s; want provenance, motivated-by, added, expires or approved-by", i+1, key, l.ID)
			}
		}
		var text []string
		for ; i < len(lines) && !strings.HasPrefix(lines[i], "## "); i++ {
			line := strings.TrimRight(lines[i], "\n")
			if rest, ok := strings.CutPrefix(line, "| "); ok || line == "|" {
				l.Evidence = append(l.Evidence, rest)
				continue
			}
			text = append(text, line)
		}
		l.Text = strings.TrimSpace(strings.Join(text, "\n"))
		l.raw = strings.Join(lines[start:i], "")
		if !strings.HasSuffix(l.raw, "\n") {
			l.raw += "\n"
		}
		f.Lessons = append(f.Lessons, l)
	}
	return f, nil
}

// renderLesson is one lesson block in the file format, ending in a blank line.
func renderLesson(l lesson) string {
	var b strings.Builder
	fmt.Fprintf(&b, "## %s\n", l.ID)
	for _, kv := range [][2]string{
		{"provenance", l.Provenance}, {"motivated-by", l.MotivatedBy},
		{"added", l.Added}, {"expires", l.Expires}, {"approved-by", l.ApprovedBy},
	} {
		if kv[1] != "" {
			fmt.Fprintf(&b, "%s: %s\n", kv[0], kv[1])
		}
	}
	fmt.Fprintf(&b, "\n%s\n", l.Text)
	if len(l.Evidence) > 0 {
		b.WriteString("\n")
		for _, e := range l.Evidence {
			fmt.Fprintf(&b, "| %s\n", e)
		}
	}
	b.WriteString("\n")
	return b.String()
}

// problem is what keeps a live lesson out of a brief, or "" when nothing does.
// Expiry is checked separately: an expired lesson is a lesson that was fine.
func (l lesson) problem() string {
	switch {
	case l.Text == "" || strings.Contains(l.Text, lessonPlaceholder):
		return "has no text"
	case !strings.HasPrefix(l.Provenance, "https://"):
		return "has no provenance URL"
	case l.ApprovedBy == "":
		return "has no approved-by"
	}
	if _, err := time.Parse(lessonDate, l.Added); err != nil {
		return "has no added date"
	}
	if _, err := time.Parse(lessonDate, l.Expires); err != nil {
		return "has no expires date"
	}
	return ""
}

// expired is whether the lesson has lapsed by today. A lesson expires ON its
// date: "expires: 2026-12-15" is last read on the 14th.
func (l lesson) expired(today time.Time) bool {
	exp, err := time.Parse(lessonDate, l.Expires)
	return err == nil && !today.Before(exp)
}

func lessonToday() time.Time {
	now := ledgerNow()
	return time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
}

// writeLessonFile replaces a file whole, through a rename, so a reader at
// brief time sees the old file or the new one and never half of either.
func writeLessonFile(path, content string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return withCode(exitEnvironment, err)
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".lessons-*")
	if err != nil {
		return withCode(exitEnvironment, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.WriteString(content); err != nil {
		tmp.Close()
		return withCode(exitEnvironment, err)
	}
	if err := tmp.Close(); err != nil {
		return withCode(exitEnvironment, err)
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return withCode(exitEnvironment, err)
	}
	return nil
}

func lessonsCommand(args []string, out io.Writer) error {
	if len(args) == 0 {
		return usagef("%s", lessonsUsage)
	}
	switch args[0] {
	case "propose":
		return lessonsPropose(args[1:], out)
	case "approve":
		return lessonsApprove(args[1:], out)
	case "list":
		return lessonsList(args[1:], out)
	case "remove":
		return lessonsRemove(args[1:], out)
	default:
		return usagef("unknown lessons command %q; %s", args[0], lessonsUsage)
	}
}

// --- propose ---

// prCommits is what propose asks gh about one pull request.
type prCommits struct {
	URL     string `json:"url"`
	State   string `json:"state"`
	Commits []struct {
		OID           string `json:"oid"`
		Headline      string `json:"messageHeadline"`
		CommittedDate string `json:"committedDate"`
	} `json:"commits"`
}

// failedThenFixed is the cycle a lesson can come from: a dispatch that said it
// was done, was not, and then was. Either it reported done more than once, or
// a pr-feedback round reached it and it reported done after that round. A
// dispatch whose last word was a feedback round has failed and not yet been
// fixed, and there is nothing to learn from it until it has.
//
// pivot is the moment the first attempt ended: the first done when there were
// several, else the first feedback round. The commits after it are what the
// second attempt changed.
func failedThenFixed(d *statsDispatch) (pivot time.Time, ok bool) {
	if d.pr == 0 || d.doneAt.IsZero() {
		return time.Time{}, false
	}
	var dones []time.Time
	for _, r := range d.reports {
		if r.Status == "done" {
			if at, err := time.Parse(time.RFC3339Nano, r.At); err == nil {
				dones = append(dones, at)
			}
		}
	}
	if len(dones) >= 2 {
		return dones[0], true
	}
	if len(d.feedbackAts) > 0 && d.doneAt.After(d.feedbackAts[len(d.feedbackAts)-1]) {
		return d.feedbackAts[0], true
	}
	return time.Time{}, false
}

// proposedID names a candidate by its repository and pull request, so the same
// cycle proposed twice is the same id and --write does not duplicate it.
func proposedID(d *statsDispatch) string {
	name := d.start.Origin
	if name == "" {
		name = d.start.Repository
	}
	name = strings.TrimSuffix(strings.TrimRight(name, "/"), ".git")
	if i := strings.LastIndexAny(name, "/:"); i >= 0 {
		name = name[i+1:]
	}
	slug := reconcile.Slug(name)
	if slug == "" {
		slug = "pr"
	}
	return fmt.Sprintf("%s-%d", slug, d.pr)
}

func lessonsPropose(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("lessons propose", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	role := fs.String("role", "", "only dispatches started in this role")
	since := fs.Duration("since", 0, "only dispatches started within this long ago (default all)")
	write := fs.Bool("write", false, "append the drafts to <role>.proposed.md, which no brief reads")
	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, lessonsUsage)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), lessonsUsage)
	}
	if *since < 0 {
		return usagef("--since must not be negative; %s", lessonsUsage)
	}

	path, err := ledgerPath()
	if err != nil {
		return err
	}
	events, skipped, err := readLedger(path)
	if err != nil {
		return err
	}
	if skipped > 0 {
		fmt.Fprintf(os.Stderr, "worktender: %d line(s) of %s could not be read and were left out\n", skipped, path)
	}

	type draft struct {
		role string
		l    lesson
	}
	var drafts []draft
	noRole, unanswered := 0, 0
	for _, d := range startedWithin(attribute(events), *since) {
		pivot, ok := failedThenFixed(d)
		if !ok {
			continue
		}
		if d.start.Role == "" {
			noRole++
			continue
		}
		if *role != "" && d.start.Role != *role {
			continue
		}

		var pr prCommits
		if err := ghJSON(d.start, []string{"pr", "view", strconv.Itoa(d.pr), "--json", "url,state,commits"}, &pr); err != nil {
			fmt.Fprintf(os.Stderr, "worktender: #%d: %v; no draft for it\n", d.start.Issue, err)
			unanswered++
			continue
		}
		if !strings.HasPrefix(pr.URL, "https://") {
			fmt.Fprintf(os.Stderr, "worktender: #%d: gh answered with no pull request URL; no draft for it\n", d.start.Issue)
			unanswered++
			continue
		}

		dones := 0
		for _, r := range d.reports {
			if r.Status == "done" {
				dones++
			}
		}
		l := lesson{
			ID:         proposedID(d),
			Provenance: pr.URL,
			MotivatedBy: safetext.Escape(fmt.Sprintf("ledger: #%d on %s started %s; %s, %s",
				d.start.Issue, d.start.Branch, d.start.At, plural(dones, "done report"), plural(d.feedback, "pr-feedback round"))),
			Text: lessonPlaceholder,
		}
		// Everything below came from GitHub and is whoever-pushed-it's prose.
		// It is quoted, escaped and kept out of the text; approve drops it.
		l.Evidence = append(l.Evidence, fmt.Sprintf("untrusted, from GitHub: commits after the first attempt ended (%s), PR %s",
			pivot.UTC().Format(time.RFC3339), safetext.Escape(pr.State)))
		for _, c := range pr.Commits {
			at, err := time.Parse(time.RFC3339, c.CommittedDate)
			if err != nil || !at.After(pivot) {
				continue
			}
			l.Evidence = append(l.Evidence, safetext.Escape(fmt.Sprintf("%.7s %s", c.OID, c.Headline)))
		}
		if len(l.Evidence) == 1 {
			l.Evidence = append(l.Evidence, "(none: the fix was force-pushed over, or never pushed)")
		}
		drafts = append(drafts, draft{d.start.Role, l})
	}

	if len(drafts) == 0 {
		fmt.Fprintln(out, "no failed-then-fixed dispatches to learn from")
	}
	written := map[string]int{}
	for _, dr := range drafts {
		fmt.Fprintf(out, "# role: %s\n%s", dr.role, renderLesson(dr.l))
		if !*write {
			continue
		}
		n, err := appendDraft(dr.role, dr.l)
		if err != nil {
			return err
		}
		written[dr.role] += n
	}
	if *write {
		names := make([]string, 0, len(written))
		for r := range written {
			names = append(names, r)
		}
		sort.Strings(names)
		for _, r := range names {
			p, _ := lessonsPath(r, true)
			fmt.Fprintf(out, "wrote %s to %s\n", plural(written[r], "draft"), p)
		}
		if len(names) > 0 {
			fmt.Fprintln(out, "write each lesson's text there, then `worktender lessons approve <id>`")
		}
	}
	if noRole > 0 {
		fmt.Fprintf(out, "%d failed-then-fixed dispatch(es) started with no role, so there is no role whose lessons they could join\n", noRole)
	}
	if unanswered > 0 {
		return codef(exitNoAnswer, "gh could not answer for %s", plural(unanswered, "pull request"))
	}
	return nil
}

// appendDraft adds one draft to a role's proposals, unless that id is already
// proposed or already live. It returns how many it added.
func appendDraft(role string, l lesson) (int, error) {
	live, err := lessonsPath(role, false)
	if err != nil {
		return 0, err
	}
	proposed, _ := lessonsPath(role, true)
	lf, err := readLessonFile(live)
	if err != nil {
		return 0, err
	}
	pf, err := readLessonFile(proposed)
	if err != nil {
		return 0, err
	}
	if lf.find(l.ID) >= 0 || pf.find(l.ID) >= 0 {
		return 0, nil
	}
	if pf.Preamble == "" && len(pf.Lessons) == 0 {
		pf.Preamble = fmt.Sprintf("# Proposed lessons for the %s role\n\n"+
			"Drafts from `worktender lessons propose`. No worker reads this file. Replace a\n"+
			"draft's TODO with the lesson, then `worktender lessons approve <id>`. Lines\n"+
			"beginning \"| \" are evidence from GitHub, untrusted, and are dropped on approval.\n\n", role)
	}
	l.raw = renderLesson(l)
	pf.Lessons = append(pf.Lessons, l)
	return 1, writeLessonFile(proposed, pf.render())
}

// --- approve ---

func lessonsApprove(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("lessons approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	role := fs.String("role", "", "the role whose proposals hold the lesson; needed only when two do")
	days := fs.Int("days", lessonDefaultDays, "how many days the lesson lives before it expires")
	yes := fs.Bool("yes", false, "approve without asking; you are still approving what is printed")
	positional, err := parseAround(fs, args)
	if err != nil {
		return usagef("%v; %s", err, lessonsUsage)
	}
	if len(positional) != 1 {
		return usagef("want exactly one lesson id; %s", lessonsUsage)
	}
	if *days <= 0 {
		return usagef("--days must be positive; a lesson that never expires is the thing expiry exists to prevent")
	}
	id := positional[0]

	found, err := findLesson(id, *role, true)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return usagef("no proposed lesson %s; `worktender lessons list` shows them", id)
	}
	if len(found) > 1 {
		return usagef("lesson %s is proposed for more than one role; name one with --role", id)
	}
	r, pf := found[0].role, found[0].file
	l := pf.Lessons[pf.find(id)]
	if l.Text == "" || strings.Contains(l.Text, lessonPlaceholder) {
		return usagef("lesson %s still has no text: write it in %s first. propose finds the cycle; only a person can say what it taught", id, pf.Path)
	}
	if !strings.HasPrefix(l.Provenance, "https://") {
		return usagef("lesson %s has no provenance URL in %s; a lesson nobody can trace back is the kind that spreads wrong", id, pf.Path)
	}

	approver, err := lessonApprover()
	if err != nil {
		return err
	}
	today := lessonToday()
	l.Added = today.Format(lessonDate)
	l.Expires = today.AddDate(0, 0, *days).Format(lessonDate)
	l.ApprovedBy = approver
	l.Evidence = nil
	l.raw = renderLesson(l)

	livePath, _ := lessonsPath(r, false)
	lf, err := readLessonFile(livePath)
	if err != nil {
		return err
	}
	if lf.find(id) >= 0 {
		return usagef("lesson %s is already live in %s; remove it from %s, or remove the live one first", id, livePath, pf.Path)
	}

	fmt.Fprintf(out, "Every %s worker started from now until %s will read this lesson. Read all of it:\n\n", r, l.Expires)
	fmt.Fprint(out, l.raw)
	if !*yes {
		fmt.Fprint(out, "Approve exactly this? Type yes: ")
		answer, _ := bufio.NewReader(approveInput).ReadString('\n')
		if strings.TrimSpace(answer) != "yes" {
			return usagef("not approved; nothing was changed")
		}
	}

	if lf.Preamble == "" && len(lf.Lessons) == 0 {
		lf.Preamble = fmt.Sprintf("# Lessons for the %s role\n\n"+
			"Every worker `worktender start` briefs in this role reads the first %d unexpired\n"+
			"lessons below. Approve new ones with `worktender lessons approve`; delete one\n"+
			"with `worktender lessons remove <id>`, or by editing this file.\n\n", r, lessonsPerBrief)
	}
	lf.Lessons = append(lf.Lessons, l)
	if err := writeLessonFile(livePath, lf.render()); err != nil {
		return err
	}
	pf.Lessons = append(pf.Lessons[:pf.find(id)], pf.Lessons[pf.find(id)+1:]...)
	if err := writeLessonFile(pf.Path, pf.render()); err != nil {
		return fmt.Errorf("approved %s into %s, but could not drop it from %s: %w; delete it there yourself", id, livePath, pf.Path, err)
	}
	fmt.Fprintf(out, "\napproved %s into %s by %s, until %s\n", id, livePath, approver, l.Expires)
	return nil
}

// lessonApprover is who is approving: the gh login, which is an account, else
// git's user.name, which is at least a name the person chose. A var so the
// suite can say who it is.
var lessonApprover = func() (string, error) {
	if out, err := exec.Command("gh", "api", "user", "--jq", ".login").Output(); err == nil {
		if login := strings.TrimSpace(string(out)); login != "" && !strings.ContainsFunc(login, safetext.IsUnsafe) {
			return login, nil
		}
	}
	if out, err := exec.Command("git", "config", "user.name").Output(); err == nil {
		if name := strings.TrimSpace(string(out)); name != "" && !strings.ContainsFunc(name, safetext.IsUnsafe) {
			return name, nil
		}
	}
	return "", codef(exitEnvironment, "cannot tell who is approving: `gh api user` and `git config user.name` both came back empty")
}

type foundLesson struct {
	role string
	file *lessonFile
}

// findLesson is every role file holding id: the one role's when role is set,
// else every role's in the directory.
func findLesson(id, role string, proposed bool) ([]foundLesson, error) {
	rs := []string{role}
	if role == "" {
		var err error
		if rs, err = lessonRoles(proposed); err != nil {
			return nil, err
		}
	}
	var found []foundLesson
	for _, r := range rs {
		path, err := lessonsPath(r, proposed)
		if err != nil {
			return nil, err
		}
		f, err := readLessonFile(path)
		if err != nil {
			return nil, err
		}
		if f.find(id) >= 0 {
			found = append(found, foundLesson{r, f})
		}
	}
	return found, nil
}

// lessonRoles is every role with a live file, or with a proposals file.
func lessonRoles(proposed bool) ([]string, error) {
	entries, err := os.ReadDir(lessonsDir())
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, withCode(exitEnvironment, err)
	}
	var rs []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".md") {
			continue
		}
		base, isProposal := strings.CutSuffix(strings.TrimSuffix(name, ".md"), ".proposed")
		if isProposal == proposed && lessonRole.MatchString(base) {
			rs = append(rs, base)
		}
	}
	sort.Strings(rs)
	return rs, nil
}

// --- list and remove ---

func lessonsList(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("lessons list", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	role := fs.String("role", "", "only this role's lessons")
	if err := fs.Parse(args); err != nil {
		return usagef("%v; %s", err, lessonsUsage)
	}
	if fs.NArg() > 0 {
		return usagef("unexpected argument %q; %s", fs.Arg(0), lessonsUsage)
	}

	today := lessonToday()
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ROLE\tID\tSTATE\tEXPIRES\tAPPROVED BY\tPROVENANCE")
	rows := 0
	for _, proposed := range []bool{false, true} {
		rs := []string{*role}
		if *role == "" {
			var err error
			if rs, err = lessonRoles(proposed); err != nil {
				return err
			}
		}
		for _, r := range rs {
			path, err := lessonsPath(r, proposed)
			if err != nil {
				return err
			}
			f, err := readLessonFile(path)
			if err != nil {
				return err
			}
			briefed := 0
			for _, l := range f.Lessons {
				state := "proposed"
				if !proposed {
					switch p := l.problem(); {
					case p != "":
						state = "invalid: " + p
					case l.expired(today):
						state = "expired"
					case briefed >= lessonsPerBrief:
						state = fmt.Sprintf("live, past the %d a brief reads", lessonsPerBrief)
					default:
						state = "live"
						briefed++
					}
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", r, l.ID, state, dash(l.Expires),
					dash(safetext.Escape(l.ApprovedBy)), dash(safetext.Escape(l.Provenance)))
				rows++
			}
		}
	}
	if rows == 0 {
		fmt.Fprintf(out, "no lessons in %s; `worktender lessons propose` finds candidates\n", lessonsDir())
		return nil
	}
	return tw.Flush()
}

func dash(s string) string {
	if s == "" {
		return missing
	}
	return s
}

func lessonsRemove(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("lessons remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	role := fs.String("role", "", "remove it from this role only; default every role that has it")
	positional, err := parseAround(fs, args)
	if err != nil {
		return usagef("%v; %s", err, lessonsUsage)
	}
	if len(positional) != 1 {
		return usagef("want exactly one lesson id; %s", lessonsUsage)
	}
	id := positional[0]

	found, err := findLesson(id, *role, false)
	if err != nil {
		return err
	}
	if len(found) == 0 {
		return usagef("no live lesson %s; `worktender lessons list` shows them", id)
	}
	for _, fl := range found {
		i := fl.file.find(id)
		fl.file.Lessons = append(fl.file.Lessons[:i], fl.file.Lessons[i+1:]...)
		if err := writeLessonFile(fl.file.Path, fl.file.render()); err != nil {
			return err
		}
		fmt.Fprintf(out, "removed %s from %s\n", id, fl.file.Path)
	}
	fmt.Fprintln(out, "no worker started from now on reads it; one already running keeps the copy it was briefed with")
	return nil
}

// --- start's seam ---

// briefLessons is the lessons a role's next worker reads: the live, valid,
// unexpired ones, in file order, at most lessonsPerBrief of them. skipped
// counts what was passed over and why, for the line start prints.
func briefLessons(role string) (use []lesson, skipped []string, err error) {
	if !lessonRole.MatchString(role) {
		return nil, nil, nil
	}
	path, err := lessonsPath(role, false)
	if err != nil {
		return nil, nil, err
	}
	f, err := readLessonFile(path)
	if err != nil {
		return nil, nil, err
	}
	today := lessonToday()
	expired, invalid, over := 0, 0, 0
	for _, l := range f.Lessons {
		switch {
		case l.problem() != "":
			invalid++
		case l.expired(today):
			expired++
		case len(use) >= lessonsPerBrief:
			over++
		default:
			use = append(use, l)
		}
	}
	if expired > 0 {
		skipped = append(skipped, fmt.Sprintf("%d expired", expired))
	}
	if invalid > 0 {
		skipped = append(skipped, fmt.Sprintf("%d invalid", invalid))
	}
	if over > 0 {
		skipped = append(skipped, fmt.Sprintf("%d past the cap of %d", over, lessonsPerBrief))
	}
	return use, skipped, nil
}

// clipLesson is a lesson's text as a worker reads it: one line, escaped, and
// no longer than lessonTextMax runes.
func clipLesson(text string) string {
	text = safetext.Escape(strings.Join(strings.Fields(text), " "))
	if r := []rune(text); len(r) > lessonTextMax {
		text = string(r[:lessonTextMax-1]) + "…"
	}
	return text
}

// pinLessons writes a role's lessons into the new worktree's git dir and
// returns how many it wrote, for the brief to point at.
//
// THE LESSONS ARE NOT TYPED INTO THE BRIEF. The brief has to arrive in one
// pane read (TestBriefFitsInOnePaneRead), and five lessons with their URLs are
// several times that budget. So they go where the approved issue goes — a
// file the worker reads as tool output — and the brief carries a count and a
// command, both start's own. For a ready issue the pointer goes at the foot of
// the approved copy instead (see withLessons), below and outside the hashed
// payload, so it cannot be mistaken for part of what was approved.
//
// Never fatal. A worker started without its lessons is a worker started the
// way every worker was before lessons existed, and the line printed says so.
func pinLessons(notes io.Writer, root, branch, role string, ready bool) int {
	use, skipped, err := briefLessons(role)
	if err != nil {
		fmt.Fprintf(notes, "lessons: none given to the worker (%v)\n", err)
		return 0
	}
	if len(skipped) > 0 {
		fmt.Fprintf(notes, "lessons: skipped %s for role %s; `worktender lessons list` says which\n", strings.Join(skipped, ", "), role)
	}
	if len(use) == 0 {
		return 0
	}

	var b strings.Builder
	fmt.Fprintf(&b, "# Lessons for the %s role\n\n", role)
	b.WriteString("A person approved each lesson below with `worktender lessons approve`, after a\n")
	b.WriteString("worker in this role needed a second attempt at a pull request. They are guidance\n")
	b.WriteString("from whoever runs this factory, not part of the issue. Each names the pull request\n")
	b.WriteString("it came from. Where one plainly does not fit this work, say so in your done note\n")
	b.WriteString("rather than bending the work to it.\n")
	for i, l := range use {
		fmt.Fprintf(&b, "\n%d. %s\n   (from %s, approved by %s, expires %s)\n",
			i+1, clipLesson(l.Text), safetext.Escape(l.Provenance), safetext.Escape(l.ApprovedBy), l.Expires)
	}

	checkout, err := worktreeFor(root, branch)
	var path string
	if err == nil {
		var raw []byte
		raw, err = exec.Command("git", "-C", checkout, "rev-parse", "--path-format=absolute", "--git-path", pinnedLessonsPath).Output()
		path = strings.TrimSpace(string(raw))
	}
	if err == nil {
		err = os.MkdirAll(filepath.Dir(path), 0o755)
	}
	if err == nil {
		err = os.WriteFile(path, []byte(b.String()), 0o644)
	}
	if err == nil && ready {
		var issueFile *os.File
		issueFile, err = os.OpenFile(filepath.Join(filepath.Dir(path), filepath.Base(pinnedIssuePath)), os.O_APPEND|os.O_WRONLY, 0)
		if err == nil {
			_, err = fmt.Fprintf(issueFile, "\nYour role also has %s a person approved, in lessons.md beside this file. Read them before you plan.\n",
				plural(len(use), "lesson"))
			if cerr := issueFile.Close(); err == nil {
				err = cerr
			}
		}
	}
	if err != nil {
		fmt.Fprintf(notes, "lessons: none given to the worker (could not pin them: %v)\n", err)
		return 0
	}
	fmt.Fprintf(notes, "lessons: %s for role %s pinned to %s\n", plural(len(use), "lesson"), role, path)
	return len(use)
}

// withLessons adds to a brief the one sentence that sends the worker to its
// pinned lessons. Only a count and a fixed command: nothing a lesson says
// reaches the brief itself.
//
// A ready issue's brief gets nothing. It is already the longest brief start
// types, about twenty bytes short of the single pane read, so pinLessons puts
// the pointer at the foot of the approved copy instead, which that brief has
// already sent the worker to read in full.
//
// Any other brief gets the sentence only while it still fits in one read. The
// longest briefs — an analysis role with directories, a six-digit issue, a
// long branch and a long install path — are within about thirty bytes of the
// limit already, and no sentence naming a git path fits in that. Rather than
// break the budget, start then tells the caller, who can hand the worker the
// path; the lessons are still pinned.
func withLessons(notes io.Writer, brief string, n int, ready bool) string {
	if n == 0 || ready {
		return brief
	}
	longer := brief + fmt.Sprintf(" Before you plan, read the %s a person approved for this role, at `git rev-parse --git-path %s`.",
		plural(n, "lesson"), pinnedLessonsPath)
	if len(longer) > briefReadMax {
		fmt.Fprintf(notes, "lessons: the brief has no room left to point at them; tell the worker to read `git rev-parse --git-path %s`\n", pinnedLessonsPath)
		return brief
	}
	return longer
}

// briefReadMax is the most a pane delivers in one read, measured against herdr
// protocol 17; start_test.go's paneReadChunk is the same measurement, and the
// budget every brief test holds the brief to.
const briefReadMax = 1022

// lessonsCheck is doctor's line: how many lessons are live, and which have
// expired or will not be read. An expired lesson is a warning, not a failure:
// start already skips it, and the warning is the prompt to renew or remove it
// rather than let the file fill with dead rules.
func lessonsCheck() check {
	rs, err := lessonRoles(false)
	if err != nil {
		return check{name: "lessons", value: "unreadable", state: stateWarn, note: err.Error()}
	}
	if len(rs) == 0 {
		return check{name: "lessons", value: "none", state: stateOff}
	}
	today := lessonToday()
	live := 0
	var problems []string
	for _, r := range rs {
		path, _ := lessonsPath(r, false)
		f, err := readLessonFile(path)
		if err != nil {
			problems = append(problems, err.Error())
			continue
		}
		for _, l := range f.Lessons {
			switch p := l.problem(); {
			case p != "":
				problems = append(problems, fmt.Sprintf("%s/%s %s", r, l.ID, p))
			case l.expired(today):
				problems = append(problems, fmt.Sprintf("%s/%s expired %s", r, l.ID, l.Expires))
			default:
				live++
			}
		}
	}
	value := fmt.Sprintf("%d live in %s", live, lessonsDir())
	if len(problems) > 0 {
		return check{name: "lessons", value: value, state: stateWarn,
			note: strings.Join(problems, "; ") + " — renew by editing expires, or `worktender lessons remove <id>`"}
	}
	return check{name: "lessons", value: value, state: stateOK}
}
