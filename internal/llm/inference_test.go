package llm

import (
	"io"
	"errors"
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

func TestClassifyProviderErrorTreatsBareEOFAsReset(t *testing.T) {
	cases := []error{
		io.EOF,
		errors.New(`Post "http://127.0.0.1:30002/v1/chat/completions": EOF`),
		errors.New("read tcp 127.0.0.1:1->127.0.0.1:30002: read: connection reset by peer"),
	}
	for _, err := range cases {
		if got := classifyProviderError(err); got != ProviderReset {
			t.Fatalf("classifyProviderError(%v)=%q, want %q", err, got, ProviderReset)
		}
		if !TransientProviderFailure(err) {
			t.Fatalf("TransientProviderFailure(%v)=false, want true", err)
		}
	}
}
