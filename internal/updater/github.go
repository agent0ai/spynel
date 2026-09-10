package updater

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

const defaultGitHubURL = "https://api.github.com/repos/agent0ai/spynel/releases/latest"
const defaultDownloadURL = "https://github.com/agent0ai/spynel/releases/download/v"
const maxArchiveBytes = 512 << 20
const maxChecksumBytes = 1 << 20

func stableVersion(version string) bool {
	parsed, ok := parseVersion(version)
	return ok && len(parsed.prerelease) == 0 && !strings.Contains(version, "+") && version == strings.TrimSpace(version)
}

func archiveName(version string) string {
	return fmt.Sprintf("spynel_%s_%s_%s.tar.gz", strings.TrimPrefix(version, "v"), runtime.GOOS, runtime.GOARCH)
}

// releaseClient retains injected transports but always bounds redirects and
// refuses TLS downgrades. HTTP is allowed only for explicit mirror overrides.
func (m *Manager) releaseClient() *http.Client {
	client := *http.DefaultClient
	if m.Client != nil {
		client = *m.Client
	}
	client.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("too many release redirects")
		}
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("release download refused an HTTPS-to-HTTP redirect")
		}
		return nil
	}
	return &client
}

func (m *Manager) download(ctx context.Context, source string, output io.Writer, limit int64) error {
	parsed, err := url.Parse(source)
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") || parsed.User != nil {
		return errors.New("invalid release URL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, source, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "spynel/"+m.CurrentVersion)
	response, err := m.releaseClient().Do(req)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("release server returned %s", response.Status)
	}
	if response.ContentLength > limit {
		return errors.New("release download exceeds byte limit")
	}
	n, err := io.Copy(output, io.LimitReader(response.Body, limit+1))
	if err != nil {
		return err
	}
	if n > limit {
		return errors.New("release download exceeds byte limit")
	}
	return nil
}

func (m *Manager) checkGitHub(ctx context.Context, result Result) (Result, error) {
	timeout := m.CheckTimeout
	if timeout <= 0 {
		timeout = DefaultCheckTimeout
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var data strings.Builder
	endpoint := m.GitHubURL
	if endpoint == "" {
		endpoint = defaultGitHubURL
	}
	if err := m.download(ctx, endpoint, &data, maxChecksumBytes); err != nil {
		return result, fmt.Errorf("GitHub update check: %w", err)
	}
	var release struct {
		Tag        string `json:"tag_name"`
		Draft      bool   `json:"draft"`
		Prerelease bool   `json:"prerelease"`
	}
	if err := json.Unmarshal([]byte(data.String()), &release); err != nil {
		return result, fmt.Errorf("decode GitHub release: %w", err)
	}
	if release.Draft || release.Prerelease || !strings.HasPrefix(release.Tag, "v") || !stableVersion(release.Tag) {
		return result, errors.New("GitHub did not return a stable semantic release")
	}
	result.Latest = strings.TrimPrefix(release.Tag, "v")
	result.Available = compareVersions(result.Latest, m.CurrentVersion) > 0
	return result, nil
}

// Install downloads and validates a complete GitHub bundle before atomically
// switching this installation. Existing processes keep their immutable bundle.
func (m *Manager) Install(ctx context.Context, version string) error {
	if m == nil || m.InstallRoot == "" {
		return errors.New("this installation is not managed by the release installer")
	}
	if !stableVersion(version) || compareVersions(version, m.CurrentVersion) <= 0 {
		return errors.New("update requires a newer stable version")
	}
	ctx, cancel := context.WithTimeout(ctx, 4*time.Minute)
	defer cancel()
	temp, err := os.MkdirTemp(m.InstallRoot, ".download-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(temp)
	base := strings.TrimRight(os.Getenv("SPYNEL_DOWNLOAD_BASE"), "/")
	if base == "" {
		base = defaultDownloadURL + strings.TrimPrefix(version, "v")
	}
	for _, item := range []struct {
		name  string
		limit int64
	}{{archiveName(version), maxArchiveBytes}, {"checksums.txt", maxChecksumBytes}} {
		file, err := os.OpenFile(filepath.Join(temp, item.name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if err != nil {
			return err
		}
		err = m.download(ctx, base+"/"+item.name, file, item.limit)
		closeErr := file.Close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
	}
	_, err = InstallArchive(ctx, m.InstallRoot, filepath.Join(temp, archiveName(version)), filepath.Join(temp, "checksums.txt"), version)
	return err
}
