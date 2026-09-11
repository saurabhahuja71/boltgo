package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"
)

const LocalCodingReproducibleProfile = "local-coding-reproducible"

type ProviderState string

const (
	ProviderConnected    ProviderState = "connected"
	ProviderReconnecting ProviderState = "reconnecting"
	ProviderUnavailable  ProviderState = "unavailable"
	ProviderResumed      ProviderState = "resumed"
)

type ProviderErrorClass string

const (
	ProviderConnectionRefused ProviderErrorClass = "connection_refused"
	ProviderTimeout           ProviderErrorClass = "timeout"
	ProviderReset             ProviderErrorClass = "connection_reset"
	ProviderAuthentication    ProviderErrorClass = "authentication"
	ProviderConfiguration     ProviderErrorClass = "configuration"
	ProviderModelUnavailable  ProviderErrorClass = "model_unavailable"
	ProviderUnavailableClass  ProviderErrorClass = "unavailable"
)

// ProviderError is safe to show to users: it contains no request headers or
// credentials, only a category and bounded provider detail.
type ProviderError struct {
	Class  ProviderErrorClass
	Detail string
}

func (e *ProviderError) Error() string {
	if e.Detail == "" {
		return string(e.Class)
	}
	return string(e.Class) + ": " + e.Detail
}

type ProviderReadiness struct {
	State          ProviderState
	Model          string
	ModelAvailable bool
	Models         []string
}

func classifyProviderError(err error) ProviderErrorClass {
	if err == nil {
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") {
		return ProviderTimeout
	}
	if errors.Is(err, syscall.ECONNREFUSED) || strings.Contains(strings.ToLower(err.Error()), "connection refused") {
		return ProviderConnectionRefused
	}
	if errors.Is(err, syscall.ECONNRESET) || strings.Contains(strings.ToLower(err.Error()), "connection reset") || strings.Contains(strings.ToLower(err.Error()), "unexpected eof") {
		return ProviderReset
	}
	low := strings.ToLower(err.Error())
	if strings.Contains(low, "401") || strings.Contains(low, "403") || strings.Contains(low, "unauthorized") || strings.Contains(low, "forbidden") {
		return ProviderAuthentication
	}
	if strings.Contains(low, "400") || strings.Contains(low, "invalid") || strings.Contains(low, "bad request") {
		return ProviderConfiguration
	}
	return ProviderUnavailableClass
}

func wrapProviderError(err error) error {
	if err == nil {
		return nil
	}
	if _, ok := err.(*ProviderError); ok {
		return err
	}
	return &ProviderError{Class: classifyProviderError(err), Detail: compactProviderDetail(err.Error())}
}

func compactProviderDetail(detail string) string {
	detail = strings.Join(strings.Fields(detail), " ")
	if len(detail) > 240 {
		return detail[:240] + "…"
	}
	return detail
}

func ProviderErrorClassOf(err error) ProviderErrorClass {
	var pe *ProviderError
	if errors.As(err, &pe) {
		return pe.Class
	}
	return classifyProviderError(err)
}

func (c *Client) DailyReadiness(ctx context.Context, model string) (ProviderReadiness, error) {
	models, err := c.ListModels(ctx)
	if err != nil {
		return ProviderReadiness{State: ProviderUnavailable, Model: model}, wrapProviderError(err)
	}
	available := false
	for _, candidate := range models {
		if candidate == model {
			available = true
			break
		}
	}
	if len(models) > 0 && !available {
		return ProviderReadiness{State: ProviderUnavailable, Model: model, Models: models}, &ProviderError{Class: ProviderModelUnavailable, Detail: "configured model is not listed by the provider"}
	}
	return ProviderReadiness{State: ProviderConnected, Model: model, ModelAvailable: available, Models: models}, nil
}

// SamplingOptions is deliberately opt-in. A nil pointer on ChatRequest keeps
// the historical provider payload unchanged.
type SamplingOptions struct {
	Profile          string   `json:"-"`
	Temperature      *float64 `json:"temperature,omitempty"`
	TopP             *float64 `json:"top_p,omitempty"`
	TopK             *int     `json:"top_k,omitempty"`
	MinP             *float64 `json:"min_p,omitempty"`
	Seed             *int     `json:"seed,omitempty"`
	RepeatPenalty    *float64 `json:"repeat_penalty,omitempty"`
	PresencePenalty  *float64 `json:"presence_penalty,omitempty"`
	FrequencyPenalty *float64 `json:"frequency_penalty,omitempty"`
}

func LocalCodingReproducibleOptions() SamplingOptions {
	temperature, topP, minP := 0.0, 1.0, 0.0
	topK, seed := 1, 17
	repeat, presence, frequency := 1.05, 0.0, 0.0
	return SamplingOptions{
		Profile: LocalCodingReproducibleProfile, Temperature: &temperature,
		TopP: &topP, TopK: &topK, MinP: &minP, Seed: &seed,
		RepeatPenalty: &repeat, PresencePenalty: &presence, FrequencyPenalty: &frequency,
	}
}

func ResolveSamplingProfile(name string) (SamplingOptions, error) {
	switch strings.TrimSpace(name) {
	case "":
		return SamplingOptions{}, nil
	case LocalCodingReproducibleProfile:
		return LocalCodingReproducibleOptions(), nil
	default:
		return SamplingOptions{}, fmt.Errorf("unknown inference profile %q", name)
	}
}

// ValidateSamplingProfile fails closed unless the endpoint identifies as
// Ollama. The profile uses Ollama sampling controls that are not part of the
// generic OpenAI contract, so it must never silently degrade on another API.
func ValidateSamplingProfile(ctx context.Context, baseURL, model, profile string) error {
	if strings.TrimSpace(profile) == "" {
		return nil
	}
	if profile != LocalCodingReproducibleProfile {
		return fmt.Errorf("cannot validate unsupported inference profile %q", profile)
	}
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil || u.Scheme == "" || u.Host == "" {
		return fmt.Errorf("inference profile %q requires a valid Ollama base URL: %s", profile, baseURL)
	}
	u.Path = strings.TrimSuffix(strings.TrimSuffix(u.Path, "/"), "/v1") + "/api/version"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return fmt.Errorf("inference profile %q provider check: %w", profile, err)
	}
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("inference profile %q requires Ollama at %s: %w", profile, u.String(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("inference profile %q requires Ollama /api/version; provider returned %s", profile, resp.Status)
	}
	return nil
}

func (s SamplingOptions) MarshalJSON() ([]byte, error) {
	type plain SamplingOptions
	return json.Marshal(plain(s))
}
