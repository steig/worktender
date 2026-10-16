package main

import (
	"bufio"
	"flag"
	"fmt"
	"io"
	"os"
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

	if !*yes {
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

// approveInput is where the confirmation is read from; a var so the suite can
// answer it.
var approveInput io.Reader = os.Stdin

const approveUsage = "usage: worktender approve <issue> [--repo <path>] [--yes]"
