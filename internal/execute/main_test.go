package execute_test

import (
	"os"
	"strings"
	"testing"
)

// TestMain drops the host's WORKTENDER_ settings so the tests see the same
// environment on a developer machine and on the factory; a test that wants
// one sets it with t.Setenv.
func TestMain(m *testing.M) {
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "WORKTENDER_") {
			os.Unsetenv(name)
		}
	}
	os.Exit(m.Run())
}
