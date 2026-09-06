package upgrade

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

const DefaultRepository = "saurabhahuja71/boltgo"

var checksumPattern = regexp.MustCompile(`(?i)\b([a-f0-9]{64})\b`)

type Asset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type Release struct {
	TagName string  `json:"tag_name"`
	Assets  []Asset `json:"assets"`
}

type Result struct {
	CurrentVersion string
	LatestVersion  string
	AssetName      string
	Updated        bool
}

type Client struct {
	HTTPClient *http.Client
	APIBaseURL string
	Repository string
}

func (c Client) Latest(ctx context.Context) (Release, error) {
	base := strings.TrimRight(c.APIBaseURL, "/")
	if base == "" {
		base = "https://api.github.com"
	}
	repo := c.Repository
	if repo == "" {
		repo = DefaultRepository
	}
	url := fmt.Sprintf("%s/repos/%s/releases/latest", base, repo)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return Release{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "bolt-upgrade")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return Release{}, fmt.Errorf("fetch latest release: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return Release{}, fmt.Errorf("fetch latest release: HTTP %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var release Release
	if err := json.NewDecoder(resp.Body).Decode(&release); err != nil {
		return Release{}, fmt.Errorf("decode latest release: %w", err)
	}
	if strings.TrimSpace(release.TagName) == "" {
		return Release{}, errors.New("latest release has no tag")
	}
	return release, nil
}

func (c Client) Upgrade(ctx context.Context, executable, currentVersion string, output io.Writer) (Result, error) {
	if output == nil {
		output = io.Discard
	}
	release, err := c.Latest(ctx)
	if err != nil {
		return Result{CurrentVersion: currentVersion}, err
	}
	result := Result{CurrentVersion: currentVersion, LatestVersion: release.TagName}
	if !Newer(currentVersion, release.TagName) {
		return result, nil
	}

	osName, arch := runtime.GOOS, runtime.GOARCH
	asset, checksum, err := selectAssets(release.Assets, osName, arch)
	if err != nil {
		return result, err
	}
	result.AssetName = asset.Name
	fmt.Fprintf(output, "Bolt upgrade: %s -> %s (%s)\n", currentVersion, release.TagName, asset.Name)

	binary, err := c.download(ctx, asset.BrowserDownloadURL, 128<<20)
	if err != nil {
		return result, fmt.Errorf("download %s: %w", asset.Name, err)
	}
	if checksum != nil {
		checksumText, err := c.download(ctx, checksum.BrowserDownloadURL, 16<<10)
		if err != nil {
			return result, fmt.Errorf("download checksum %s: %w", checksum.Name, err)
		}
		want := checksumPattern.FindStringSubmatch(string(checksumText))
		if len(want) != 2 {
			return result, fmt.Errorf("checksum %s contains no SHA-256 digest", checksum.Name)
		}
		have := sha256.Sum256(binary)
		if !strings.EqualFold(hex.EncodeToString(have[:]), want[1]) {
			return result, fmt.Errorf("checksum mismatch for %s", asset.Name)
		}
	}
	if err := atomicReplace(executable, binary); err != nil {
		return result, fmt.Errorf("replace executable: %w", err)
	}
	result.Updated = true
	fmt.Fprintf(output, "Bolt upgrade: installed %s\n", release.TagName)
	return result, nil
}

func (c Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c Client) download(ctx context.Context, url string, limit int64) ([]byte, error) {
	if strings.TrimSpace(url) == "" {
		return nil, errors.New("release asset has no download URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "bolt-upgrade")
	resp, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %s", resp.Status)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(data)) > limit {
		return nil, errors.New("release asset is too large")
	}
	return data, nil
}

func selectAssets(assets []Asset, osName, arch string) (Asset, *Asset, error) {
	ext := ""
	if osName == "windows" {
		ext = ".exe"
	}
	want := []string{
		fmt.Sprintf("bolt-%s-%s%s", osName, arch, ext),
		fmt.Sprintf("agenterm-%s-%s%s", osName, arch, ext),
	}
	var binary Asset
	for _, name := range want {
		for _, asset := range assets {
			if asset.Name == name {
				binary = asset
				break
			}
		}
		if binary.Name != "" {
			break
		}
	}
	if binary.Name == "" {
		return Asset{}, nil, fmt.Errorf("release has no binary for %s/%s", osName, arch)
	}
	for _, asset := range assets {
		if asset.Name == binary.Name+".sha256" {
			copy := asset
			return binary, &copy, nil
		}
	}
	return binary, nil, nil
}

func atomicReplace(path string, data []byte) error {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("target is not a regular file: %s", resolved)
	}
	dir := filepath.Dir(resolved)
	tmp, err := os.CreateTemp(dir, ".bolt-upgrade-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err := tmp.Chmod(info.Mode().Perm()); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, resolved)
}

func Newer(current, latest string) bool {
	current = strings.TrimPrefix(strings.TrimSpace(current), "v")
	latest = strings.TrimPrefix(strings.TrimSpace(latest), "v")
	if current == "" || current == "dev" || current == "development" {
		return latest != ""
	}
	if current == latest {
		return false
	}
	curParts, curOK := versionParts(current)
	latestParts, latestOK := versionParts(latest)
	if curOK && latestOK {
		for i := 0; i < len(curParts) && i < len(latestParts); i++ {
			if latestParts[i] != curParts[i] {
				return latestParts[i] > curParts[i]
			}
		}
		return len(latestParts) > len(curParts)
	}
	return true
}

func versionParts(version string) ([]int, bool) {
	parts := strings.SplitN(version, "-", 2)[0]
	fields := strings.Split(parts, ".")
	if len(fields) < 2 || len(fields) > 3 {
		return nil, false
	}
	out := make([]int, len(fields))
	for i, field := range fields {
		if field == "" {
			return nil, false
		}
		value := 0
		for _, r := range field {
			if r < '0' || r > '9' {
				return nil, false
			}
			value = value*10 + int(r-'0')
		}
		out[i] = value
	}
	return out, true
}
