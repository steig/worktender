package roles

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const sample = `{
  "default_role": "engineer",
  "labels": [
    { "match": "role:ux", "role": "ux" },
    { "match": "area:warehouse*", "role": "warehousing" }
  ],
  "roles": {
    "engineer":    { "brief": "code" },
    "ux":          { "agent": "ux-designer", "add_dirs": ["~/kb/design", "docs/design"] },
    "warehousing": { "agent": "warehousing-analyst" }
  }
}`

func parse(t *testing.T, data string) *Config {
	t.Helper()
	c, err := Parse("config.json", []byte(data))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	return c
}

// No file is the configuration everyone starts with, and it must resolve to
// nothing at all — the behaviour worktender had before roles existed.
func TestLoadWithoutAFileIsNoConfigurationAndNoRole(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())

	c, err := Load()
	if err != nil || c != nil {
		t.Fatalf("Load() = %v, %v; want nil, nil", c, err)
	}
	launch, err := c.Resolve("", []string{"role:ux"}, "/repo")
	if err != nil || launch != nil {
		t.Fatalf("Resolve on no configuration = %v, %v; want nil, nil", launch, err)
	}
	if args := launch.Args(); args != nil {
		t.Errorf("no role contributed args %v", args)
	}
}

func TestLoadReadsTheXDGPath(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, "worktender"), 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "worktender", "config.json")
	if err := os.WriteFile(path, []byte(sample), 0o644); err != nil {
		t.Fatal(err)
	}

	c, err := Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if c.Path != path || len(c.Roles) != 3 {
		t.Errorf("loaded %s with %d roles; want %s with 3", c.Path, len(c.Roles), path)
	}
}

func TestPathFallsBackToDotConfig(t *testing.T) {
	home := t.TempDir()
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("HOME", home)

	if got, want := Path(), filepath.Join(home, ".config", "worktender", "config.json"); got != want {
		t.Errorf("Path() = %s; want %s", got, want)
	}
}

func TestParseRefuses(t *testing.T) {
	for name, tc := range map[string]struct{ data, want string }{
		"an unknown key":               {`{"roles":{"a":{"add_dir":["x"]}}}`, `unknown field "add_dir"`},
		"an unknown brief":             {`{"roles":{"a":{"brief":"poetry"}}}`, `roles.a.brief: "poetry" is not a brief style`},
		"a default that is not a role": {`{"default_role":"nope","roles":{}}`, `default_role: "nope"`},
		"a label pointing nowhere":     {`{"labels":[{"match":"x","role":"nope"}],"roles":{}}`, `labels[0].role: "nope"`},
		"a label matching everything":  {`{"labels":[{"match":"*","role":"a"}],"roles":{"a":{}}}`, `matches every label`},
		"a glob that is not a suffix":  {`{"labels":[{"match":"a*b","role":"a"}],"roles":{"a":{}}}`, `single trailing *`},
		"an agent name that is a path": {`{"roles":{"a":{"agent":"../../etc/x"}}}`, `is not an agent name`},
		"an empty add_dir":             {`{"roles":{"a":{"add_dirs":[" "]}}}`, `add_dirs[0] is empty`},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := Parse("config.json", []byte(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Parse error = %v; want it to mention %q", err, tc.want)
			}
			if !strings.HasPrefix(err.Error(), "config.json: ") {
				t.Errorf("error does not name the file: %v", err)
			}
		})
	}
}

// The flag beats a label, a label beats the default, and the first rule in the
// file decides between two labels — the user's order, not GitHub's.
func TestResolveOrder(t *testing.T) {
	c := parse(t, sample)
	for name, tc := range map[string]struct {
		flag         string
		labels       []string
		role, reason string
	}{
		"flag over label":       {"warehousing", []string{"role:ux"}, "warehousing", "flag"},
		"label over default":    {"", []string{"bug", "role:ux"}, "ux", "label role:ux"},
		"prefix glob":           {"", []string{"area:warehouse-ops"}, "warehousing", "label area:warehouse-ops"},
		"rule order wins":       {"", []string{"area:warehouse", "role:ux"}, "ux", "label role:ux"},
		"default when no label": {"", []string{"bug"}, "engineer", "default"},
		"default with none":     {"", nil, "engineer", "default"},
	} {
		t.Run(name, func(t *testing.T) {
			l, err := c.Resolve(tc.flag, tc.labels, "/repo")
			if err != nil {
				t.Fatalf("Resolve: %v", err)
			}
			if l.Role != tc.role || l.Reason != tc.reason {
				t.Errorf("resolved %s (%s); want %s (%s)", l.Role, l.Reason, tc.role, tc.reason)
			}
		})
	}
}

func TestResolveWithoutADefaultIsNoRole(t *testing.T) {
	c := parse(t, `{"roles":{"ux":{"agent":"ux"}},"labels":[{"match":"role:ux","role":"ux"}]}`)
	l, err := c.Resolve("", []string{"bug"}, "/repo")
	if err != nil || l != nil {
		t.Fatalf("Resolve = %v, %v; want no role", l, err)
	}
}

func TestResolveRefusesAFlagNamingNoRole(t *testing.T) {
	c := parse(t, sample)
	_, err := c.Resolve("designer", nil, "/repo")
	if !errors.Is(err, ErrUnknownRole) || !strings.Contains(err.Error(), "engineer, ux, warehousing") {
		t.Fatalf("err = %v; want ErrUnknownRole listing the roles", err)
	}

	var none *Config
	if _, err := none.Resolve("ux", nil, "/repo"); !errors.Is(err, ErrUnknownRole) {
		t.Fatalf("--role with no configuration: err = %v; want ErrUnknownRole", err)
	}
}

// A label can choose a role and nothing else. The arguments a role produces are
// the agent and the directories; the model and the permission mode never come
// from configuration at all.
func TestArgsCarryAgentAndDirsOnly(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	c := parse(t, sample)

	l, err := c.Resolve("", []string{"role:ux"}, "/repo")
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := []string{
		"--agent", "ux-designer",
		"--add-dir", filepath.Join(home, "kb", "design"),
		"--add-dir", "/repo/docs/design",
	}
	if got := l.Args(); !reflect.DeepEqual(got, want) {
		t.Errorf("Args() = %v; want %v", got, want)
	}
	for _, a := range l.Args() {
		if a == "--model" || a == "--permission-mode" {
			t.Errorf("a role produced %s", a)
		}
	}
	if l.Brief != DefaultBrief {
		t.Errorf("a role naming no brief got %q; want %q", l.Brief, DefaultBrief)
	}
}

func TestCheckAgentLooksInTheRepositoryThenHome(t *testing.T) {
	home, repo := t.TempDir(), t.TempDir()
	t.Setenv("HOME", home)
	l := &Launch{Role: "ux", Agent: "ux-designer"}

	err := l.CheckAgent(repo)
	if err == nil {
		t.Fatal("no agent file anywhere, and the check passed")
	}
	for _, p := range []string{
		filepath.Join(repo, ".claude", "agents", "ux-designer.md"),
		filepath.Join(home, ".claude", "agents", "ux-designer.md"),
	} {
		if !strings.Contains(err.Error(), p) {
			t.Errorf("error does not name %s: %v", p, err)
		}
	}

	for _, dir := range []string{repo, home} {
		t.Run(dir, func(t *testing.T) {
			path := filepath.Join(dir, ".claude", "agents", "ux-designer.md")
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte("---\nname: ux-designer\n---\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			defer os.Remove(path)
			if err := l.CheckAgent(repo); err != nil {
				t.Errorf("agent at %s not found: %v", path, err)
			}
		})
	}
}

func TestLineSaysWhatWasChosenAndWhy(t *testing.T) {
	l := &Launch{Role: "ux", Reason: "label role:ux", Agent: "ux-designer", Brief: DefaultBrief}
	if got, want := l.Line(), "role: ux (label role:ux) · agent: ux-designer"; got != want {
		t.Errorf("Line() = %q; want %q", got, want)
	}
	var none *Launch
	if none.Line() != "" || none.JSON() != nil {
		t.Error("no role should print nothing and serialise to null")
	}
}
