package tools

import "testing"

func TestExecutionContextMatchesHostAndKubernetesContext(t *testing.T) {
	info := executionContext{hostname: "phoenix702339.example.com", user: "opc", kubectlContext: "phoenix702339"}
	for _, target := range []string{"phoenix702339", "phoenix702339.example.com"} {
		if !info.matches(target, "") {
			t.Fatalf("expected %q to match local execution context", target)
		}
	}
	if (executionContext{hostname: "other-host", kubectlContext: "phoenix702339"}).matches("phoenix702339", "") == false {
		t.Fatal("expected matching kubectl context to select local execution")
	}
	if info.matches("different-host", "") {
		t.Fatal("did not expect unrelated host to select local execution")
	}
}

func TestNormalizeHostAndAuthenticationFailure(t *testing.T) {
	if got := normalizeHost("opc@[PHOENIX702339.EXAMPLE.COM]"); got != "phoenix702339.example.com" {
		t.Fatalf("normalizeHost = %q", got)
	}
	if !isSSHAuthenticationFailure("Permission denied (publickey,password).") {
		t.Fatal("expected public-key/password failure to be recognized")
	}
	if isSSHAuthenticationFailure("Connection timed out") {
		t.Fatal("timeout is not an authentication failure")
	}
}

func TestRootContainerImageCommand(t *testing.T) {
	for _, tt := range []struct {
		in, want string
	}{
		{"docker images", "sudo -n docker images"},
		{"podman images -a", "sudo -n podman images -a"},
		{"sudo docker images", "sudo docker images"},
		{"docker ps", "docker ps"},
	} {
		if got := rootContainerImageCommand(tt.in); got != tt.want {
			t.Fatalf("rootContainerImageCommand(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestInteractiveRootShellIsRejectedWithReadOnlyGuidance(t *testing.T) {
	for _, command := range []string{"sudo -i", "su -"} {
		if !isInteractiveRootShell(command) {
			t.Fatalf("expected %q to be recognized as interactive", command)
		}
	}
	if isInteractiveRootShell("podman images") {
		t.Fatal("read-only image command must remain executable")
	}
}

func TestBundledInteractiveImageCommandBecomesDirectRootCommand(t *testing.T) {
	for _, tt := range []struct{ in, want string }{
		{"sudo -i; docker images", "sudo -n docker images"},
		{"sudo su -; podman images -a", "sudo -n podman images -a"},
	} {
		got, ok := normalizeInteractiveRootCommand(tt.in)
		if !ok || got != tt.want {
			t.Fatalf("normalizeInteractiveRootCommand(%q) = %q, %v; want %q, true", tt.in, got, ok, tt.want)
		}
	}
}

func TestLocalReadOnlyKubernetesCommand(t *testing.T) {
	if !localReadOnlyKubernetesCommand("KUBECONFIG=/home/user/.kube/config-cluster-a kubectl get pods -A") {
		t.Fatal("explicit-kubeconfig read-only kubectl should use the local tunnel")
	}
	if localReadOnlyKubernetesCommand("KUBECONFIG=/home/user/.kube/config-cluster-a kubectl delete pod sample") {
		t.Fatal("mutating kubectl command must not use the local read-only path")
	}
	if localReadOnlyKubernetesCommand("kubectl get pods") {
		t.Fatal("implicit kubeconfig kubectl must not bypass SSH target selection")
	}
}
