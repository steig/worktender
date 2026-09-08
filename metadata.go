package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/steig/worktender/internal/herdrapi"
	"github.com/steig/worktender/internal/wt"
)

// The metadata channel is how a report actually reaches a gate.
//
// The pane alone does not carry a report off a Claude Code worker: that TUI
// collapses a finished tool call to "Ran 1 shell command", so a worker which
// runs `worktender report` leaves the envelope in its transcript and nothing on
// screen. herdr's pane metadata tokens do not depend on what a TUI renders.
//
// The note is chunked across slots because it is 200 runes against a slot that
// holds 80, and a coordinator that releases on a `done` with no summary has to
// go and read the pane anyway. Chunking is safe because the joined note runs
// back through reportNote before it is a report at all, so reassembly cannot
// widen what the writer's validation allows.
//
// One report is identified by a counter, never by content: the slots are a
// fixed template, so two dispatches of the same slice produce byte-identical
// reports and a reader comparing them would hear the second as a repeat. The
// counter is not a predicate surface; it decides only whether there is
// something new to judge.

const (
	// tokenSource is the provenance herdr files these writes under. It is NOT a
	// namespace — herdr keeps one token map per pane and every writer merges
	// into it — so the keys below carry the namespace themselves.
	tokenSource = "steig.worktender"

	// tokenValueLimit is how much of a token value herdr keeps, in runes.
	// Measured on herdr 0.7.5: 81 runes in stores 80, with no error, no warning
	// and no mention of the limit in the schema.
	tokenValueLimit = 80
)

// The keys, all under one prefix because the map is shared with every other
// writer on the pane, and versioned because the presence of tokenKeyVersion is
// what distinguishes "worktender wrote a report here" from "some keys exist".
const (
	tokenKeyVersion = "worktender_v"
	tokenKeyStatus  = "worktender_status"
	tokenKeyPR      = "worktender_pr"
	// The note's chunks are this plus their index: worktender_note0, worktender_note1…
	tokenKeyNotePrefix = "worktender_note"
	// worktender_seq counts reports on this pane: it says which report this
	// is, never what it says.
	tokenKeySeq = "worktender_seq"
)

// tokenVersion is the layout above. It moves when a reader of the old layout
// would misread the new one, and a reader that does not recognise it treats the
// pane as carrying no report rather than guessing.
const tokenVersion = "1"

// noteChunks is how many slots a full-length note needs. Derived from the two
// limits so the layout cannot drift from either of them.
const noteChunks = (noteLimit + tokenValueLimit - 1) / tokenValueLimit

func noteChunkKey(i int) string { return fmt.Sprintf("%s%d", tokenKeyNotePrefix, i) }

// encodeReport lays a report out across tokens.
//
// Every slot this plugin owns is written on every report, and unused note
// chunks are written as explicit nulls: a write merges, so a chunk left unnamed
// is left over from the previous report, and the next reader would join a
// sentence neither report contained.
func encodeReport(r report, seq uint64) (map[string]any, error) {
	// Zero is what a reader uses for "this pane carries no report", so a report
	// claiming it could never be read back.
	if seq == 0 {
		return nil, fmt.Errorf("a report cannot be numbered 0; %s counts from 1", tokenKeySeq)
	}

	tokens := map[string]any{
		tokenKeyVersion: tokenVersion,
		tokenKeyStatus:  r.status,
		tokenKeyPR:      prSlot(r),
		tokenKeySeq:     strconv.FormatUint(seq, 10),
	}

	chunks := chunkRunes(r.note, tokenValueLimit)
	if len(chunks) > noteChunks {
		return nil, fmt.Errorf("the note needs %d token slots and the layout has %d; it cannot be delivered without losing the end of it", len(chunks), noteChunks)
	}
	for i := range noteChunks {
		if i < len(chunks) {
			tokens[noteChunkKey(i)] = chunks[i]
			continue
		}
		tokens[noteChunkKey(i)] = nil
	}

	// Names the slot rather than the symptom: herdr cuts an over-long value and
	// says nothing, so a slot past the limit would otherwise surface as a report
	// that fails to confirm for no visible reason.
	for key, value := range tokens {
		text, ok := value.(string)
		if !ok {
			continue
		}
		if n := utf8.RuneCountInString(text); n > tokenValueLimit {
			return nil, fmt.Errorf("token %s is %d characters and herdr stores %d, cutting the rest without saying so", key, n, tokenValueLimit)
		}
	}
	return tokens, nil
}

// decodeReport reads a report back out of a pane's tokens, all-or-nothing,
// along with the sequence that identifies it; the sequence is zero exactly when
// there is no report. Every slot goes back through the validator the writer
// used, so a token map assembled by something else is rejected rather than
// half-read.
func decodeReport(tokens map[string]string) (report, uint64, bool) {
	if tokens[tokenKeyVersion] != tokenVersion {
		return report{}, 0, false
	}

	seq, ok := parseSeqValue(tokens[tokenKeySeq])
	if !ok {
		return report{}, 0, false
	}

	status := tokens[tokenKeyStatus]
	if !isReportStatus(status) {
		return report{}, 0, false
	}

	pr, ok := parsePRValue(tokens[tokenKeyPR])
	if !ok {
		return report{}, 0, false
	}

	note, ok := joinNoteChunks(tokens)
	if !ok {
		return report{}, 0, false
	}
	// The joined note, not the chunks: the join is what a coordinator is shown,
	// so the join is what has to survive validation.
	if reportNote(note) != nil {
		return report{}, 0, false
	}
	return report{status: status, pr: pr, note: note}, seq, true
}

// parseSeqValue reads the counter slot. Absent, zero, or anything that is not a
// plain decimal is not a sequence, and a report without one is not a report.
func parseSeqValue(raw string) (uint64, bool) {
	seq, err := strconv.ParseUint(raw, 10, 64)
	if err != nil || seq == 0 {
		return 0, false
	}
	return seq, true
}

// nextSeq is the number the report about to be written gets: one past whatever
// the pane already carries, or one when there is no readable counter.
//
// A restart can never make a gate release early — a number below the mark is
// not news — but it can cost the restarting report, which needs another writer
// in this pane's token map to happen at all.
func nextSeq(tokens map[string]string) uint64 {
	seq, ok := parseSeqValue(tokens[tokenKeySeq])
	if !ok {
		return 1
	}
	return seq + 1
}

// joinNoteChunks reassembles the note from contiguous slots. A gap is fatal
// rather than skipped: encodeReport nulls the chunks it does not use, so a hole
// with a later chunk beyond it is a shape this plugin cannot produce, and
// joining across it would splice two reports into one sentence.
func joinNoteChunks(tokens map[string]string) (string, bool) {
	var note strings.Builder
	for i := range noteChunks {
		chunk, present := tokens[noteChunkKey(i)]
		if present {
			note.WriteString(chunk)
			continue
		}
		for j := i + 1; j < noteChunks; j++ {
			if _, later := tokens[noteChunkKey(j)]; later {
				return "", false
			}
		}
		break
	}
	return note.String(), true
}

// maxBoundaryShift bounds how far chunkRunes will pull a split point back to
// dodge whitespace. A real note's word gaps are a handful of runes at most, so
// this comfortably covers them. The cap matters for the input a real note
// never has: a long run of interior whitespace (reportNote does not limit run
// length), which would otherwise erode the boundary one rune at a time toward
// a zero-width chunk, spraying a note across far more than noteChunks slots
// and rejecting a note that was never actually too long. Past this cap the
// boundary is left on whitespace, and confirmTokens is what catches the
// resulting mismatch at delivery — the same path that already catches
// herdr's other undocumented cuts.
const maxBoundaryShift = 16

// chunkRunes splits by runes rather than bytes: the limit is counted in runes,
// and a byte-wise split would cut a character in half.
//
// A split point is pulled back off a run of whitespace when a plain size-wide
// cut would land on one: herdr trims a stored value's leading and trailing
// whitespace before this plugin's write ever gets a reply (issue #166 — a note
// that split "raw SQL error, PR open" into "raw SQL error," and " PR open"
// came back missing that second chunk's leading space), the same undocumented
// behavior as the length cut above. A chunk boundary on whitespace is
// therefore not a chunk that reassembles into what the worker wrote, so the
// boundary moves instead of the content — up to maxBoundaryShift.
func chunkRunes(s string, size int) []string {
	var chunks []string
	runes := []rune(s)
	for start := 0; start < len(runes); {
		end := min(start+size, len(runes))
		floor := max(start+1, end-maxBoundaryShift)
		for end > floor && end < len(runes) && (unicode.IsSpace(runes[end-1]) || unicode.IsSpace(runes[end])) {
			end--
		}
		chunks = append(chunks, string(runes[start:end]))
		start = end
	}
	return chunks
}

// deliverReport writes a report to the pane's metadata and proves it landed.
// The second return names what was missing when there was nowhere to deliver
// to: running `worktender report` outside herdr is legitimate, so delivery is
// skipped rather than refused, and the caller says which variable was absent.
func deliverReport(r report) (string, error) {
	for _, name := range []string{paneEnv, socketEnv} {
		if os.Getenv(name) == "" {
			return name, nil
		}
	}

	client, err := herdrapi.New()
	if err != nil {
		return "", err
	}
	return "", writeReport(client, os.Getenv(paneEnv), r)
}

const (
	// paneEnv is the pane herdr injects into the shell it starts. A report is
	// never written to a pane its author does not occupy, which would let a
	// worker file under another worker's name.
	paneEnv = "HERDR_PANE_ID"

	socketEnv = "HERDR_SOCKET_PATH"
)

// writeReport numbers the report, delivers the tokens, and reads them back.
//
// The number comes from the pane rather than this process, which is new every
// time. herdr's own `seq` param cannot help: it is write-only, so a reader
// could not tell the guard had fired.
//
// The read-back is the point. herdr cuts an over-long value, strips control
// characters, and drops an empty one — returning ok for all three — so a writer
// trusting the reply would report a delivered envelope the coordinator reads
// mangled. Comparing what came back also covers modes nobody has measured yet.
func writeReport(client *herdrapi.Client, paneID string, r report) error {
	before, err := client.PaneGet(paneID)
	if err != nil {
		return fmt.Errorf("read pane %s before reporting to it: %w", paneID, err)
	}

	tokens, err := encodeReport(r, nextSeq(before.Pane.Tokens))
	if err != nil {
		return err
	}
	if err := client.PaneReportMetadata(paneID, tokenSource, tokens); err != nil {
		return fmt.Errorf("deliver report to pane %s: %w", paneID, err)
	}

	info, err := client.PaneGet(paneID)
	if err != nil {
		return fmt.Errorf("confirm report on pane %s: %w", paneID, err)
	}
	return confirmTokens(paneID, tokens, info.Pane.Tokens)
}

// confirmTokens fails on the first slot herdr stored differently from what it
// was given, naming both so the failure says what was lost rather than that
// something was.
func confirmTokens(paneID string, want map[string]any, stored map[string]string) error {
	for key, value := range want {
		got, present := stored[key]
		text, wanted := value.(string)

		switch {
		case !wanted && present:
			return fmt.Errorf("report not delivered intact to pane %s: herdr kept %s as %q after it was cleared", paneID, key, got)
		case !wanted:
			continue
		case !present:
			return fmt.Errorf("report not delivered intact to pane %s: herdr stored no %s at all", paneID, key)
		case got != text:
			return fmt.Errorf("report not delivered intact to pane %s: herdr stored %s as %q, not the %q this report wrote", paneID, key, got, text)
		}
	}
	return nil
}

// paneReport is the listing's report lookup: what the worker in a pane last
// told its coordinator, read back off the pane's own herdr metadata.
//
// It lives here rather than in the listing because decoding the envelope is
// this command's business — internal/wt renders a column and should not know
// the token layout, the note chunking, or which slots go back through a
// validator.
//
// A pane that carries no report is not an error. It is the ordinary answer for
// a worker that has not reported yet, and the caller distinguishes it from an
// unreadable pane by Found rather than by err.
func paneReport(paneID string) (wt.Report, error) {
	client, err := herdrapi.New()
	if err != nil {
		return wt.Report{}, err
	}
	info, err := client.PaneGet(paneID)
	if err != nil {
		return wt.Report{}, err
	}
	r, _, ok := decodeReport(info.Pane.Tokens)
	if !ok {
		return wt.Report{}, nil
	}
	return wt.Report{Found: true, Status: r.status, PR: r.pr, Note: r.note}, nil
}
