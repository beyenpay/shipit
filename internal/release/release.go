// Package release talks to the GitHub Releases API: it resolves a release,
// downloads an asset, verifies it against checksums.txt and unpacks it safely.
package release

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

const (
	DefaultBaseURL = "https://api.github.com"
	ChecksumsAsset = "checksums.txt"

	maxArchiveSize   = 1 << 30 // compressed archive, 1 GiB
	maxChecksumsSize = 1 << 20
	maxReleaseJSON   = 8 << 20
	maxErrBody       = 4 << 10
)

type Asset struct {
	ID    int64  `json:"id"`
	Name  string `json:"name"`
	Size  int64  `json:"size"`
	State string `json:"state"`
}

type Release struct {
	Tag        string  `json:"tag_name"`
	Draft      bool    `json:"draft"`
	Prerelease bool    `json:"prerelease"`
	Assets     []Asset `json:"assets"`
}

// Client is a minimal GitHub API client. An empty Token is fine for public
// repositories (but the unauthenticated rate limit is low).
type Client struct {
	BaseURL string
	Token   string
	HTTP    *http.Client
}

func NewClient(token string) *Client {
	tr := &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: 30 * time.Second,
		MaxIdleConns:          4,
		IdleConnTimeout:       60 * time.Second,
	}
	// No overall client timeout: downloads can be large. The caller bounds the
	// whole operation with the context.
	return &Client{BaseURL: DefaultBaseURL, Token: token, HTTP: &http.Client{Transport: tr}}
}

// Release resolves a release. An empty tag means "latest", which GitHub
// defines as the most recently created non-draft, non-prerelease release
// (not necessarily the highest version).
func (c *Client) Release(ctx context.Context, repo, tag string) (*Release, error) {
	base, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	path := base + "/releases/latest"
	if tag != "" {
		path = base + "/releases/tags/" + url.PathEscape(tag)
	}
	resp, err := c.get(ctx, path, "application/vnd.github+json")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	var r Release
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxReleaseJSON)).Decode(&r); err != nil {
		return nil, fmt.Errorf("github: decode release: %w", err)
	}
	if r.Draft {
		return nil, fmt.Errorf("release %s is still a draft", r.Tag)
	}
	if tag != "" && r.Tag != tag {
		return nil, fmt.Errorf("github returned release %q for tag %q", r.Tag, tag)
	}
	return &r, nil
}

// CheckRepo verifies that the token (or anonymous access) can read the repo.
func (c *Client) CheckRepo(ctx context.Context, repo string) error {
	base, err := repoPath(repo)
	if err != nil {
		return err
	}
	resp, err := c.get(ctx, base, "application/vnd.github+json")
	if err != nil {
		return err
	}
	return resp.Body.Close()
}

// TokenStatus validates the client's token against the API and returns its
// expiry as reported by GitHub (empty if it does not expire or there is no
// token).
func (c *Client) TokenStatus(ctx context.Context) (expires string, err error) {
	resp, err := c.get(ctx, "/rate_limit", "application/vnd.github+json")
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return resp.Header.Get("github-authentication-token-expiration"), nil
}

// DownloadVerified downloads the named asset of rel to dst (which must not
// exist) and checks its sha256 against the release's checksums.txt. On any
// failure dst is removed.
func (c *Client) DownloadVerified(ctx context.Context, repo string, rel *Release, name, dst string) error {
	asset, err := rel.need(name)
	if err != nil {
		return err
	}
	sums, err := rel.need(ChecksumsAsset)
	if err != nil {
		return err
	}
	if asset.Size > maxArchiveSize {
		return fmt.Errorf("asset %q is %d bytes, over the %d byte limit", name, asset.Size, int64(maxArchiveSize))
	}

	raw, err := c.readAsset(ctx, repo, sums, maxChecksumsSize)
	if err != nil {
		return fmt.Errorf("download %s: %w", ChecksumsAsset, err)
	}
	table, err := ParseChecksums(raw)
	if err != nil {
		return err
	}
	want, ok := table[name]
	if !ok {
		return fmt.Errorf("%s has no entry for %q", ChecksumsAsset, name)
	}

	got, err := c.saveAsset(ctx, repo, asset, dst)
	if err != nil {
		os.Remove(dst)
		return fmt.Errorf("download %s: %w", name, err)
	}
	if got != want {
		os.Remove(dst)
		return fmt.Errorf("sha256 mismatch for %s: want %s, got %s", name, want, got)
	}
	return nil
}

// ParseChecksums parses `sha256sum` output ("<hex>  <name>" or "<hex> *<name>").
func ParseChecksums(b []byte) (map[string]string, error) {
	m := make(map[string]string)
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		sum, name, ok := strings.Cut(line, " ")
		if !ok {
			return nil, fmt.Errorf("%s: malformed line %q", ChecksumsAsset, line)
		}
		sum = strings.ToLower(sum)
		if raw, err := hex.DecodeString(sum); err != nil || len(raw) != sha256.Size {
			return nil, fmt.Errorf("%s: invalid sha256 %q", ChecksumsAsset, sum)
		}
		name = strings.TrimPrefix(strings.TrimLeft(name, " *"), "./")
		if name == "" {
			return nil, fmt.Errorf("%s: malformed line %q", ChecksumsAsset, line)
		}
		m[name] = sum
	}
	return m, nil
}

func (r *Release) need(name string) (*Asset, error) {
	names := make([]string, 0, len(r.Assets))
	for i := range r.Assets {
		a := &r.Assets[i]
		names = append(names, a.Name)
		if a.Name != name {
			continue
		}
		if a.State != "" && a.State != "uploaded" {
			return nil, fmt.Errorf("asset %q is in state %q, not uploaded", name, a.State)
		}
		return a, nil
	}
	have := "none"
	if len(names) > 0 {
		have = strings.Join(names, ", ")
	}
	return nil, fmt.Errorf("release %s has no asset %q (has: %s)", r.Tag, name, have)
}

func (c *Client) readAsset(ctx context.Context, repo string, a *Asset, limit int64) ([]byte, error) {
	resp, err := c.openAsset(ctx, repo, a)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > limit {
		return nil, fmt.Errorf("larger than %d bytes", limit)
	}
	return b, nil
}

// saveAsset streams the asset into dst and returns the hex sha256 of what was
// written.
func (c *Client) saveAsset(ctx context.Context, repo string, a *Asset, dst string) (string, error) {
	resp, err := c.openAsset(ctx, repo, a)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()

	f, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return "", err
	}
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(resp.Body, maxArchiveSize+1))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		return "", err
	}
	if n > maxArchiveSize {
		return "", fmt.Errorf("larger than %d bytes", int64(maxArchiveSize))
	}
	if a.Size > 0 && n != a.Size {
		return "", fmt.Errorf("got %d bytes, release says %d", n, a.Size)
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func (c *Client) openAsset(ctx context.Context, repo string, a *Asset) (*http.Response, error) {
	base, err := repoPath(repo)
	if err != nil {
		return nil, err
	}
	// The API answers with a redirect to a signed storage URL. net/http drops
	// the Authorization header when the redirect leaves api.github.com, so the
	// token is never sent to the storage host.
	return c.get(ctx, fmt.Sprintf("%s/releases/assets/%d", base, a.ID), "application/octet-stream")
}

func (c *Client) get(ctx context.Context, path, accept string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	req.Header.Set("User-Agent", "shipit")
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode/100 != 2 {
		defer resp.Body.Close()
		return nil, apiError(resp)
	}
	return resp, nil
}

func apiError(resp *http.Response) error {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrBody))
	var m struct {
		Message string `json:"message"`
	}
	_ = json.Unmarshal(body, &m)

	var hint string
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		hint = "token invalid or expired"
	case http.StatusNotFound:
		hint = "wrong repo or tag, or the token cannot read this repo (private repos need a token with Contents: read)"
	case http.StatusForbidden, http.StatusTooManyRequests:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			hint = "rate limit exceeded (configure a token to raise it)"
		} else {
			hint = "forbidden"
		}
	}
	msg := fmt.Sprintf("github: %s", resp.Status)
	if m.Message != "" {
		msg += ": " + m.Message
	}
	if hint != "" {
		msg += " (" + hint + ")"
	}
	return fmt.Errorf("%s", msg)
}

func repoPath(repo string) (string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") ||
		strings.HasPrefix(owner, ".") || strings.HasPrefix(name, ".") {
		return "", fmt.Errorf("invalid repo %q, want owner/name", repo)
	}
	return "/repos/" + url.PathEscape(owner) + "/" + url.PathEscape(name), nil
}
