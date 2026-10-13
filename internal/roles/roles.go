// Package roles reads the user's role configuration and resolves which role a
// slice of work runs as.
//
// A role is a Claude Code agent and a brief style, nothing more. The agent file
// carries the model, the system prompt and whatever MCP servers the domain
// needs, so the expertise lives where it also works outside worktender, and
// this package only chooses among agents the user has already written.
//
// The configuration is the user's alone. There is no repository layer, and
// that is the security model rather than an omission: a file a cloned
// repository could ship would choose the directories and agents a worker is
// started with, and nothing here would be in a position to tell that apart
// from the user's own choice. Labels are the one input from outside, and they
// only ever select a role the user already defined.
//
// One input the user does not control survives, and it is Claude Code's rather
// than this package's: claude prefers a repository's .claude/agents/<name>.md
// over the user's file of the same name, so a repository can redefine the agent
// a role names — its prompt, its model, its tools. That is true of every claude
// session started in that repository, with or without worktender, so it is not
// refused here; it is reported. CheckAgent records which file will be used, and
// the printed line and the JSON both say whether it was the repository's.
//
// Neither the model nor the permission mode is configurable here. The model
// belongs to the agent file, and `--model` on the command line overrides it —
// verified against Claude Code 2.1.296, see #185. Autonomy stays a flag for the
// reason it is never defaulted anywhere else in this plugin.
package roles

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"

	"github.com/steig/worktender/internal/safetext"
)

// Briefs are the brief styles a role may name. "code" is the brief `start`
// has always typed.
var Briefs = map[string]bool{"code": true}

// DefaultBrief is what a role that names no brief style gets.
const DefaultBrief = "code"

// Config is the user's role configuration, as read from Path.
type Config struct {
	// Path is the file this was read from, for messages and for doctor.
	Path string `json:"-"`

	DefaultRole string          `json:"default_role"`
	Labels      []LabelRule     `json:"labels"`
	Roles       map[string]Role `json:"roles"`
}

// LabelRule maps an issue label to a role. Match is a label name, or a prefix
// followed by a single trailing `*`.
type LabelRule struct {
	Match string `json:"match"`
	Role  string `json:"role"`
}

// Role is one way of starting a worker.
type Role struct {
	// Agent is a Claude Code agent name: `.claude/agents/<agent>.md` in the
	// repository or in the user's home. Empty means claude's own default.
	Agent string `json:"agent"`
	// Brief is the brief style; empty means DefaultBrief.
	Brief string `json:"brief"`
	// AddDirs are directories the worker may read beside its worktree. `~`
	// expands to the home directory; a relative path is relative to the
	// repository.
	AddDirs []string `json:"add_dirs"`
}

// agentName is what a Claude Code agent file may be called. It is checked
// because the name becomes a path segment in the preflight.
var agentName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]*$`)

// Path is where the configuration lives: $XDG_CONFIG_HOME/worktender/config.json,
// falling back to ~/.config — on darwin too, the rule herdr uses for its own
// directory, so the two sit side by side.
func Path() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "worktender", "config.json")
}

// Load reads the configuration at Path. A missing file is not an error: it is
// the configuration everyone has until they write one, and it resolves to no
// role at all, which is exactly what worktender did before roles existed.
func Load() (*Config, error) {
	path := Path()
	if path == "" {
		return nil, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return Parse(path, data)
}

// Parse decodes and validates a configuration. Unknown keys are refused rather
// than ignored: a misspelt "add_dir" that silently does nothing is a worker
// started without the material its role promised.
func Parse(path string, data []byte) (*Config, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.Path = path
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

func (c *Config) validate() error {
	for name, r := range c.Roles {
		if r.Agent != "" && !agentName.MatchString(r.Agent) {
			return fmt.Errorf("roles.%s.agent: %q is not an agent name", name, r.Agent)
		}
		if r.Brief != "" && !Briefs[r.Brief] {
			return fmt.Errorf("roles.%s.brief: %q is not a brief style; want one of %s", name, r.Brief, briefList())
		}
		for i, d := range r.AddDirs {
			if strings.TrimSpace(d) == "" {
				return fmt.Errorf("roles.%s.add_dirs[%d] is empty", name, i)
			}
		}
	}
	if c.DefaultRole != "" {
		if _, ok := c.Roles[c.DefaultRole]; !ok {
			return fmt.Errorf("default_role: %q is not a defined role", c.DefaultRole)
		}
	}
	for i, l := range c.Labels {
		if l.Match == "" || l.Match == "*" {
			return fmt.Errorf("labels[%d].match: %q matches every label; use default_role for that", i, l.Match)
		}
		if strings.Contains(strings.TrimSuffix(l.Match, "*"), "*") {
			return fmt.Errorf("labels[%d].match: %q — only a single trailing * is supported", i, l.Match)
		}
		if _, ok := c.Roles[l.Role]; !ok {
			return fmt.Errorf("labels[%d].role: %q is not a defined role", i, l.Role)
		}
	}
	return nil
}

func briefList() string {
	names := make([]string, 0, len(Briefs))
	for b := range Briefs {
		names = append(names, b)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

// Launch is a resolved role: what to start the agent with, and why.
type Launch struct {
	Role string
	// Reason is how the role was chosen: "flag", "label <name>" or "default".
	Reason string
	Agent  string
	// AgentFile is the definition claude will load for Agent, set by
	// CheckAgent; AgentSource is "repo" or "user" accordingly.
	AgentFile   string
	AgentSource string
	// AddDirs are resolved to absolute paths.
	AddDirs []string
	Brief   string
}

// ErrUnknownRole is a --role naming a role the configuration does not define.
var ErrUnknownRole = errors.New("unknown role")

// Resolve picks the role for one slice: the flag, else the first label rule
// matching any of the issue's labels, else default_role. A nil configuration,
// or one with nothing that applies, resolves to no role — a nil Launch — and
// the caller behaves as it did before roles existed.
//
// Rules are tried in the order they are written, and each against every label,
// so the user's ordering decides between two labels rather than GitHub's.
func (c *Config) Resolve(flagRole string, labels []string, repoRoot string) (*Launch, error) {
	if c == nil {
		if flagRole != "" {
			return nil, fmt.Errorf("%w %q: no role configuration at %s", ErrUnknownRole, flagRole, Path())
		}
		return nil, nil
	}

	name, reason := "", ""
	switch {
	case flagRole != "":
		if _, ok := c.Roles[flagRole]; !ok {
			return nil, fmt.Errorf("%w %q: %s defines %s", ErrUnknownRole, flagRole, c.Path, c.roleList())
		}
		name, reason = flagRole, "flag"
	default:
		for _, rule := range c.Labels {
			if label, ok := rule.matchAny(labels); ok {
				name, reason = rule.Role, "label "+label
				break
			}
		}
		if name == "" && c.DefaultRole != "" {
			name, reason = c.DefaultRole, "default"
		}
	}
	if name == "" {
		return nil, nil
	}

	role := c.Roles[name]
	launch := &Launch{Role: name, Reason: reason, Agent: role.Agent, Brief: role.Brief}
	if launch.Brief == "" {
		launch.Brief = DefaultBrief
	}
	for _, d := range role.AddDirs {
		abs, err := ExpandDir(d, repoRoot)
		if err != nil {
			return nil, fmt.Errorf("role %s: %w", name, err)
		}
		launch.AddDirs = append(launch.AddDirs, abs)
	}
	return launch, nil
}

func (c *Config) roleList() string {
	if len(c.Roles) == 0 {
		return "no roles"
	}
	names := make([]string, 0, len(c.Roles))
	for n := range c.Roles {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (r LabelRule) matchAny(labels []string) (string, bool) {
	prefix, glob := strings.CutSuffix(r.Match, "*")
	for _, l := range labels {
		if l == r.Match || (glob && strings.HasPrefix(l, prefix)) {
			return l, true
		}
	}
	return "", false
}

// ExpandDir resolves one add_dirs entry: `~` to the home directory, a relative
// path against the repository. A relative path with no repository is an error.
func ExpandDir(d, repoRoot string) (string, error) {
	switch {
	case d == "~" || strings.HasPrefix(d, "~/"):
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("expand %s: %w", d, err)
		}
		return filepath.Join(home, strings.TrimPrefix(d, "~")), nil
	case filepath.IsAbs(d):
		return filepath.Clean(d), nil
	case repoRoot == "":
		return "", fmt.Errorf("add_dirs entry %q is relative and there is no repository to resolve it against", d)
	default:
		return filepath.Join(repoRoot, d), nil
	}
}

// Args are the agent arguments the launch contributes. --model and
// --permission-mode are not among them: both come from flags only.
func (l *Launch) Args() []string {
	if l == nil {
		return nil
	}
	var args []string
	if l.Agent != "" {
		args = append(args, "--agent", l.Agent)
	}
	for _, d := range l.AddDirs {
		args = append(args, "--add-dir", d)
	}
	return args
}

// Line is the one line `start` and `dispatch` print about the choice, so the
// coordinator can check it rather than trust it.
//
// Escaped, because the reason can carry an issue label, and a label is text
// whoever can edit the issue wrote.
func (l *Launch) Line() string {
	if l == nil {
		return ""
	}
	var b strings.Builder
	fmt.Fprintf(&b, "role: %s (%s)", l.Role, l.Reason)
	if l.Agent != "" {
		fmt.Fprintf(&b, " · agent: %s", l.Agent)
		if l.AgentSource != "" {
			fmt.Fprintf(&b, " (%s)", l.AgentSource)
		}
	}
	if l.Brief != DefaultBrief {
		fmt.Fprintf(&b, " · brief: %s", l.Brief)
	}
	if len(l.AddDirs) > 0 {
		fmt.Fprintf(&b, " · dirs: %s", strings.Join(l.AddDirs, ", "))
	}
	return safetext.Escape(b.String())
}

// AgentDirs are the directories Claude Code reads agent definitions from, in
// the order it prefers them: the repository's own, then the user's.
func AgentDirs(repoRoot string) []string {
	var dirs []string
	if repoRoot != "" {
		dirs = append(dirs, filepath.Join(repoRoot, ".claude", "agents"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		dirs = append(dirs, filepath.Join(home, ".claude", "agents"))
	}
	return dirs
}

// FindAgent is the definition under dir that claude loads for `--agent name`,
// or "" when there is none.
//
// An agent is known by the `name:` in its frontmatter, not by its file name,
// and claude reads subdirectories too — both measured against Claude Code
// 2.1.296, where agents/sub/whatever.md declaring `name: probe2` answered to
// `--agent probe2`. A check by file name would refuse that agent, and worse,
// report the user's file as the one in use while a differently named file in
// the repository was the one that loaded. A file with no name in its
// frontmatter is matched by its stem. Two files declaring one name resolve to
// the first in lexical order; which one claude picks is not known.
func FindAgent(dir, name string) string {
	found := ""
	_ = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != "" {
			return nil
		}
		if d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		declared, ok := frontmatterName(path)
		if !ok {
			declared = strings.TrimSuffix(d.Name(), ".md")
		}
		if declared == name {
			found = path
		}
		return nil
	})
	return found
}

// frontmatterName reads `name:` from a leading `---` block.
func frontmatterName(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	rest, ok := strings.CutPrefix(text, "---\n")
	if !ok {
		return "", false
	}
	for line := range strings.SplitSeq(rest, "\n") {
		if strings.TrimSpace(line) == "---" {
			break
		}
		if value, ok := strings.CutPrefix(line, "name:"); ok {
			return strings.Trim(strings.TrimSpace(value), `"'`), true
		}
	}
	return "", false
}

// CheckAgent refuses a launch whose agent has no definition anywhere claude
// will look, and records which definition claude will use. Run before anything
// is created: `claude --agent` with a name it cannot find fails inside the
// pane, after the worktree already exists.
//
// Only the repository's and the user's agent directories are searched. An
// agent a Claude Code plugin provides is real to claude and invisible here, so
// it is refused; define it in one of those directories to use it in a role.
func (l *Launch) CheckAgent(repoRoot string) error {
	if l == nil || l.Agent == "" {
		return nil
	}
	dirs := AgentDirs(repoRoot)
	for i, dir := range dirs {
		if path := FindAgent(dir, l.Agent); path != "" {
			l.AgentFile, l.AgentSource = path, "user"
			if repoRoot != "" && i == 0 {
				l.AgentSource = "repo"
			}
			return nil
		}
	}
	return fmt.Errorf("role %s names agent %q, and no agent of that name is defined under %s",
		l.Role, l.Agent, strings.Join(dirs, " or "))
}

// LaunchJSON is the launch as `start --json` and `dispatch --json` carry it.
type LaunchJSON struct {
	Role   string  `json:"role"`
	Reason string  `json:"reason"`
	Agent  *string `json:"agent"`
	// AgentFile is the definition claude will load, and AgentSource whether it
	// is the repository's ("repo") or the user's ("user"). A repository's file
	// takes precedence over the user's of the same name.
	AgentFile   *string  `json:"agent_file"`
	AgentSource *string  `json:"agent_source"`
	AddDirs     []string `json:"add_dirs"`
	Brief       string   `json:"brief"`
}

// JSON is nil for no role, so the field is null rather than an empty object
// that reads like a role with nothing in it.
func (l *Launch) JSON() *LaunchJSON {
	if l == nil {
		return nil
	}
	out := &LaunchJSON{Role: l.Role, Reason: l.Reason, AddDirs: l.AddDirs, Brief: l.Brief}
	if out.AddDirs == nil {
		out.AddDirs = []string{}
	}
	if l.Agent != "" {
		agent := l.Agent
		out.Agent = &agent
	}
	if l.AgentFile != "" {
		file, source := l.AgentFile, l.AgentSource
		out.AgentFile, out.AgentSource = &file, &source
	}
	return out
}
