package permissions

import "testing"

func TestAskRequiresExplicitDecisionAndPersistsPermanentGrant(t *testing.T) {
	m := New(ModeAsk, t.TempDir()+"/permissions.json")
	r := Request{Tool: "run_shell", Capability: "tool execution", Level: LevelConfirm, Arguments: `{"command":"echo hi"}`}
	if d, prompt := m.Check(r); d != Deny || !prompt {
		t.Fatalf("check = %q, %v", d, prompt)
	}
	if got := m.Commit(r, AllowPermanent); got != AllowPermanent {
		t.Fatal(got)
	}
	restored := New(ModeAsk, m.StorePath)
	if d, prompt := restored.Check(r); d == Deny || prompt {
		t.Fatalf("restored = %q, %v", d, prompt)
	}
}

func TestDangerousGrantCannotBecomeSessionOrPermanent(t *testing.T) {
	m := New(ModeAllow, "")
	r := Request{Tool: "run_shell", Capability: "tool execution", Level: LevelDangerous, Arguments: "rm -rf /"}
	if _, prompt := m.Check(r); !prompt {
		t.Fatal("dangerous action should prompt")
	}
	if got := m.Commit(r, AllowSession); got != Deny {
		t.Fatalf("got %q", got)
	}
}

func TestPlanDeniesConfirmButAllowsSafe(t *testing.T) {
	m := New(ModePlan, "")
	if d, prompt := m.Check(Request{Tool: "run_shell", Capability: "tool execution", Level: LevelConfirm}); d != Deny || prompt {
		t.Fatalf("plan confirm = %q, %v", d, prompt)
	}
	if d, prompt := m.Check(Request{Tool: "read_file", Level: LevelSafe}); d != AllowOnce || prompt {
		t.Fatalf("plan safe = %q, %v", d, prompt)
	}
}
