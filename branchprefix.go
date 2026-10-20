package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// branchPrefixPattern is what --branch-prefix accepts: nothing, or a lowercase
// type with an optional trailing slash. It is the shape issueBranchPattern can
// read back, so a branch start names is one refocus, review and verify parse.
var branchPrefixPattern = regexp.MustCompile(`^(?:[a-z]+/?)?$`)

// hookTypes matches the `(feat|fix|chore)/` alternation a branch-naming hook
// carries, the one shape these hooks share whether they are shell or node.
var hookTypes = regexp.MustCompile(`\(((?:[a-z]+\|)+[a-z]+)\)/`)

// hookBranchPrefix is the `type/` the repo's own hooks require of a branch
// name, or "" when no hook names one. Detection is best-effort: it reads only
// hook lines that mention "branch" next to a `(a|b)/` alternation, so a hook
// written any other way is missed, and --branch-prefix is the override. Absent means start behaves as it always
// did, so a repo without a naming hook sees no change.
func hookBranchPrefix(root string) string {
	dirs := []string{filepath.Join(root, ".husky")}
	if out, err := exec.Command("git", "-C", root, "rev-parse", "--git-path", "hooks").Output(); err == nil {
		hooks := strings.TrimSpace(string(out))
		if !filepath.IsAbs(hooks) {
			hooks = filepath.Join(root, hooks)
		}
		dirs = append(dirs, hooks)
	}
	for _, dir := range dirs {
		entries, _ := os.ReadDir(dir)
		for _, e := range entries {
			if e.IsDir() || strings.HasSuffix(e.Name(), ".sample") {
				continue
			}
			if p := branchTypeIn(filepath.Join(dir, e.Name())); p != "" {
				return p
			}
		}
	}
	return ""
}

// branchTypeIn reads one hook for a branch-name check. Only lines that talk
// about a branch count, so a commit-message `(feat|fix)/` rule is not mistaken
// for one. feat is preferred when allowed: it is the default type of work.
func branchTypeIn(path string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.Contains(strings.ToLower(line), "branch") {
			continue
		}
		m := hookTypes.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		types := strings.Split(m[1], "|")
		for _, t := range types {
			if t == "feat" {
				return "feat/"
			}
		}
		return types[0] + "/"
	}
	return ""
}
