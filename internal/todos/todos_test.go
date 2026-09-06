package todos

import "testing"

func TestStorePendingAndCompletedItems(t *testing.T) {
	s := NewStore()
	item := s.Add("inspect config")
	if got := s.Summary(); got != "[ ] "+item.ID+". inspect config\n" {
		t.Fatalf("pending = %q", got)
	}
	if !s.Complete(item.ID) {
		t.Fatal("complete failed")
	}
	if got := s.Summary(); got != "[x] "+item.ID+". inspect config\n" {
		t.Fatalf("completed = %q", got)
	}
}
