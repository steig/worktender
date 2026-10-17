package main

import (
	"bufio"
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"sort"

	"github.com/steig/worktender/internal/wt"
)

// What a dispatch spent, in tokens, read from its worker's own Claude Code
// transcripts. Tokens rather than dollars: a price table in code would be
// wrong the first time a price changed, and nobody would notice.
//
// The ledger holds only the session ids, recorded by `report` while herdr could
// still name them; the counts are read here, at stats time, from wherever
// Claude filed the session. A transcript that is gone is skipped without a
// word, and the dispatch's tokens read as unknown rather than zero.

// tokenCounts is a sum of the usage Claude reports per API response.
// CacheWrite is not one of the three asked for, but leaving it out would make
// a session that mostly built its cache look nearly free.
type tokenCounts struct {
	Input      int64 `json:"input_tokens"`
	Output     int64 `json:"output_tokens"`
	CacheRead  int64 `json:"cache_read_tokens"`
	CacheWrite int64 `json:"cache_write_tokens"`
}

func (c *tokenCounts) add(o tokenCounts) {
	c.Input += o.Input
	c.Output += o.Output
	c.CacheRead += o.CacheRead
	c.CacheWrite += o.CacheWrite
}

type modelTokens struct {
	Model string `json:"model"`
	tokenCounts
}

// countTokens reads the transcripts of every dispatch that recorded a session.
func countTokens(dispatches []*statsDispatch) {
	home, err := os.UserHomeDir()
	if err != nil {
		return
	}
	for _, d := range dispatches {
		d.tokens = sessionTokens(home, d.sessions)
	}
}

// sessionTokens sums the usage in the sessions' transcripts by model, sorted by
// model, and nil when no transcript was found.
//
// Each id is looked for wherever Claude filed it — the project directory is
// derived from a cwd that is Claude's to name — along with the transcripts of
// any subagents it ran, which were its spending too. A response is counted
// once by its message id: Claude writes one line per content block, each
// repeating the response's usage, and a session resumed from another directory
// is filed twice.
func sessionTokens(home string, sessions []string) []modelTokens {
	var files []string
	for _, id := range sessions {
		if !wt.PlainSessionID(id) {
			continue
		}
		for _, pattern := range []string{
			filepath.Join(home, ".claude", "projects", "*", id+".jsonl"),
			filepath.Join(home, ".claude", "projects", "*", id, "subagents", "*.jsonl"),
		} {
			matches, _ := filepath.Glob(pattern)
			files = append(files, matches...)
		}
	}
	if len(files) == 0 {
		return nil
	}

	byModel := map[string]*tokenCounts{}
	seen := map[string]bool{}
	for _, path := range files {
		file, err := os.Open(path)
		if err != nil {
			continue
		}
		// Not a Scanner: a line carrying a tool result can run to megabytes.
		r := bufio.NewReader(file)
		for {
			line, err := r.ReadBytes('\n')
			if bytes.Contains(line, []byte(`"usage"`)) {
				var entry struct {
					Type    string `json:"type"`
					Message struct {
						ID    string `json:"id"`
						Model string `json:"model"`
						Usage struct {
							Input      int64 `json:"input_tokens"`
							Output     int64 `json:"output_tokens"`
							CacheRead  int64 `json:"cache_read_input_tokens"`
							CacheWrite int64 `json:"cache_creation_input_tokens"`
						} `json:"usage"`
					} `json:"message"`
				}
				if json.Unmarshal(line, &entry) == nil && entry.Type == "assistant" && entry.Message.ID != "" && !seen[entry.Message.ID] {
					seen[entry.Message.ID] = true
					// Zero usage is a message Claude Code made up itself — an
					// interruption, an error — not a response anyone paid for.
					if u := tokenCounts(entry.Message.Usage); u != (tokenCounts{}) {
						if byModel[entry.Message.Model] == nil {
							byModel[entry.Message.Model] = &tokenCounts{}
						}
						byModel[entry.Message.Model].add(u)
					}
				}
			}
			if err != nil {
				break
			}
		}
		file.Close()
	}
	// Transcripts holding no paid response say nothing about what the work
	// cost, so they are unknown like a missing one, not a zero.
	if len(byModel) == 0 {
		return nil
	}
	return sortedByModel(byModel)
}

func sortedByModel(byModel map[string]*tokenCounts) []modelTokens {
	tokens := []modelTokens{}
	for model, c := range byModel {
		tokens = append(tokens, modelTokens{Model: model, tokenCounts: *c})
	}
	sort.Slice(tokens, func(i, j int) bool { return tokens[i].Model < tokens[j].Model })
	return tokens
}

// summariseTokens is the merged dispatches' tokens: totals by model, and the
// median of each count per merged pull request, over those whose transcripts
// were found.
func summariseTokens(dispatches []*statsDispatch) (byModel []modelTokens, median *tokenCounts, counted int) {
	totals := map[string]*tokenCounts{}
	var input, output, cacheRead, cacheWrite []int64
	for _, d := range dispatches {
		if m := d.merged(); m == nil || !*m || d.tokens == nil {
			continue
		}
		var sum tokenCounts
		for _, t := range d.tokens {
			if totals[t.Model] == nil {
				totals[t.Model] = &tokenCounts{}
			}
			totals[t.Model].add(t.tokenCounts)
			sum.add(t.tokenCounts)
		}
		input, output = append(input, sum.Input), append(output, sum.Output)
		cacheRead, cacheWrite = append(cacheRead, sum.CacheRead), append(cacheWrite, sum.CacheWrite)
	}
	if len(input) == 0 {
		return sortedByModel(totals), nil, 0
	}
	median = &tokenCounts{Input: medianInt64(input), Output: medianInt64(output), CacheRead: medianInt64(cacheRead), CacheWrite: medianInt64(cacheWrite)}
	return sortedByModel(totals), median, len(input)
}

func medianInt64(vs []int64) int64 {
	slices.Sort(vs)
	if len(vs)%2 == 0 {
		return (vs[len(vs)/2-1] + vs[len(vs)/2]) / 2
	}
	return vs[len(vs)/2]
}
