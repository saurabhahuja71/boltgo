package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Client talks to any OpenAI-compatible Chat Completions API
// (Ollama, xAI, OpenAI, vLLM, LocalAI, …).
type Client struct {
	BaseURL    string
	APIKey     string
	HTTPClient *http.Client
}

func New(baseURL, apiKey string) *Client {
	// Streaming has no overall Timeout (models can load for minutes), but we
	// must bound time-to-first-byte so a wedged Ollama does not hang forever.
	var transport http.RoundTripper
	if base, ok := http.DefaultTransport.(*http.Transport); ok {
		t := base.Clone()
		t.ResponseHeaderTimeout = 8 * time.Minute
		t.IdleConnTimeout = 90 * time.Second
		transport = t
	} else {
		transport = &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			ResponseHeaderTimeout: 8 * time.Minute,
			IdleConnTimeout:       90 * time.Second,
		}
	}
	return &Client{
		BaseURL: strings.TrimRight(baseURL, "/"),
		APIKey:  apiKey,
		HTTPClient: &http.Client{
			Timeout:   0, // body stream can be long; use context + header timeout
			Transport: transport,
		},
	}
}

type Role string

const (
	RoleSystem    Role = "system"
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type Message struct {
	Role         Role       `json:"role"`
	Content      string     `json:"content,omitempty"`
	Name         string     `json:"name,omitempty"`
	ToolCallID   string     `json:"tool_call_id,omitempty"`
	ToolCalls    []ToolCall `json:"tool_calls,omitempty"`
	Usage        *Usage     `json:"-"`
	FinishReason string     `json:"-"`
}

// Usage is provider-reported token accounting. Providers that do not expose
// usage leave it nil, allowing the TUI to show an honest fallback.
type Usage struct {
	PromptTokens     int `json:"prompt_tokens,omitempty"`
	CompletionTokens int `json:"completion_tokens,omitempty"`
	TotalTokens      int `json:"total_tokens,omitempty"`
}

type ToolCall struct {
	ID string `json:"id"`
	// Index identifies a call when a provider omits its ID on continuation
	// chunks. A pointer distinguishes an omitted index from index zero.
	Index    *int         `json:"index,omitempty"`
	Type     string       `json:"type"`
	Function FunctionCall `json:"function"`
}

// UnmarshalJSON accepts both the OpenAI function wrapper and provider variants
// that put the function name/arguments directly on the tool call. Keeping this
// normalization here ensures streamed and non-streamed calls reach the same
// dispatcher arguments instead of silently becoming {}.
func (t *ToolCall) UnmarshalJSON(data []byte) error {
	var raw struct {
		ID         string          `json:"id"`
		Index      *int            `json:"index"`
		Type       string          `json:"type"`
		Function   json.RawMessage `json:"function"`
		Name       string          `json:"name"`
		Tool       string          `json:"tool"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
		Args       json.RawMessage `json:"args"`
		Path       string          `json:"path"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	t.ID, t.Index, t.Type = raw.ID, raw.Index, raw.Type
	if len(raw.Function) > 0 && string(raw.Function) != "null" {
		if err := json.Unmarshal(raw.Function, &t.Function); err != nil {
			return err
		}
	}
	if t.Function.Name == "" {
		t.Function.Name = raw.Name
		if t.Function.Name == "" {
			t.Function.Name = raw.Tool
		}
	}
	if t.Function.Arguments == "" {
		arguments := raw.Arguments
		if len(arguments) == 0 || string(arguments) == "null" {
			arguments = raw.Parameters
		}
		if len(arguments) == 0 || string(arguments) == "null" {
			arguments = raw.Args
		}
		if (len(arguments) == 0 || string(arguments) == "null") && raw.Path != "" {
			arguments, _ = json.Marshal(map[string]string{"path": raw.Path})
		}
		if len(arguments) > 0 && string(arguments) != "null" {
			if arguments[0] == '"' {
				var s string
				if err := json.Unmarshal(arguments, &s); err != nil {
					return err
				}
				t.Function.Arguments = s
			} else {
				t.Function.Arguments = string(arguments)
			}
		}
	}
	return nil
}

type FunctionCall struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// UnmarshalJSON accepts arguments as a JSON string or object/array.
// SGLang/Ollama usually send a string; some servers emit a nested object,
// which would otherwise fail the whole tool_call and drop str_replace.
func (f *FunctionCall) UnmarshalJSON(data []byte) error {
	var raw struct {
		Name       string          `json:"name"`
		Arguments  json.RawMessage `json:"arguments"`
		Parameters json.RawMessage `json:"parameters"`
		Args       json.RawMessage `json:"args"`
		Path       string          `json:"path"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	f.Name = raw.Name
	arguments := raw.Arguments
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = raw.Parameters
	}
	if len(arguments) == 0 || string(arguments) == "null" {
		arguments = raw.Args
	}
	if (len(arguments) == 0 || string(arguments) == "null") && raw.Path != "" {
		arguments, _ = json.Marshal(map[string]string{"path": raw.Path})
	}
	if len(arguments) == 0 || string(arguments) == "null" {
		f.Arguments = ""
		return nil
	}
	// Already a JSON string value
	if arguments[0] == '"' {
		var s string
		if err := json.Unmarshal(arguments, &s); err != nil {
			return err
		}
		f.Arguments = s
		return nil
	}
	// Object or array → keep as compact JSON text for tool runners
	f.Arguments = string(arguments)
	return nil
}

type Tool struct {
	Type     string             `json:"type"` // "function"
	Function ToolFunctionSchema `json:"function"`
}

type ToolFunctionSchema struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters,omitempty"`
}

type ChatRequest struct {
	Model    string    `json:"model"`
	Messages []Message `json:"messages"`
	Tools    []Tool    `json:"tools,omitempty"`
	// ToolChoice: "auto" | "none" | or {"type":"function","function":{"name":"..."}}
	// Omit when empty. Helps Ollama/OpenAI skip tools for pure chat.
	ToolChoice    any            `json:"tool_choice,omitempty"`
	Temperature   float64        `json:"temperature,omitempty"`
	MaxTokens     int            `json:"max_tokens,omitempty"`
	Stream        bool           `json:"stream"`
	StreamOptions *StreamOptions `json:"stream_options,omitempty"`
}

type StreamOptions struct {
	IncludeUsage bool `json:"include_usage"`
}

type ChatResponse struct {
	Choices []struct {
		Message      Message `json:"message"`
		FinishReason string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string     `json:"content"`
			ToolCalls []ToolCall `json:"tool_calls"`
			Role      string     `json:"role"`
		} `json:"delta"`
		// Message is non-stream shape. Some proxies (e.g. sglang-toolcall-proxy
		// before delta rewrite) emit a full completion as one SSE frame.
		Message      *Message `json:"message,omitempty"`
		FinishReason *string  `json:"finish_reason"`
	} `json:"choices"`
	Usage *Usage `json:"usage,omitempty"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// StreamHandler receives partial content and final message.
type StreamHandler interface {
	OnToken(token string)
	OnToolCallDelta(index int, tc ToolCall)
	// OnStatus is optional progress (model load, first byte, …). May be no-op.
	OnStatus(text string)
}

// statusEmitter is implemented by handlers that want progress callbacks.
type statusEmitter interface {
	OnStatus(text string)
}

func emitStatus(h StreamHandler, text string) {
	if h == nil {
		return
	}
	if s, ok := h.(statusEmitter); ok {
		s.OnStatus(text)
	}
}

// ChatStream streams a completion; returns the assembled assistant message.
func (c *Client) ChatStream(ctx context.Context, req ChatRequest, h StreamHandler) (Message, error) {
	req.Stream = true
	if req.StreamOptions == nil {
		req.StreamOptions = &StreamOptions{IncludeUsage: true}
	}
	body, err := json.Marshal(req)
	if err != nil {
		return Message{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}

	emitStatus(h, "connecting to "+c.BaseURL+" …")
	resp, err := c.HTTPClient.Do(httpReq)
	if err != nil {
		return Message{}, fmt.Errorf("chat request failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode >= 300 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Message{}, fmt.Errorf("API %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	emitStatus(h, "streaming "+req.Model+" (first token can wait while Ollama loads the model)…")

	// Non-SSE JSON fallback (some servers ignore stream)
	ct := resp.Header.Get("Content-Type")
	if strings.Contains(ct, "application/json") && !strings.Contains(ct, "event-stream") {
		var full ChatResponse
		if err := json.NewDecoder(resp.Body).Decode(&full); err != nil {
			return Message{}, err
		}
		if full.Error != nil {
			return Message{}, fmt.Errorf("API error: %s", full.Error.Message)
		}
		if len(full.Choices) == 0 {
			return Message{}, fmt.Errorf("empty choices")
		}
		msg := full.Choices[0].Message
		canonicalizeToolCallTypes(msg.ToolCalls)
		msg.Usage = full.Usage
		msg.FinishReason = full.Choices[0].FinishReason
		if h != nil && msg.Content != "" {
			h.OnToken(msg.Content)
		}
		return msg, nil
	}

	msg := Message{Role: RoleAssistant}
	// Accumulate each call independently. Providers commonly omit IDs on
	// continuation chunks, so the index is retained in ToolCall and used as a
	// second identity. The order slice is only a fallback for providers that
	// omit both fields (which is unambiguous when there is one call, or when a
	// response repeats the same positional list).
	toolAcc := map[string]*ToolCall{}
	toolOrder := []string{}
	gotToken := false

	sc := bufio.NewScanner(resp.Body)
	// Increase buffer for large tool payloads
	buf := make([]byte, 0, 64*1024)
	sc.Buffer(buf, 1024*1024)

	for sc.Scan() {
		if err := ctx.Err(); err != nil {
			return msg, err
		}
		line := sc.Text()
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue
		}
		if chunk.Error != nil {
			return Message{}, fmt.Errorf("API error: %s", chunk.Error.Message)
		}
		if len(chunk.Choices) == 0 {
			if chunk.Usage != nil {
				msg.Usage = chunk.Usage
			}
			continue
		}
		if chunk.Usage != nil {
			msg.Usage = chunk.Usage
		}
		ch0 := chunk.Choices[0]
		if ch0.FinishReason != nil {
			msg.FinishReason = *ch0.FinishReason
		}
		delta := ch0.Delta
		// Full message stuffed into SSE (proxy bug / non-stream JSON over event-stream).
		if ch0.Message != nil && delta.Content == "" && len(delta.ToolCalls) == 0 {
			m := ch0.Message
			if m.Content != "" {
				if !gotToken {
					gotToken = true
					emitStatus(h, "receiving tokens…")
				}
				msg.Content += m.Content
				if h != nil {
					h.OnToken(m.Content)
				}
			}
			for i, tc := range m.ToolCalls {
				acc := accumulateToolCall(toolAcc, &toolOrder, i, tc)
				if h != nil {
					h.OnToolCallDelta(toolCallIndex(acc, i), *acc)
				}
			}
			continue
		}
		if delta.Content != "" {
			if !gotToken {
				gotToken = true
				emitStatus(h, "receiving tokens…")
			}
			msg.Content += delta.Content
			if h != nil {
				h.OnToken(delta.Content)
			}
		}
		for i, tc := range delta.ToolCalls {
			acc := accumulateToolCall(toolAcc, &toolOrder, i, tc)
			if h != nil {
				h.OnToolCallDelta(toolCallIndex(acc, i), *acc)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return Message{}, err
	}

	if len(toolAcc) > 0 {
		// stable order by index
		for i, key := range toolOrder {
			if tc := toolAcc[key]; tc != nil {
				if tc.Type != "function" {
					tc.Type = "function"
				}
				if tc.ID == "" {
					tc.ID = fmt.Sprintf("call_%d_%d", time.Now().UnixNano(), i)
				}
				if tc.Type == "" {
					tc.Type = "function"
				}
				msg.ToolCalls = append(msg.ToolCalls, *tc)
			}
		}
	}
	return msg, nil
}

// canonicalizeToolCallTypes prevents malformed provider response metadata from
// being persisted and echoed in the next OpenAI-compatible request. The only
// tool-call discriminator supported by the Chat Completions wire format is
// "function"; the call ID, function name, and assembled arguments are kept.
func canonicalizeToolCallTypes(calls []ToolCall) {
	for i := range calls {
		if calls[i].Type != "function" {
			calls[i].Type = "function"
		}
	}
}

func toolCallIndex(tc *ToolCall, fallback int) int {
	if tc != nil && tc.Index != nil {
		return *tc.Index
	}
	return fallback
}

// accumulateToolCall merges one provider delta into exactly one call state.
// ID and index are both accepted because OpenAI-compatible providers often
// send the ID only on the first chunk and the index on later chunks.
func accumulateToolCall(calls map[string]*ToolCall, order *[]string, position int, delta ToolCall) *ToolCall {
	key := ""
	if delta.ID != "" {
		key = "id:" + delta.ID
		if calls[key] == nil && delta.Index != nil {
			key = findToolCallByIndex(calls, *delta.Index)
		}
	}
	if key == "" && delta.Index != nil {
		key = findToolCallByIndex(calls, *delta.Index)
		if key == "" {
			key = fmt.Sprintf("index:%d", *delta.Index)
		}
	}
	if key == "" && position < len(*order) {
		key = (*order)[position]
	}
	if key == "" {
		key = fmt.Sprintf("anonymous:%d", len(*order))
		*order = append(*order, key)
	}
	acc := calls[key]
	if acc == nil {
		acc = &ToolCall{Type: "function", Index: delta.Index}
		calls[key] = acc
		if len(*order) == 0 || (*order)[len(*order)-1] != key {
			*order = append(*order, key)
		}
	}
	if delta.ID != "" {
		acc.ID = delta.ID
	}
	if delta.Index != nil {
		acc.Index = delta.Index
	}
	if delta.Type != "" {
		acc.Type = delta.Type
	}
	if delta.Function.Name != "" {
		acc.Function.Name = mergeToolArgument(acc.Function.Name, delta.Function.Name)
	}
	if delta.Function.Arguments != "" {
		acc.Function.Arguments = mergeToolArgument(acc.Function.Arguments, delta.Function.Arguments)
	}
	return acc
}

func findToolCallByIndex(calls map[string]*ToolCall, index int) string {
	for key, call := range calls {
		if call.Index != nil && *call.Index == index {
			return key
		}
	}
	return ""
}

// mergeToolArgument handles both delta fragments and proxies that emit a
// cumulative full-message argument on successive SSE frames. It never lets a
// later empty/short fragment erase an already accumulated path.
func mergeToolArgument(existing, incoming string) string {
	if existing == "" {
		return incoming
	}
	if incoming == "" || strings.HasPrefix(incoming, existing) {
		return incoming
	}
	if strings.HasPrefix(existing, incoming) {
		return existing
	}
	return existing + incoming
}

// Chat non-streaming convenience.
func (c *Client) Chat(ctx context.Context, req ChatRequest) (Message, error) {
	req.Stream = false
	body, err := json.Marshal(req)
	if err != nil {
		return Message{}, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/chat/completions", bytes.NewReader(body))
	if err != nil {
		return Message{}, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.APIKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	// finite timeout for non-stream
	cli := *c.HTTPClient
	cli.Timeout = 10 * time.Minute
	resp, err := cli.Do(httpReq)
	if err != nil {
		return Message{}, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode >= 300 {
		return Message{}, fmt.Errorf("API %s: %s", resp.Status, strings.TrimSpace(string(b)))
	}
	var full ChatResponse
	if err := json.Unmarshal(b, &full); err != nil {
		return Message{}, err
	}
	if full.Error != nil {
		return Message{}, fmt.Errorf("API error: %s", full.Error.Message)
	}
	if len(full.Choices) == 0 {
		return Message{}, fmt.Errorf("empty choices")
	}
	msg := full.Choices[0].Message
	canonicalizeToolCallTypes(msg.ToolCalls)
	msg.Usage = full.Usage
	msg.FinishReason = full.Choices[0].FinishReason
	return msg, nil
}

// modelsListResponse is OpenAI-compatible GET /v1/models body.
type modelsListResponse struct {
	Data []struct {
		ID string `json:"id"`
	} `json:"data"`
	// Ollama native /api/tags shape (fallback if someone points at non-/v1).
	Models []struct {
		Name string `json:"name"`
	} `json:"models"`
	Error *struct {
		Message string `json:"message"`
	} `json:"error,omitempty"`
}

// ListModels returns model ids from GET {base}/models (Ollama / OpenAI-compatible).
func (c *Client) ListModels(ctx context.Context) ([]string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return nil, err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	cli := &http.Client{Timeout: 8 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		return nil, fmt.Errorf("list models at %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		return nil, fmt.Errorf("list models %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var parsed modelsListResponse
	if err := json.Unmarshal(body, &parsed); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	if parsed.Error != nil {
		return nil, fmt.Errorf("API error: %s", parsed.Error.Message)
	}
	out := make([]string, 0, len(parsed.Data)+len(parsed.Models))
	seen := map[string]struct{}{}
	for _, d := range parsed.Data {
		id := strings.TrimSpace(d.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	for _, m := range parsed.Models {
		id := strings.TrimSpace(m.Name)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

// Ping checks the server is reachable (Ollama tags or models list).
func (c *Client) Ping(ctx context.Context) error {
	// Try OpenAI-compatible /models
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/models", nil)
	if err != nil {
		return err
	}
	if c.APIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.APIKey)
	}
	cli := &http.Client{Timeout: 5 * time.Second}
	resp, err := cli.Do(req)
	if err != nil {
		// Ollama root without /v1
		return fmt.Errorf("cannot reach %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 500 {
		return fmt.Errorf("server error: %s", resp.Status)
	}
	return nil
}
