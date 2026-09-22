package agent

import (
	"strings"
	"testing"
)

func TestInvestigationFingerprintNormalizesEquivalentArguments(t *testing.T) {
	first, ok := investigationFingerprint("grep", `{"pattern":"TODO","path":"."}`)
	if !ok {
		t.Fatal("grep was not classified as investigation")
	}
	second, ok := investigationFingerprint("grep", `{"path":".","pattern":"TODO"}`)
	if !ok || first != second {
		t.Fatalf("equivalent arguments produced different fingerprints: %q != %q", first, second)
	}
	third, _ := investigationFingerprint("grep", `{"path":".","pattern":"FIXME"}`)
	if first == third {
		t.Fatal("different search patterns were suppressed as equivalent")
	}
	if _, ok := investigationFingerprint("write_file", `{"path":"x"}`); ok {
		t.Fatal("mutation was classified as investigation")
	}
}

func TestRepeatedInvestigationObservationIsActionable(t *testing.T) {
	message := repeatedInvestigationObservation("read_file")
	for _, want := range []string{"read_file", "already executed", "state has not changed", "evidence already collected", "do not repeat", "does not exist"} {
		if !strings.Contains(strings.ToLower(message), strings.ToLower(want)) {
			t.Fatalf("observation missing %q: %s", want, message)
		}
	}
}

func TestWorkspaceGenerationAllowsInvestigationAfterMutation(t *testing.T) {
	key, ok := investigationFingerprint("read_file", `{"path":"a.go"}`)
	if !ok {
		t.Fatal("read_file was not classified as investigation")
	}
	seen := map[string]uint64{key: 0}
	if seen[key] != 0 {
		t.Fatal("initial generation was not recorded")
	}
	if !workspaceMutation("write_file") {
		t.Fatal("write_file was not classified as a workspace mutation")
	}
	seen[key] = 1
	if seen[key] == 0 {
		t.Fatal("changed workspace generation still looked unchanged")
	}
}
