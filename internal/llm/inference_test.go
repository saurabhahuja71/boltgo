package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestSamplingProfileOverlaysExplicitControls(t *testing.T) {
	opts := LocalCodingReproducibleOptions()
	b, err := json.Marshal(ChatRequest{Model: "qwen3-coder:latest", Temperature: 0.7, MaxTokens: 2048, Sampling: &opts})
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, want := range []string{`"temperature":0`, `"top_p":1`, `"top_k":1`, `"min_p":0`, `"seed":17`, `"repeat_penalty":1.05`, `"presence_penalty":0`, `"frequency_penalty":0`, `"max_tokens":2048`} {
		if !strings.Contains(text, want) {
			t.Fatalf("profile request missing %s: %s", want, text)
		}
	}
}

func TestGenericRequestDoesNotGainSamplingControls(t *testing.T) {
	b, err := json.Marshal(ChatRequest{Model: "test", Temperature: 0.7, MaxTokens: 2048})
	if err != nil {
		t.Fatal(err)
	}
	text := string(b)
	for _, field := range []string{"top_p", "top_k", "min_p", "seed", "repeat_penalty", "presence_penalty", "frequency_penalty"} {
		if strings.Contains(text, `"`+field+`"`) {
			t.Fatalf("generic request unexpectedly contains %s: %s", field, text)
		}
	}
}

func TestSamplingProfileFailsClosedForNonOllamaEndpoint(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer server.Close()
	if err := ValidateSamplingProfile(context.Background(), server.URL+"/v1", "model", LocalCodingReproducibleProfile); err == nil {
		t.Fatal("profile validation unexpectedly accepted a non-Ollama endpoint")
	}
}
