package main

import (
	_ "embed"
	"fmt"
	"io"
	"regexp"
)

// manifest is embedded so the reported version is the one release PRs bump,
// with no -ldflags for scripts/build.sh and a plain go build to disagree on.
//
//go:embed herdr-plugin.toml
var manifest string

var embeddedVersion = regexp.MustCompile(`(?m)^version\s*=\s*"([^"]+)"`)

func versionCommand(args []string, out io.Writer) error {
	if len(args) > 0 {
		return usagef("version takes no arguments")
	}
	m := embeddedVersion.FindStringSubmatch(manifest)
	if m == nil {
		return withCode(exitEnvironment, fmt.Errorf("herdr-plugin.toml has no version"))
	}
	_, err := fmt.Fprintln(out, m[1])
	return err
}
