package tools

import (
	"net/url"
	"strings"
)

type ResourceType string

const (
	ResourceHTTP             ResourceType = "http"
	ResourceGitHubRepository ResourceType = "github_repository"
	ResourceGitHubIssue      ResourceType = "github_issue"
	ResourceGitHubPull       ResourceType = "github_pull_request"
	ResourceGitHubActionsRun ResourceType = "github_actions_run"
	ResourceGitHubActionsJob ResourceType = "github_actions_job"
)

type Resource struct {
	Type       ResourceType
	URL        string
	Owner      string
	Repository string
	RunID      string
	JobID      string
}

func ClassifyResourceURL(raw string) Resource {
	r := Resource{Type: ResourceHTTP, URL: strings.TrimSpace(raw)}
	u, err := url.Parse(r.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || !strings.EqualFold(u.Hostname(), "github.com") {
		return r
	}
	parts := strings.Split(strings.Trim(u.Path, "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		return r
	}
	r.Owner, r.Repository = parts[0], strings.TrimSuffix(parts[1], ".git")
	r.Type = ResourceGitHubRepository
	if len(parts) >= 4 && parts[2] == "issues" {
		r.Type = ResourceGitHubIssue
		return r
	}
	if len(parts) >= 4 && (parts[2] == "pull" || parts[2] == "pulls") {
		r.Type = ResourceGitHubPull
		return r
	}
	if len(parts) >= 4 && parts[2] == "actions" && parts[3] == "runs" && len(parts) >= 5 && parts[4] != "" {
		r.RunID = parts[4]
		r.Type = ResourceGitHubActionsRun
		if len(parts) >= 7 && parts[5] == "job" && parts[6] != "" {
			r.JobID = parts[6]
			r.Type = ResourceGitHubActionsJob
		}
	}
	return r
}
