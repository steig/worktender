package main

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// repoConfName is the file a repository uses to say how worktender works in it.
const repoConfName = ".worktender.toml"

// repoConf is what a repository declares. A nil field is "not declared", which
// is not the same as "": branch_prefix = "" declares no prefix, and so stops
// hook detection.
type repoConf struct {
	BranchPrefix *string
	Base         *string
}

// loadRepoConf reads .worktender.toml from root, which must be the main
// checkout and never a worker's worktree: the worker's pull request can edit
// the file, and a worker must not choose the settings it is started under.
// A missing file is the configuration every repository has until it writes one.
// Unknown keys are warned about on warn and ignored; a line that does not parse
// is a usage error naming it.
//
// Only the TOML this needs is read: `key = "string"` lines, blank lines and
// comments. A table or any other value type is refused rather than guessed at.
func loadRepoConf(root string, warn io.Writer) (*repoConf, error) {
	path := filepath.Join(root, repoConfName)
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return &repoConf{}, nil
	}
	if err != nil {
		return nil, withCode(exitEnvironment, fmt.Errorf("read %s: %w", path, err))
	}

	conf := &repoConf{}
	for i, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, rest, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || strings.ContainsAny(key, " \t[]\".") {
			return nil, usagef("%s:%d: want key = \"value\", got %q", repoConfName, i+1, line)
		}
		value, err := repoConfString(rest)
		if err != nil {
			return nil, usagef("%s:%d: %s: %v", repoConfName, i+1, key, err)
		}
		switch key {
		case "branch_prefix":
			if !branchPrefixPattern.MatchString(value) {
				return nil, usagef("%s:%d: branch_prefix %q is not a type like feat or feat/", repoConfName, i+1, value)
			}
			conf.BranchPrefix = &value
		case "base":
			conf.Base = &value
		default:
			fmt.Fprintf(warn, "worktender: %s:%d: unknown key %q ignored\n", repoConfName, i+1, key)
		}
	}
	return conf, nil
}

// repoConfString reads a basic ("...") or literal ('...') string and nothing
// after it but an optional comment.
func repoConfString(s string) (string, error) {
	s = strings.TrimSpace(s)
	var value, rest string
	switch {
	case strings.HasPrefix(s, `"`):
		quoted, err := strconv.QuotedPrefix(s)
		if err != nil {
			return "", errors.New("unterminated string")
		}
		value, _ = strconv.Unquote(quoted)
		rest = s[len(quoted):]
	case strings.HasPrefix(s, "'"):
		end := strings.Index(s[1:], "'")
		if end < 0 {
			return "", errors.New("unterminated string")
		}
		value, rest = s[1:1+end], s[end+2:]
	default:
		return "", errors.New("want a quoted string")
	}
	if rest = strings.TrimSpace(rest); rest != "" && !strings.HasPrefix(rest, "#") {
		return "", fmt.Errorf("unexpected %q after the string", rest)
	}
	return value, nil
}
