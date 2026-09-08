package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type FailureCategory string

const (
	FailureSuccess          FailureCategory = "success"
	FailureAuthentication   FailureCategory = "authentication_required"
	FailurePermissionDenied FailureCategory = "permission_denied"
	FailureNotFound         FailureCategory = "resource_not_found"
	FailureTimeout          FailureCategory = "timeout"
	FailureNetwork          FailureCategory = "network_error"
	FailureInvalidInput     FailureCategory = "invalid_input"
	FailureUnsupported      FailureCategory = "unsupported"
	FailureCommand          FailureCategory = "command_failed"
	FailureUnknown          FailureCategory = "unknown"
)

type ExecutionAttempt struct {
	Operation  string
	Category   FailureCategory
	HTTPStatus int
}

// ExecutionResult preserves the existing human-readable output while adding
// enough machine-readable context for the agent to choose a safe fallback.
type ExecutionResult struct {
	Output                 string
	Category               FailureCategory
	Retryable              bool
	AuthenticationRequired bool
	PermissionDenied       bool
	ResourceNotFound       bool
	Timeout                bool
	NetworkError           bool
	HTTPStatus             int
	ExitCode               int
	Stderr                 string
	Attempts               []ExecutionAttempt
}

type DetailedRunner interface {
	RunDetailed(ctx context.Context, argsJSON string) ExecutionResult
}

type ExecutionCandidate struct {
	Key       string
	Operation string
	Run       func(context.Context) ExecutionResult
}

// ExecuteCandidates is a bounded, read-only policy primitive. Candidate keys
// prevent equivalent fallback operations from being repeated.
func ExecuteCandidates(ctx context.Context, candidates []ExecutionCandidate) ExecutionResult {
	seen := map[string]bool{}
	var last ExecutionResult
	for _, candidate := range candidates {
		if candidate.Run == nil || seen[candidate.Key] {
			continue
		}
		seen[candidate.Key] = true
		last = candidate.Run(ctx)
		last.Attempts = append(last.Attempts, ExecutionAttempt{Operation: candidate.Operation, Category: last.Category, HTTPStatus: last.HTTPStatus})
		if last.Category == FailureSuccess {
			return last
		}
	}
	if last.Category == "" {
		return ExecutionResult{Category: FailureUnsupported, Output: "no executable fallback candidate"}
	}
	return last
}

func (r *Registry) RunDetailed(ctx context.Context, name, argsJSON string) ExecutionResult {
	t, ok := r.runners[name]
	if !ok {
		return ExecutionResult{Output: fmt.Sprintf("unknown tool %q", name), Category: FailureUnsupported}
	}
	if detailed, ok := t.(DetailedRunner); ok {
		return detailed.RunDetailed(ctx, argsJSON)
	}
	out, err := t.Run(ctx, argsJSON)
	return resultFromOutput(out, err)
}

func resultFromOutput(out string, err error) ExecutionResult {
	r := ExecutionResult{Output: out, Category: FailureSuccess}
	if err != nil {
		r.Category = classifyFailure(err.Error())
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(strings.ToLower(err.Error()), "timeout") {
			r.Retryable, r.Timeout = true, true
		}
		return r
	}
	line := strings.TrimSpace(out)
	if strings.HasPrefix(line, "HTTP ") {
		fields := strings.Fields(strings.SplitN(line, "\n", 2)[0])
		if len(fields) >= 2 {
			if code, parseErr := strconv.Atoi(fields[1]); parseErr == nil {
				r.HTTPStatus = code
				switch {
				case code == 401:
					r.Category, r.AuthenticationRequired = FailureAuthentication, true
				case code == 403:
					r.Category, r.PermissionDenied = FailurePermissionDenied, true
				case code == 404:
					r.Category, r.ResourceNotFound = FailureNotFound, true
				case code >= 400:
					r.Category, r.Retryable = FailureUnknown, code >= 500
				}
			}
		}
	}
	if strings.HasPrefix(strings.ToLower(line), "error:") {
		r.Category = classifyFailure(line)
	}
	return r
}

func classifyFailure(text string) FailureCategory {
	low := strings.ToLower(text)
	switch {
	case strings.Contains(low, "authentication"), strings.Contains(low, "not logged in"), strings.Contains(low, "login required"):
		return FailureAuthentication
	case strings.Contains(low, "permission denied"), strings.Contains(low, "forbidden"):
		return FailurePermissionDenied
	case strings.Contains(low, "timeout"), strings.Contains(low, "deadline exceeded"):
		return FailureTimeout
	case strings.Contains(low, "no such host"), strings.Contains(low, "connection refused"), strings.Contains(low, "network is unreachable"), strings.Contains(low, "temporary failure"):
		return FailureNetwork
	case strings.Contains(low, "required"), strings.Contains(low, "invalid url"), strings.Contains(low, "must start with"):
		return FailureInvalidInput
	case strings.Contains(low, "exit status"), strings.Contains(low, "command failed"):
		return FailureCommand
	default:
		return FailureUnknown
	}
}

func (r ExecutionResult) ModelOutput() string {
	if len(r.Attempts) == 0 && r.Category == FailureSuccess {
		return r.Output
	}
	var b strings.Builder
	b.WriteString(r.Output)
	b.WriteString("\n\n[tool execution]")
	fmt.Fprintf(&b, "\ncategory: %s\nretryable: %t", r.Category, r.Retryable)
	if r.HTTPStatus != 0 {
		fmt.Fprintf(&b, "\nhttp_status: %d", r.HTTPStatus)
	}
	if r.AuthenticationRequired {
		b.WriteString("\nauthentication_required: true")
	}
	if r.PermissionDenied {
		b.WriteString("\npermission_denied: true")
	}
	if r.ResourceNotFound {
		b.WriteString("\nresource_not_found: true")
	}
	for _, attempt := range r.Attempts {
		fmt.Fprintf(&b, "\nattempt: %s (%s)", attempt.Operation, attempt.Category)
	}
	return b.String()
}

type CommandCapabilities struct {
	GHInstalled      bool
	GHAuthenticated  bool
	CurlAvailable    bool
	GHTokenAvailable bool
}

type GitHubFallbackDeps struct {
	Capabilities func(context.Context) CommandCapabilities
	RunCommand   func(context.Context, []string) ExecutionResult
	FetchAPI     func(context.Context, Resource) ExecutionResult
}

func DiscoverCommandCapabilities(ctx context.Context) CommandCapabilities {
	c := CommandCapabilities{}
	if _, err := exec.LookPath("gh"); err == nil {
		c.GHInstalled = true
		check := exec.CommandContext(ctx, "gh", "auth", "status", "--hostname", "github.com")
		c.GHAuthenticated = check.Run() == nil
	}
	_, err := exec.LookPath("curl")
	c.CurlAvailable = err == nil
	c.GHTokenAvailable = strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) != "" || strings.TrimSpace(os.Getenv("GH_TOKEN")) != ""
	return c
}

func DefaultGitHubFallbackDeps() GitHubFallbackDeps {
	return GitHubFallbackDeps{
		Capabilities: DiscoverCommandCapabilities,
		RunCommand: func(ctx context.Context, args []string) ExecutionResult {
			if len(args) == 0 {
				return ExecutionResult{Category: FailureInvalidInput, Output: "command required"}
			}
			cmdCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
			defer cancel()
			cmd := exec.CommandContext(cmdCtx, args[0], args[1:]...)
			out, err := cmd.CombinedOutput()
			result := resultFromOutput(string(out), err)
			if cmdCtx.Err() != nil {
				result.Category, result.Retryable, result.Timeout = FailureTimeout, true, true
			}
			if err != nil {
				if exit, ok := err.(*exec.ExitError); ok {
					result.ExitCode = exit.ExitCode()
				}
			}
			return result
		},
		FetchAPI: fetchGitHubActionsAPI,
	}
}

// ResolveGitHubActionsFallback makes one bounded, read-only gh attempt. A
// token/API transport can be added later without changing the agent loop.
func ResolveGitHubActionsFallback(ctx context.Context, resource Resource, deps GitHubFallbackDeps) ExecutionResult {
	if deps.Capabilities == nil {
		deps.Capabilities = DiscoverCommandCapabilities
	}
	if deps.RunCommand == nil {
		deps.RunCommand = DefaultGitHubFallbackDeps().RunCommand
	}
	capabilities := deps.Capabilities(ctx)
	if deps.FetchAPI == nil {
		deps.FetchAPI = fetchGitHubActionsAPI
	}
	args := []string{"gh", "run", "view", resource.RunID, "--repo", resource.Owner + "/" + resource.Repository, "--log"}
	if resource.JobID != "" {
		args = []string{"gh", "run", "view", resource.RunID, "--repo", resource.Owner + "/" + resource.Repository, "--job", resource.JobID, "--log"}
	}
	candidates := []ExecutionCandidate{}
	if capabilities.GHInstalled && capabilities.GHAuthenticated {
		candidates = append(candidates, ExecutionCandidate{
			Key:       "gh-actions-logs:" + resource.Owner + "/" + resource.Repository + ":" + resource.RunID + ":" + resource.JobID,
			Operation: "gh actions logs",
			Run:       func(runCtx context.Context) ExecutionResult { return deps.RunCommand(runCtx, args) },
		})
	}
	if capabilities.GHTokenAvailable {
		candidates = append(candidates, ExecutionCandidate{
			Key:       "github-actions-api:" + resource.Owner + "/" + resource.Repository + ":" + resource.RunID + ":" + resource.JobID,
			Operation: "GitHub Actions API logs",
			Run:       func(runCtx context.Context) ExecutionResult { return deps.FetchAPI(runCtx, resource) },
		})
	}
	if len(candidates) == 0 {
		category := FailureUnsupported
		message := "GitHub Actions fallback unavailable: gh CLI is not installed"
		if capabilities.GHInstalled && !capabilities.GHAuthenticated {
			category, message = FailureAuthentication, "GitHub Actions fallback requires authenticated gh CLI or a GitHub API token"
		}
		return ExecutionResult{Category: category, AuthenticationRequired: category == FailureAuthentication, Output: message, Attempts: []ExecutionAttempt{{Operation: "GitHub Actions fallback", Category: category}}}
	}
	return ExecuteCandidates(ctx, candidates)
}

func fetchGitHubActionsAPI(ctx context.Context, resource Resource) ExecutionResult {
	return fetchGitHubActionsAPIWithClient(ctx, resource, nil, "https://api.github.com")
}

const (
	maxGitHubArchiveBytes   = 10 << 20
	maxGitHubExtractedBytes = 2 << 20
	maxGitHubLogFileBytes   = 512 << 10
	maxGitHubArchiveEntries = 128
)

func fetchGitHubActionsAPIWithClient(ctx context.Context, resource Resource, client *http.Client, apiBaseURL string) ExecutionResult {
	token := strings.TrimSpace(os.Getenv("GITHUB_TOKEN"))
	if token == "" {
		token = strings.TrimSpace(os.Getenv("GH_TOKEN"))
	}
	if token == "" {
		return ExecutionResult{Category: FailureAuthentication, AuthenticationRequired: true, Output: "GitHub Actions API requires GITHUB_TOKEN or GH_TOKEN"}
	}
	endpoint := fmt.Sprintf("%s/repos/%s/%s/actions/runs/%s/logs", strings.TrimRight(apiBaseURL, "/"), resource.Owner, resource.Repository, resource.RunID)
	if resource.JobID != "" {
		endpoint = fmt.Sprintf("%s/repos/%s/%s/actions/jobs/%s/logs", strings.TrimRight(apiBaseURL, "/"), resource.Owner, resource.Repository, resource.JobID)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return resultFromOutput("", err)
	}
	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("Authorization", "Bearer "+token)
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	response, err := client.Do(request)
	if err != nil {
		result := resultFromOutput("error: GitHub API request failed: "+err.Error(), nil)
		if ctx.Err() != nil {
			result.Category, result.Retryable, result.Timeout = FailureTimeout, true, true
		}
		return result
	}
	defer response.Body.Close()
	body, readErr := io.ReadAll(io.LimitReader(response.Body, maxGitHubArchiveBytes+1))
	if readErr != nil {
		return resultFromOutput("error: GitHub API response read failed: "+readErr.Error(), nil)
	}
	if len(body) > maxGitHubArchiveBytes {
		return ExecutionResult{Category: FailureUnknown, Output: "GitHub Actions log archive exceeds the 10 MiB limit"}
	}
	if response.StatusCode >= 300 {
		return resultFromOutput(fmt.Sprintf("HTTP %d %s\n\n%s", response.StatusCode, response.Status, string(body)), nil)
	}
	logs, extractErr := extractGitHubLogArchive(body)
	if extractErr != nil {
		return ExecutionResult{Category: FailureUnknown, Output: "GitHub Actions log archive could not be read: " + extractErr.Error()}
	}
	return ExecutionResult{Output: logs, Category: FailureSuccess}
}

func extractGitHubLogArchive(data []byte) (string, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", fmt.Errorf("invalid ZIP archive: %w", err)
	}
	if len(archive.File) == 0 {
		return "", errors.New("archive is empty")
	}

	var output strings.Builder
	extracted := 0
	usable := 0
	for index, entry := range archive.File {
		if index >= maxGitHubArchiveEntries {
			return "", fmt.Errorf("archive contains more than %d entries", maxGitHubArchiveEntries)
		}
		if !safeGitHubArchivePath(entry.Name) || entry.FileInfo().IsDir() || entry.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if entry.UncompressedSize64 > maxGitHubLogFileBytes || entry.UncompressedSize64 > uint64(maxGitHubExtractedBytes-extracted) {
			return "", fmt.Errorf("log entry %q exceeds extraction limits", entry.Name)
		}
		reader, openErr := entry.Open()
		if openErr != nil {
			return "", fmt.Errorf("open %q: %w", entry.Name, openErr)
		}
		content, readErr := io.ReadAll(io.LimitReader(reader, maxGitHubLogFileBytes+1))
		_ = reader.Close()
		if readErr != nil {
			return "", fmt.Errorf("read %q: %w", entry.Name, readErr)
		}
		if len(content) > maxGitHubLogFileBytes || len(content) > maxGitHubExtractedBytes-extracted {
			return "", fmt.Errorf("log entry %q exceeds extraction limits", entry.Name)
		}
		extracted += len(content)
		if len(content) == 0 || bytes.IndexByte(content, 0) >= 0 || !utf8.Valid(content) {
			continue
		}
		if usable > 0 {
			output.WriteString("\n\n")
		}
		fmt.Fprintf(&output, "--- %s ---\n%s", entry.Name, content)
		usable++
	}
	if usable == 0 {
		return "", errors.New("archive contains no usable text logs")
	}
	if output.Len() > maxGitHubExtractedBytes {
		return "", errors.New("extracted logs exceed the 2 MiB limit")
	}
	return "GitHub Actions logs:\n" + output.String(), nil
}

func safeGitHubArchivePath(name string) bool {
	if name == "" || strings.Contains(name, "\\") || path.IsAbs(name) {
		return false
	}
	clean := path.Clean(name)
	if clean == "." || strings.HasPrefix(clean, "../") || clean == ".." {
		return false
	}
	for _, part := range strings.Split(clean, "/") {
		if part == ".." {
			return false
		}
	}
	return clean == name
}
