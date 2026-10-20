package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/steig/worktender/internal/gitx"
)

// approve is the human half of clearing an issue for an agent: it shows a
// person exactly what an agent would receive, and on their say-so records that
// they approved it.
//
// It prints the canonical payload approvalPayload builds — raw, escaped, never
// rendered — and its sha256. On confirmation it posts the hash as a comment and
// applies factory:ready, both with the person's own gh auth: the approval is
// their act, attributable to their account, which is what start checks has
// write access. Nothing here can approve on a coordinator's behalf that the
// coordinator could not already do with gh; the point is that what gets
// approved is a payload someone was shown, not a page someone glanced at.
//
// The comment is posted before the label: a label without an approval is
// refused by start, while an approval without a label is merely not queued.
func approveCommand(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("approve", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	repo := fs.String("repo", ".", "repository the issue belongs to")
	yes := fs.Bool("yes", false, "approve without asking; you are still approving what is printed")

	shown := fs.String("sha256", "", "approve only if the payload hashes to this; skips the prompt")
	pane := fs.Bool("pane", false, "open a pane in the issue's herdr space that asks, instead of asking here")

	positional, err := parseAround(fs, args)
	if err != nil {
		return usagef("%v; %s", err, approveUsage)
	}
	if len(positional) != 1 {
		return usagef("want exactly one issue number; %s", approveUsage)
	}
	number, err := strconv.Atoi(strings.TrimPrefix(positional[0], "#"))
	if err != nil || number <= 0 {
		return usagef("%q is not an issue number; want a positive integer like 42", positional[0])
	}

	if *shown != "" {
		if !sha256Hex.MatchString(*shown) {
			return usagef("--sha256 wants 64 lowercase hex characters; %s", approveUsage)
		}
		if *yes {
			return usagef("--sha256 and --yes do not combine: --sha256 already approves without asking; %s", approveUsage)
		}
	}

	if *pane && (*yes || *shown != "") {
		return usagef("--pane opens the prompt for a person to answer; it does not combine with --yes or --sha256, which skip it; %s", approveUsage)
	}

	if *pane {
		return approvePane(*repo, number, out)
	}

	root, err := gitx.RepoRoot(*repo)
	if err != nil {
		return usagef("--repo: %v", err)
	}

	owner, name, err := ghRepository(root)
	if err != nil {
		return err
	}
	snap, err := fetchIssueSnapshot(root, owner, name, number)
	if err != nil {
		return err
	}
	payload, hash, _, err := approvalPayload(number, snap.Title, snap.Body)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "This is exactly what an agent will receive for %s/%s#%d. Read all of it:\n\n", owner, name, number)
	fmt.Fprint(out, payload)
	fmt.Fprintf(out, "\nsha256: %s\n", hash)

	switch {
	case *shown != "":
		if hash != *shown {
			return withCode(exitNeedsHuman, fmt.Errorf("#%d changed since it was shown (sha256 %s, not %s); nothing was approved", number, hash, *shown))
		}
	case !*yes:
		fmt.Fprint(out, "\nApprove exactly this, and label the issue factory:ready? Type yes: ")
		answer, _ := bufio.NewReader(approveInput).ReadString('\n')
		if strings.TrimSpace(answer) != "yes" {
			return usagef("not approved; nothing was changed")
		}
	}

	target := []string{}
	if origin := gitx.RemoteURL(root); origin != "" {
		target = []string{"--repo", origin}
	}
	comment := approvalCommentPrefix + hash
	if _, err := ghOutput(root, append([]string{"issue", "comment", strconv.Itoa(number), "--body", comment}, target...)...); err != nil {
		return fmt.Errorf("post the approval on #%d: %w", number, err)
	}
	if _, err := ghOutput(root, append([]string{"issue", "edit", strconv.Itoa(number), "--add-label", labelFactoryReady}, target...)...); err != nil {
		return fmt.Errorf("approved #%d, but could not apply %s: %w; apply it yourself, or create the label "+
			"with `gh label create %s`", number, labelFactoryReady, err, labelFactoryReady)
	}
	fmt.Fprintf(out, "\napproved #%d (%s) and labelled it %s\n", number, comment, labelFactoryReady)
	return nil
}

// approvePane is for a caller with no TTY, a coordinator tab above all: it
// opens a shell in the issue's own herdr space and runs the plain
// `worktender approve` there, so the person types yes at the same prompt they
// would have seen. The command it types is built here from the issue number and
// the repository root alone, never with --yes or --sha256.
//
// This is a convenience, not an access control. Anything that can call herdr
// can send keys to that pane, or run `approve --yes` itself. Keeping a
// coordinator from approving takes a credential it does not hold, such as a
// narrowed gh token.
func approvePane(repo string, number int, out io.Writer) error {
	s, err := newSessionIn(repo, herdrRequired)
	if err != nil {
		return err
	}
	list, err := s.client.WorktreeList(s.root)
	if err != nil {
		return fmt.Errorf("list worktrees: %w", err)
	}
	workspace := ""
	for _, w := range list.Worktrees {
		if w.Branch != nil && w.OpenWorkspaceID != nil && issueFromBranch(*w.Branch) == number {
			workspace = *w.OpenWorkspaceID
			break
		}
	}
	if workspace == "" {
		return withCode(exitEnvironment, fmt.Errorf("no open herdr workspace holds a branch for #%d; run `worktender approve %d` in a terminal instead", number, number))
	}
	created, err := s.client.PaneSplit(workspace, s.root)
	if err != nil {
		return fmt.Errorf("open a pane in workspace %s: %w", workspace, err)
	}
	id := created.Pane.PaneID
	warnName(os.Stderr, "the approve pane", s.client.PaneRename(id, "approve"))
	command := fmt.Sprintf("%s approve %d --repo %s", shellQuote(selfPath()), number, shellQuote(s.root))
	if err := s.client.PaneSendText(id, command); err != nil {
		return fmt.Errorf("type the approve command into %s: %w", id, err)
	}
	if err := s.client.PaneSendKeys(id, []string{briefSubmitKey}); err != nil {
		return fmt.Errorf("run the approve command in %s: %w", id, err)
	}
	fmt.Fprintf(out, "opened an approve prompt for #%d in its herdr space; type yes there\n", number)
	return nil
}

// shellQuote makes s one word to a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// approveInput is where the confirmation is read from; a var so the suite can
// answer it.
var approveInput io.Reader = os.Stdin

var sha256Hex = regexp.MustCompile(`^[0-9a-f]{64}$`)

const approveUsage = "usage: worktender approve <issue> [--repo <path>] [--yes | --sha256 <hex> | --pane]"
