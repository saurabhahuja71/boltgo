package todos

import (
	"fmt"
	"sync"
)

type Item struct {
	ID, Description string
	Completed       bool
}
type Store struct {
	mu    sync.RWMutex
	items []Item
	next  int
}

func NewStore() *Store { return &Store{next: 1} }
func (s *Store) Add(description string) Item {
	s.mu.Lock()
	defer s.mu.Unlock()
	item := Item{ID: fmt.Sprintf("todo_%d", s.next), Description: description}
	s.next++
	s.items = append(s.items, item)
	return item
}
func (s *Store) Update(id, description string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id && description != "" {
			s.items[i].Description = description
			return true
		}
	}
	return false
}
func (s *Store) Complete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.items {
		if s.items[i].ID == id {
			s.items[i].Completed = true
			return true
		}
	}
	return false
}
func (s *Store) Items() []Item {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]Item, len(s.items))
	copy(out, s.items)
	return out
}
func (s *Store) Summary() string {
	items := s.Items()
	if len(items) == 0 {
		return "(no todos)"
	}
	out := ""
	for _, item := range items {
		mark := "[ ]"
		if item.Completed {
			mark = "[x]"
		}
		out += fmt.Sprintf("%s %s. %s\n", mark, item.ID, item.Description)
	}
	return out
}

var Global = NewStore()
