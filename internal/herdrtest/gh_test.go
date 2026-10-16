package herdrtest

import (
	"os/exec"
	"testing"
)

// The payload carries exactly what broke #211 in CI: a JSON-escaped newline
// and an escaped backslash, both of which dash's echo would expand. /bin/sh is
// bash on some hosts and dash on others, so the test also runs under dash when
// one is on PATH; where none is, /bin/sh alone still has to pass.
func TestCatScriptIsByteExactUnderSh(t *testing.T) {
	const payload = `{"body":"line one\nline two","path":"C:\\dir"}` + "\n"

	shells := []string{"/bin/sh"}
	if dash, err := exec.LookPath("dash"); err == nil {
		shells = append(shells, dash)
	}

	script := CatScript(t, payload)
	for _, sh := range shells {
		t.Run(sh, func(t *testing.T) {
			out, err := exec.Command(sh, "-c", script).Output()
			if err != nil {
				t.Fatalf("%s -c %q: %v", sh, script, err)
			}
			if string(out) != payload {
				t.Errorf("%s printed %q, want %q", sh, out, payload)
			}
		})
	}
}
