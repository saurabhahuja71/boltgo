package tools

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/user"
	"strings"
	"time"
)

type sshExecute struct{}

func (sshExecute) Name() string { return "ssh_execute" }
func (sshExecute) Description() string {
	return "Execute a literal remote command on an SSH config alias (for example podman8 or podman9). Use this tool directly for SSH host commands; do not wrap remote SSH commands in run_shell. Docker/Podman image-list commands are run with non-interactive sudo so the result is from root storage. Read-only kubectl commands with an explicit KUBECONFIG use the current local shell and tunnel rather than treating a Kubernetes cluster name as an SSH hostname. Before using this, prefer run_shell for kubectl, watch, logs, and other commands that should run in the current shell; ssh_execute checks the current hostname and kubectl context and runs locally when the target is already local."
}
func (sshExecute) Schema() map[string]any {
	return map[string]any{"type": "object", "required": []string{"host", "command"}, "additionalProperties": false, "properties": map[string]any{
		"host":    map[string]any{"type": "string", "description": "SSH config alias"},
		"command": map[string]any{"type": "string", "description": "Literal remote command"},
		"timeout": map[string]any{"type": "number", "default": 30},
	}}
}
func (sshExecute) Run(ctx context.Context, argsJSON string) (string, error) {
	var in struct {
		Host    string  `json:"host"`
		Command string  `json:"command"`
		Timeout float64 `json:"timeout"`
	}
	if err := json.Unmarshal([]byte(argsJSON), &in); err != nil {
		return "", err
	}
	if strings.TrimSpace(in.Host) == "" || strings.TrimSpace(in.Command) == "" {
		return "", fmt.Errorf("host and command required")
	}
	if in.Timeout <= 0 {
		in.Timeout = 30
	}
	in.Command = rootContainerImageCommand(in.Command)
	ctx, cancel := context.WithTimeout(ctx, time.Duration(in.Timeout*float64(time.Second)))
	defer cancel()
	config := os.Getenv("SSH_CONFIG_PATH")
	if config == "" {
		if home, err := os.UserHomeDir(); err == nil {
			candidate := home + "/.ssh/config"
			if _, err := os.Stat(candidate); err == nil {
				config = candidate
			}
		}
	}
	info := currentExecutionContext(ctx)
	if localReadOnlyKubernetesCommand(in.Command) || info.matches(in.Host, config) {
		return runLocalCommand(ctx, in.Command, in.Host, info), nil
	}
	args := []string{"-T", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"}
	if config != "" {
		args = append(args, "-F", config)
	}
	args = append(args, in.Host, in.Command)
	out, err := exec.CommandContext(ctx, "ssh", args...).CombinedOutput()
	if ctx.Err() != nil {
		return string(out), fmt.Errorf("ssh timed out after %.0fs", in.Timeout)
	}
	if err != nil {
		if isSSHAuthenticationFailure(string(out)) {
			return string(out), fmt.Errorf("SSH authentication failed for %q; no retry was attempted. If you are already on the target system, run this locally:\n\n%s\n\nCurrent local context: hostname=%s user=%s kubectl-context=%s",
				in.Host, in.Command, info.hostname, info.user, info.kubectlContext)
		}
		return string(out), fmt.Errorf("ssh failed: %w", err)
	}
	return string(out), nil
}

func localReadOnlyKubernetesCommand(command string) bool {
	low := strings.ToLower(strings.TrimSpace(command))
	if !strings.Contains(low, "kubectl") || !strings.Contains(low, "kubeconfig=") {
		return false
	}
	for _, forbidden := range []string{" apply ", " delete ", " patch ", " replace ", " edit ", " scale ", " rollout ", " label ", " taint "} {
		if strings.Contains(" "+low+" ", forbidden) {
			return false
		}
	}
	return true
}

// rootContainerImageCommand makes the common image-inventory request
// deterministic on hosts where the SSH account is an unprivileged user and
// Podman/Docker is configured with root-owned storage. It leaves all other
// commands untouched.
func rootContainerImageCommand(command string) string {
	trimmed := strings.TrimSpace(command)
	fields := strings.Fields(trimmed)
	if len(fields) < 2 || (fields[0] != "docker" && fields[0] != "podman") || fields[1] != "images" {
		return command
	}
	if fields[0] == "sudo" || strings.HasPrefix(trimmed, "sudo ") {
		return command
	}
	return "sudo -n " + trimmed
}

type executionContext struct {
	hostname       string
	user           string
	kubectlContext string
}

func currentExecutionContext(ctx context.Context) executionContext {
	info := executionContext{}
	info.hostname, _ = os.Hostname()
	info.hostname = strings.TrimSpace(info.hostname)
	if current, err := user.Current(); err == nil {
		info.user = current.Username
	}
	if out, err := exec.CommandContext(ctx, "whoami").Output(); err == nil {
		info.user = strings.TrimSpace(string(out))
	}
	probeCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(probeCtx, "kubectl", "config", "current-context").Output(); err == nil {
		info.kubectlContext = strings.TrimSpace(string(out))
	}
	return info
}

func (e executionContext) matches(target, config string) bool {
	target = normalizeHost(target)
	if target == "" {
		return false
	}
	for _, candidate := range []string{e.hostname, shortHost(e.hostname), e.kubectlContext} {
		if hostsMatch(target, candidate) {
			return true
		}
	}
	if resolved := sshConfiguredHost(target, config); resolved != "" {
		return hostsMatch(resolved, e.hostname) || hostsMatch(resolved, shortHost(e.hostname))
	}
	return false
}

func sshConfiguredHost(target, config string) string {
	args := []string{"-G"}
	if config != "" {
		args = append(args, "-F", config)
	}
	args = append(args, target)
	out, err := exec.Command("ssh", args...).Output()
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 2 && fields[0] == "hostname" {
			return fields[1]
		}
	}
	return ""
}

func runLocalCommand(ctx context.Context, command, target string, info executionContext) string {
	out, err := exec.CommandContext(ctx, "bash", "-lc", command).CombinedOutput()
	if err != nil {
		return fmt.Sprintf("%s\n[local execution for %q failed: %v]\n[local context: hostname=%s user=%s kubectl-context=%s]", out, target, err, info.hostname, info.user, info.kubectlContext)
	}
	return string(out)
}

func normalizeHost(host string) string {
	host = strings.TrimSpace(strings.ToLower(host))
	if at := strings.LastIndex(host, "@"); at >= 0 {
		host = host[at+1:]
	}
	host = strings.TrimPrefix(host, "[")
	host = strings.TrimSuffix(host, "]")
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	return strings.TrimSuffix(host, ".")
}

func shortHost(host string) string {
	host = normalizeHost(host)
	if dot := strings.IndexByte(host, '.'); dot >= 0 {
		return host[:dot]
	}
	return host
}

func hostsMatch(a, b string) bool {
	a, b = normalizeHost(a), normalizeHost(b)
	if a == "" || b == "" {
		return false
	}
	if ipA, ipB := net.ParseIP(a), net.ParseIP(b); ipA != nil && ipB != nil {
		return ipA.Equal(ipB)
	}
	return a == b || shortHost(a) == shortHost(b)
}

func isSSHAuthenticationFailure(output string) bool {
	low := strings.ToLower(output)
	return strings.Contains(low, "permission denied (publickey,password)") ||
		strings.Contains(low, "permission denied (publickey") ||
		strings.Contains(low, "authentication failed")
}
