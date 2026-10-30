package main

import (
	"strings"
	"testing"
)

const unlabelledMeta = `{"number":42,"title":"Fix the thing","labels":[{"name":"bug"}]}`

// The caller's hash is the whole approval: no label, no comment.
func TestStartWithSha256ProceedsWithoutAComment(t *testing.T) {
	r := newReadyStart(t)
	newFakeGitHub(t, unlabelledMeta, snapshotJSON(t, readyTitle, readyBody), nil)
	payload, hash, _, _ := approvalPayload(42, readyTitle, readyBody)

	if err := r.run("--sha256", hash); err != nil {
		t.Fatalf("start: %v", err)
	}

	pinned := pinnedCopy(t, r.checkout)
	if !strings.Contains(pinned, payload) || !strings.Contains(pinned, hash) {
		t.Errorf("the pinned copy must carry the payload byte for byte and its hash:\n%s", pinned)
	}
	if strings.Contains(pinned, "@") && strings.Contains(pinned, "approved the payload") {
		t.Errorf("the pinned copy names an approver on the --sha256 path:\n%s", pinned)
	}
	if !strings.Contains(briefSent(r.server), "Run its 2 acceptance checks") {
		t.Errorf("the worker was not briefed on the approved checks: %q", briefSent(r.server))
	}
}

// A valid approval comment from an admin does not rescue a wrong --sha256, so
// the refusal is the hash's alone.
func TestStartWithSha256RefusesAMismatch(t *testing.T) {
	r := newReadyStart(t)
	newFakeGitHub(t, readyMeta,
		snapshotJSON(t, readyTitle, readyBody, comment{"steig", approvalOf(t, readyTitle, readyBody)}),
		map[string]string{"steig": "admin"})

	err := r.run("--sha256", strings.Repeat("0", 64))

	_, hash, _, _ := approvalPayload(42, readyTitle, readyBody)
	assertRefusedBeforeAnyAgent(t, r.server, err, hash)
	if !strings.Contains(err.Error(), strings.Repeat("0", 64)) {
		t.Errorf("the refusal should name the hash it was given too: %v", err)
	}
}

func TestStartRequiresApprovalWhenTheHostAsks(t *testing.T) {
	t.Setenv(requireApprovalEnv, "1")
	r := newReadyStart(t)
	newFakeGitHub(t, unlabelledMeta, snapshotJSON(t, readyTitle, readyBody), nil)

	assertRefusedBeforeAnyAgent(t, r.server, r.run(), "not approved")

	t.Setenv(requireApprovalEnv, "0")
	r = newReadyStart(t)
	newFakeGitHub(t, unlabelledMeta, snapshotJSON(t, readyTitle, readyBody), nil)
	if err := r.run(); err != nil {
		t.Errorf("with the variable at 0 an unlabelled issue starts as before: %v", err)
	}
}

func TestStartSha256IsAUsageErrorWhenMisused(t *testing.T) {
	r := newReadyStart(t)
	gh := newFakeGitHub(t, unlabelledMeta, snapshotJSON(t, readyTitle, readyBody), nil)
	_, hash, _, _ := approvalPayload(42, readyTitle, readyBody)

	for _, value := range []string{strings.ToUpper(hash), "abc", strings.Repeat("g", 64)} {
		if err := r.run("--sha256", value); err == nil || exitCode(err) != exitUsage {
			t.Errorf("--sha256 %q: want a usage error; got %v", value, err)
		}
	}
	if calls := gh.read("calls"); calls != "" {
		t.Errorf("a misused --sha256 reached gh:\n%s", calls)
	}
}
