package main

import (
	"fmt"
	"strings"

	"github.com/steig/worktender/internal/herdrapi"
)

// A gatekeeper screen is one Claude Code draws in place of its composer and
// that a keypress answers on the user's behalf. `start` opens a fresh worktree,
// a directory Claude Code has never seen, so the first thing it may draw there
// is the folder-trust dialog — and the Enter that submits the brief would accept
// it, permanently and with nobody looking. herdr probably classifies the dialog
// as `blocked`, which submitBrief counts as the brief having landed, so the
// press would also be reported as a success (#196).
//
// The answer is the one eliasstravik/herdr-projects reached in trust_screen.rs:
// read the visible screen before typing anything, and hand a match to a person.
//
// The needles are matched case-insensitively against the visible screen with
// its whitespace collapsed. They are kept short, because the visible source is
// the terminal as wrapped: a long sentence can arrive split across two lines
// with a dialog border between the halves, and a needle spanning the break
// matches nothing.
//
// WHERE THE TEXT CAME FROM, and how sure it is:
//
//   - "Yes, I trust this folder", "Accessing workspace:", and the two "...you
//     created or one you trust" sentences are string literals in the Claude Code
//     2.1.300 binary, read out of it with `strings`. The trust dialog itself was
//     not rendered and read back through herdr, so these are the dialog's source
//     text, not a captured screen.
//   - "trust the files in this folder" is the wording older Claude Code builds
//     used ("Do you trust the files in this folder?"). It is not in 2.1.300 and is
//     kept from memory of earlier versions and from #196, unverified.
//   - "hooks need review" comes from #196's description of the hooks-review
//     screen. No such string is in 2.1.300 — there, a folder whose settings carry
//     hooks says so inside the trust dialog, which the needles above already
//     catch — and it was not verified against any build. It stays because a
//     false positive costs a person one look, and a miss costs an accepted hook.
var gatekeeperScreens = []struct {
	name    string
	needles []string
}{
	{"folder-trust", []string{
		"yes, i trust this folder",
		"accessing workspace:",
		"is this a project you created or one you trust",
		"is this a directory you created or one you trust",
		"trust the files in this folder",
	}},
	{"hooks-review", []string{
		"hooks need review",
	}},
}

// gatekeeperScreen names the gatekeeper screen text shows, or "" for none.
func gatekeeperScreen(text string) string {
	screen := strings.ToLower(strings.Join(strings.Fields(text), " "))
	for _, s := range gatekeeperScreens {
		for _, needle := range s.needles {
			if strings.Contains(screen, needle) {
				return s.name
			}
		}
	}
	return ""
}

// refuseGatekeeper reads the pane and fails, needing a human, if it is showing
// a gatekeeper screen. It is called before every keystroke the brief sends.
//
// A read that fails is a failure too, rather than a reason to type on regardless:
// the whole point is to never press a key on a screen nobody has looked at, and
// a herdr that cannot answer pane.read is not one whose other answers are worth
// acting on.
func refuseGatekeeper(client *herdrapi.Client, pane string) error {
	read, err := client.PaneRead(pane, herdrapi.ReadSourceVisible)
	if err != nil {
		return fmt.Errorf("read %s before typing the brief: %w", pane, err)
	}
	if screen := gatekeeperScreen(read.Read.Text); screen != "" {
		return codef(exitNeedsHuman,
			"%s is showing Claude Code's %s screen, so nothing was typed or pressed there — "+
				"answering it is a decision for a person; look with `herdr pane read %s --source visible`, "+
				"answer it in the pane, then brief the agent yourself",
			pane, screen, pane)
	}
	return nil
}
