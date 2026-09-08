package tools

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func TestClassifyExecutionFailures(t *testing.T) {
	tests := []struct {
		output   string
		category FailureCategory
		status   int
	}{
		{"HTTP 404 Not Found\n", FailureNotFound, 404},
		{"HTTP 401 Unauthorized\n", FailureAuthentication, 401},
		{"HTTP 403 Forbidden\n", FailurePermissionDenied, 403},
		{"error: fetch failed: context deadline exceeded", FailureTimeout, 0},
		{"error: fetch failed: dial tcp: no such host", FailureNetwork, 0},
		{"exit status 1", FailureCommand, 0},
	}
	for _, tc := range tests {
		got := resultFromOutput(tc.output, nil)
		if tc.category == FailureCommand {
			got = resultFromOutput(tc.output, errors.New(tc.output))
		}
		if got.Category != tc.category || got.HTTPStatus != tc.status {
			t.Errorf("%q = %+v, want %s/%d", tc.output, got, tc.category, tc.status)
		}
	}
}

func TestExecuteCandidatesBoundsAndDeduplicates(t *testing.T) {
	calls := 0
	got := ExecuteCandidates(context.Background(), []ExecutionCandidate{
		{Key: "same", Operation: "first", Run: func(context.Context) ExecutionResult {
			calls++
			return ExecutionResult{Output: "404", Category: FailureNotFound}
		}},
		{Key: "same", Operation: "duplicate", Run: func(context.Context) ExecutionResult {
			calls++
			return ExecutionResult{Output: "bad", Category: FailureUnknown}
		}},
		{Key: "fallback", Operation: "fallback", Run: func(context.Context) ExecutionResult {
			calls++
			return ExecutionResult{Output: "ok", Category: FailureSuccess}
		}},
		{Key: "never", Operation: "never", Run: func(context.Context) ExecutionResult { calls++; return ExecutionResult{Category: FailureUnknown} }},
	})
	if calls != 2 || got.Category != FailureSuccess || len(got.Attempts) != 1 {
		t.Fatalf("result=%+v calls=%d", got, calls)
	}
}

func TestGitHubActionsResourceClassification(t *testing.T) {
	tests := []struct {
		raw                   string
		typ                   ResourceType
		owner, repo, run, job string
	}{
		{"https://github.com/acme/widget/actions/runs/123", ResourceGitHubActionsRun, "acme", "widget", "123", ""},
		{"https://github.com/acme/widget/actions/runs/123/job/456", ResourceGitHubActionsJob, "acme", "widget", "123", "456"},
		{"https://github.com/acme/widget/issues/9", ResourceGitHubIssue, "acme", "widget", "", ""},
		{"https://github.com/acme/widget/pull/8", ResourceGitHubPull, "acme", "widget", "", ""},
		{"https://example.com/acme/widget/actions/runs/123/job/456", ResourceHTTP, "", "", "", ""},
	}
	for _, tc := range tests {
		got := ClassifyResourceURL(tc.raw)
		if got.Type != tc.typ || got.Owner != tc.owner || got.Repository != tc.repo || got.RunID != tc.run || got.JobID != tc.job {
			t.Errorf("%s = %+v", tc.raw, got)
		}
	}
}

func TestGitHubActionsFallbackUsesAuthenticatedGHWithoutLeakingCredentials(t *testing.T) {
	var gotArgs []string
	result := ResolveGitHubActionsFallback(context.Background(), ClassifyResourceURL("https://github.com/acme/widget/actions/runs/123/job/456"), GitHubFallbackDeps{
		Capabilities: func(context.Context) CommandCapabilities {
			return CommandCapabilities{GHInstalled: true, GHAuthenticated: true}
		},
		RunCommand: func(_ context.Context, args []string) ExecutionResult {
			gotArgs = args
			return ExecutionResult{Output: "log line", Category: FailureSuccess}
		},
	})
	if result.Category != FailureSuccess || result.Output != "log line" {
		t.Fatalf("result=%+v", result)
	}
	if len(gotArgs) == 0 || gotArgs[0] != "gh" || gotArgs[6] != "--job" || gotArgs[7] != "456" {
		t.Fatalf("args=%v", gotArgs)
	}
	for _, arg := range gotArgs {
		if arg == "GH_TOKEN" || arg == "GITHUB_TOKEN" || arg == "secret" {
			t.Fatalf("credential leaked in args: %v", gotArgs)
		}
	}
}

func TestGitHubActionsFallbackUsesAPIWhenGHIsUnavailable(t *testing.T) {
	apiCalled := false
	result := ResolveGitHubActionsFallback(context.Background(), ClassifyResourceURL("https://github.com/acme/widget/actions/runs/123/job/456"), GitHubFallbackDeps{
		Capabilities: func(context.Context) CommandCapabilities { return CommandCapabilities{GHTokenAvailable: true} },
		FetchAPI: func(_ context.Context, resource Resource) ExecutionResult {
			apiCalled = true
			if resource.JobID != "456" {
				t.Fatalf("resource=%+v", resource)
			}
			return ExecutionResult{Output: "api logs", Category: FailureSuccess}
		},
	})
	if !apiCalled || result.Category != FailureSuccess || result.Output != "api logs" {
		t.Fatalf("result=%+v apiCalled=%v", result, apiCalled)
	}
}

func TestGitHubActionsFallbackRequiresAuthentication(t *testing.T) {
	result := ResolveGitHubActionsFallback(context.Background(), ClassifyResourceURL("https://github.com/acme/widget/actions/runs/123"), GitHubFallbackDeps{
		Capabilities: func(context.Context) CommandCapabilities {
			return CommandCapabilities{GHInstalled: true, GHAuthenticated: false}
		},
	})
	if result.Category != FailureAuthentication || !result.AuthenticationRequired || len(result.Attempts) != 1 {
		t.Fatalf("result=%+v", result)
	}
}

func TestGitHubActionsAPIExtractsTextLogsForModel(t *testing.T) {
	archive := testLogArchive(t, map[string]struct {
		body string
		mode os.FileMode
	}{
		"job/build.log": {body: "compile: failed\nexit status 1\n"},
		"job/test.log":  {body: "tests skipped\n"},
	})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/acme/widget/actions/jobs/456/logs" {
			t.Fatalf("path=%s", r.URL.Path)
		}
		_, _ = w.Write(archive)
	}))
	defer server.Close()
	t.Setenv("GITHUB_TOKEN", "test-token")
	result := fetchGitHubActionsAPIWithClient(context.Background(), ClassifyResourceURL("https://github.com/acme/widget/actions/runs/123/job/456"), server.Client(), server.URL)
	if result.Category != FailureSuccess {
		t.Fatalf("result=%+v", result)
	}
	for _, want := range []string{"GitHub Actions logs:", "--- job/build.log ---", "compile: failed", "--- job/test.log ---", "tests skipped"} {
		if !strings.Contains(result.Output, want) {
			t.Fatalf("output missing %q: %s", want, result.Output)
		}
	}
	if strings.Contains(result.Output, "bytes)") {
		t.Fatalf("returned byte count instead of logs: %s", result.Output)
	}
}

func TestGitHubActionsAPIRejectsMalformedAndEmptyArchives(t *testing.T) {
	tests := []struct {
		name string
		data []byte
	}{
		{name: "malformed", data: []byte("not a zip")},
		{name: "empty", data: testLogArchive(t, nil)},
		{name: "binary only", data: testLogArchive(t, map[string]struct {
			body string
			mode os.FileMode
		}{"binary": {body: "a\x00b"}})},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(tc.data) }))
			defer server.Close()
			t.Setenv("GITHUB_TOKEN", "test-token")
			result := fetchGitHubActionsAPIWithClient(context.Background(), Resource{Owner: "acme", Repository: "widget", RunID: "123"}, server.Client(), server.URL)
			if result.Category == FailureSuccess {
				t.Fatalf("unexpected success: %+v", result)
			}
		})
	}
}

func TestGitHubActionsArchiveRejectsUnsafeAndOversizedEntries(t *testing.T) {
	tests := []struct {
		name  string
		files map[string]struct {
			body string
			mode os.FileMode
		}
	}{
		{name: "path traversal", files: map[string]struct {
			body string
			mode os.FileMode
		}{"../outside.log": {body: "secret"}}},
		{name: "absolute path", files: map[string]struct {
			body string
			mode os.FileMode
		}{"/outside.log": {body: "secret"}}},
		{name: "symlink", files: map[string]struct {
			body string
			mode os.FileMode
		}{"link.log": {body: "secret", mode: os.ModeSymlink}}},
		{name: "large file", files: map[string]struct {
			body string
			mode os.FileMode
		}{"large.log": {body: strings.Repeat("x", maxGitHubLogFileBytes+1)}}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := extractGitHubLogArchive(testLogArchive(t, tc.files)); err == nil {
				t.Fatal("expected unsafe or oversized archive to fail")
			}
		})
	}
}

func TestGitHubActionsArchiveRejectsTooManyEntries(t *testing.T) {
	files := make(map[string]struct {
		body string
		mode os.FileMode
	}, maxGitHubArchiveEntries+1)
	for i := 0; i <= maxGitHubArchiveEntries; i++ {
		files[fmt.Sprintf("log-%d.txt", i)] = struct {
			body string
			mode os.FileMode
		}{body: "log"}
	}
	if _, err := extractGitHubLogArchive(testLogArchive(t, files)); err == nil {
		t.Fatal("expected too many entries to fail")
	}
}

func TestGitHubActionsArchiveRejectsTotalExtractedSize(t *testing.T) {
	files := make(map[string]struct {
		body string
		mode os.FileMode
	}, 5)
	for i := 0; i < 5; i++ {
		files[fmt.Sprintf("log-%d.txt", i)] = struct {
			body string
			mode os.FileMode
		}{body: strings.Repeat("x", 500<<10)}
	}
	if _, err := extractGitHubLogArchive(testLogArchive(t, files)); err == nil {
		t.Fatal("expected total extracted size limit to fail")
	}
}

func TestGitHubActionsAPIRejectsOversizedArchive(t *testing.T) {
	data := bytes.Repeat([]byte("x"), maxGitHubArchiveBytes+1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(data) }))
	defer server.Close()
	t.Setenv("GITHUB_TOKEN", "test-token")
	result := fetchGitHubActionsAPIWithClient(context.Background(), Resource{Owner: "acme", Repository: "widget", RunID: "123"}, server.Client(), server.URL)
	if result.Category == FailureSuccess || !strings.Contains(result.Output, "10 MiB limit") {
		t.Fatalf("result=%+v", result)
	}
}

func testLogArchive(t *testing.T, files map[string]struct {
	body string
	mode os.FileMode
}) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, file := range files {
		header := &zip.FileHeader{Name: name, Method: zip.Deflate}
		if file.mode != 0 {
			header.SetMode(file.mode)
		}
		entry, err := writer.CreateHeader(header)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(entry, file.body); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}
