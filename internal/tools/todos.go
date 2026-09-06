package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/saurabhahuja71/agenterm/internal/todos"
)

type todoAdd struct{}

func (todoAdd) Name() string        { return "add_todo" }
func (todoAdd) Description() string { return "Add a task to the current Bolt todo list." }
func (todoAdd) Schema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"description"}, "properties": map[string]any{"description": map[string]any{"type": "string"}}}
}
func (todoAdd) Run(_ context.Context, raw string) (string, error) {
	var in struct {
		Description string `json:"description"`
	}
	if err := json.Unmarshal([]byte(raw), &in); err != nil || in.Description == "" {
		return "", fmt.Errorf("description required")
	}
	item := todos.Global.Add(in.Description)
	b, _ := json.Marshal(item)
	return string(b), nil
}

type todoComplete struct{}

func (todoComplete) Name() string        { return "complete_todo" }
func (todoComplete) Description() string { return "Mark a Bolt todo complete." }
func (todoComplete) Schema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"todo_id"}, "properties": map[string]any{"todo_id": map[string]any{"type": "string"}}}
}
func (todoComplete) Run(_ context.Context, raw string) (string, error) {
	var in struct {
		ID string `json:"todo_id"`
	}
	_ = json.Unmarshal([]byte(raw), &in)
	if !todos.Global.Complete(in.ID) {
		return "", fmt.Errorf("todo not found: %s", in.ID)
	}
	return "completed " + in.ID, nil
}

type todoUpdate struct{}

func (todoUpdate) Name() string        { return "update_todo" }
func (todoUpdate) Description() string { return "Update a Bolt todo description." }
func (todoUpdate) Schema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"todo_id", "description"}, "properties": map[string]any{"todo_id": map[string]any{"type": "string"}, "description": map[string]any{"type": "string"}}}
}
func (todoUpdate) Run(_ context.Context, raw string) (string, error) {
	var in struct {
		ID          string `json:"todo_id"`
		Description string `json:"description"`
	}
	_ = json.Unmarshal([]byte(raw), &in)
	if !todos.Global.Update(in.ID, in.Description) {
		return "", fmt.Errorf("todo not found or description empty: %s", in.ID)
	}
	return "updated " + in.ID, nil
}

type todoList struct{}

func (todoList) Name() string        { return "list_todos" }
func (todoList) Description() string { return "List the current Bolt todos." }
func (todoList) Schema() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}
func (todoList) Run(context.Context, string) (string, error) { return todos.Global.Summary(), nil }
