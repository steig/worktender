package main

import "testing"

const brief77 = "Read issue #77 and do what it asks"

func TestBriefTakenEchoed(t *testing.T) {
	screen := "❯ Read issue #77 and do what it asks\n\n⏺ Reading the issue\n\n❯ \n"
	if !briefTaken(screen, brief77) {
		t.Error("an echoed brief above an empty box should count as taken")
	}
}

func TestBriefTakenNotWithoutEcho(t *testing.T) {
	screen := "✔ Update installed · /restart to apply\n\n❯ Try \"fix typecheck errors\"\n"
	if briefTaken(screen, brief77) {
		t.Error("an empty box with no transcript echo should not count as taken")
	}
}

func TestBriefTakenNotWhileHeld(t *testing.T) {
	screen := "❯ Read issue #77 and do what it asks\n"
	if briefTaken(screen, brief77) {
		t.Error("a brief still in the box should not count as taken")
	}
}
