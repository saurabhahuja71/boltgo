package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestNewer(t *testing.T) {
	for _, test := range []struct {
		current string
		latest  string
		want    bool
	}{
		{"1.0.0", "v1.1.0", true},
		{"1.1.0", "v1.1.0", false},
		{"1.2.0", "v1.1.0", false},
		{"dev", "v1.1.0", true},
	} {
		if got := Newer(test.current, test.latest); got != test.want {
			t.Fatalf("Newer(%q, %q)=%v, want %v", test.current, test.latest, got, test.want)
		}
	}
}

func TestUpgradeVerifiesChecksumAndAtomicallyReplacesBinary(t *testing.T) {
	t.Parallel()
	old := []byte("old bolt binary")
	next := []byte("new bolt binary")
	digest := sha256.Sum256(next)
	serverURL := ""
	binaryName := fmt.Sprintf("bolt-%s-%s", runtime.GOOS, runtime.GOARCH)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/test/boltgo/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.1.0","assets":[{"name":"%s","browser_download_url":"%s/binary"},{"name":"%s.sha256","browser_download_url":"%s/checksum"}]}`, binaryName, serverURL, binaryName, serverURL)
		case "/binary":
			_, _ = w.Write(next)
		case "/checksum":
			fmt.Fprintf(w, "%s  bolt-linux-amd64\n", hex.EncodeToString(digest[:]))
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL = server.URL

	dir := t.TempDir()
	executable := filepath.Join(dir, "bolt")
	if err := os.WriteFile(executable, old, 0o755); err != nil {
		t.Fatal(err)
	}
	client := Client{APIBaseURL: server.URL, Repository: "test/boltgo"}
	var output strings.Builder
	result, err := client.Upgrade(context.Background(), executable, "1.0.0", &output)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || result.AssetName != binaryName {
		t.Fatalf("unexpected result: %+v", result)
	}
	got, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(next) || !strings.Contains(output.String(), "1.0.0 -> v1.1.0") {
		t.Fatalf("upgrade output/file mismatch: output=%q file=%q", output.String(), got)
	}
}

func TestUpgradeDoesNotReplaceWhenAlreadyCurrent(t *testing.T) {
	t.Parallel()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"tag_name":"v1.1.0","assets":[]}`)
	}))
	defer server.Close()
	dir := t.TempDir()
	executable := filepath.Join(dir, "bolt")
	if err := os.WriteFile(executable, []byte("keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := (Client{APIBaseURL: server.URL, Repository: "test/boltgo"}).Upgrade(context.Background(), executable, "1.1.0", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if result.Updated {
		t.Fatal("already-current binary was replaced")
	}
	got, _ := os.ReadFile(executable)
	if string(got) != "keep" {
		t.Fatalf("binary changed: %q", got)
	}
}

func TestUpgradeLeavesBinaryUnchangedOnChecksumFailure(t *testing.T) {
	t.Parallel()
	binaryName := fmt.Sprintf("bolt-%s-%s", runtime.GOOS, runtime.GOARCH)
	serverURL := ""
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/test/boltgo/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.1.0","assets":[{"name":"%s","browser_download_url":"%s/binary"},{"name":"%s.sha256","browser_download_url":"%s/checksum"}]}`, binaryName, serverURL, binaryName, serverURL)
		case "/binary":
			_, _ = w.Write([]byte("new"))
		case "/checksum":
			fmt.Fprintf(w, "%s  %s\n", strings.Repeat("0", 64), binaryName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer server.Close()
	serverURL = server.URL
	dir := t.TempDir()
	executable := filepath.Join(dir, "bolt")
	if err := os.WriteFile(executable, []byte("keep"), 0o755); err != nil {
		t.Fatal(err)
	}
	_, err := (Client{APIBaseURL: server.URL, Repository: "test/boltgo"}).Upgrade(context.Background(), executable, "1.0.0", io.Discard)
	if err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("expected checksum failure, got %v", err)
	}
	got, _ := os.ReadFile(executable)
	if string(got) != "keep" {
		t.Fatalf("binary changed after checksum failure: %q", got)
	}
}

func TestUpgradePrefersLowercaseProxyEnvironment(t *testing.T) {
	old := []byte("old")
	next := []byte("new through proxy")
	digest := sha256.Sum256(next)
	binaryName := fmt.Sprintf("bolt-%s-%s", runtime.GOOS, runtime.GOARCH)
	var proxyHits int
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxyHits++
		switch r.URL.Path {
		case "/repos/test/boltgo/releases/latest":
			fmt.Fprintf(w, `{"tag_name":"v1.1.0","assets":[{"name":"%s","browser_download_url":"http://release.test/binary"},{"name":"%s.sha256","browser_download_url":"http://release.test/checksum"}]}`, binaryName, binaryName)
		case "/binary":
			_, _ = w.Write(next)
		case "/checksum":
			fmt.Fprintf(w, "%s  %s\n", hex.EncodeToString(digest[:]), binaryName)
		default:
			http.NotFound(w, r)
		}
	}))
	defer proxy.Close()

	// Simulate a stale uppercase proxy and the explicitly exported lowercase
	// proxy used by the VM setup instructions.
	t.Setenv("HTTP_PROXY", "http://127.0.0.1:1")
	t.Setenv("HTTPS_PROXY", "http://127.0.0.1:1")
	t.Setenv("http_proxy", proxy.URL)
	t.Setenv("https_proxy", proxy.URL)
	t.Setenv("NO_PROXY", "")
	t.Setenv("no_proxy", "")

	dir := t.TempDir()
	executable := filepath.Join(dir, "bolt")
	if err := os.WriteFile(executable, old, 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := (Client{APIBaseURL: "http://release.test", Repository: "test/boltgo"}).Upgrade(context.Background(), executable, "1.0.0", io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Updated || proxyHits < 3 {
		t.Fatalf("proxy was not used for release and asset requests: result=%+v hits=%d", result, proxyHits)
	}
	got, err := os.ReadFile(executable)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(next) {
		t.Fatalf("proxy-upgraded binary=%q, want %q", got, next)
	}
}
