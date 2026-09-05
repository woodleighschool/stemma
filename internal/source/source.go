// Package source resolves provider inputs into digest-pinned downloads.
package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
)

// Entry records only stable source references; credentials and redirects are excluded.
type Entry struct {
	ResolvedAt time.Time `json:"resolved_at,omitzero" yaml:"resolved_at,omitempty"`
	Tree       bool      `json:"tree,omitempty" yaml:"tree,omitempty"`
	Source     string    `json:"source" yaml:"source"`
	URL        string    `json:"url,omitempty" yaml:"url,omitempty"`
	Filename   string    `json:"filename" yaml:"filename"`
	Version    string    `json:"version,omitempty" yaml:"version,omitempty"`
	ReleaseID  int64     `json:"release_id,omitempty" yaml:"release_id,omitempty"`
	AssetID    int64     `json:"asset_id,omitempty" yaml:"asset_id,omitempty"`
	Artifact   cas.Ref   `json:"artifact" yaml:"artifact"`
}

// Manager acquires bytes using the same client locally and in CI.
type Manager struct {
	Store   *cas.Store
	Root    string
	Client  *http.Client
	Offline bool
}

// New creates a manager with bounded HTTP lifetimes and cross-host credential stripping.
func New(store *cas.Store, root string, offline bool) *Manager {
	return &Manager{Store: store, Root: root, Offline: offline, Client: &http.Client{Timeout: 15 * time.Minute, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return errors.New("too many redirects")
		}
		if req.URL.Host != via[0].URL.Host {
			req.Header.Del("Authorization")
		}
		if via[0].URL.Scheme == "https" && req.URL.Scheme != "https" {
			return errors.New("refusing HTTPS downgrade")
		}
		return nil
	}}}
}

// Resolve checks current upstream bytes and returns their immutable identity.
func (m *Manager) Resolve(ctx context.Context, s config.Source) (Entry, error) {
	if err := s.Validate(); err != nil {
		return Entry{}, err
	}
	if m.Offline && s.Type != "file" && s.Type != "local" {
		return Entry{}, errors.New("offline mode cannot resolve sources")
	}
	entry := Entry{Source: config.Fingerprint(s), URL: s.URL, Filename: s.Filename, Version: s.Version}
	if s.Type == "local" {
		entry.Tree = true
	}
	if s.Type == "file" {
		info, err := os.Stat(filepath.Join(m.Root, s.Path))
		if err != nil {
			return Entry{}, err
		}
		entry.Tree = info.IsDir()
	}
	if s.Type == "github" {
		if err := m.github(ctx, s, &entry); err != nil {
			return Entry{}, err
		}
	}
	if entry.Filename == "" {
		switch s.Type {
		case "local":
			entry.Filename = path.Base(s.Base)
			if entry.Filename == "." || entry.Filename == "" {
				entry.Filename = "local"
			}
		case "file":
			entry.Filename = filepath.Base(s.Path)
		default:
			u, err := url.Parse(entry.URL)
			if err != nil {
				return Entry{}, err
			}
			entry.Filename = path.Base(u.Path)
		}
	}
	if !validFilename(entry.Filename) {
		return Entry{}, errors.New("source has no safe filename; set filename explicitly")
	}
	ref, err := m.download(ctx, s, entry, s.SHA256)
	entry.Artifact = ref
	return entry, err
}

// Acquire uses cached bytes or fetches precisely the locked URL and expected digest.
func (m *Manager) Acquire(ctx context.Context, s config.Source, entry Entry) (bool, error) {
	if !validFilename(entry.Filename) || entry.Source != config.Fingerprint(s) || !config.ValidDigest(entry.Artifact.SHA256) {
		return false, errors.New("invalid or stale source lock")
	}
	if s.Type == "file" || s.Type == "local" {
		current, err := m.Resolve(ctx, s)
		if err != nil {
			return false, err
		}
		current.ResolvedAt = entry.ResolvedAt
		if current != entry {
			return false, errors.New("local source changed from its locked content")
		}
		return true, nil
	}
	if err := m.Store.Verify(ctx, entry.Artifact); err == nil {
		return true, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, err
	}
	if m.Offline {
		return false, fmt.Errorf("offline cache miss for %s", entry.Filename)
	}
	ref, err := m.download(ctx, s, entry, entry.Artifact.SHA256)
	if err != nil {
		return false, err
	}
	if ref != entry.Artifact {
		return false, errors.New("download size differs from lockfile")
	}
	return false, nil
}

func (m *Manager) download(ctx context.Context, s config.Source, entry Entry, expected string) (cas.Ref, error) {
	if s.Type == "local" {
		project, err := os.OpenRoot(m.Root)
		if err != nil {
			return cas.Ref{}, err
		}
		defer func() { _ = project.Close() }()
		base := s.Base
		if base == "" {
			base = "."
		}
		root, err := project.OpenRoot(base)
		if err != nil {
			return cas.Ref{}, err
		}
		defer func() { _ = root.Close() }()
		var names []string
		seen := map[string]bool{}
		for _, pattern := range s.Include {
			matched := false
			err := doublestar.GlobWalk(root.FS(), pattern, func(name string, _ fs.DirEntry) error {
				matched = true
				if !seen[name] {
					if len(names) >= 100000 {
						return errors.New("local source exceeds 100000 entries")
					}
					seen[name] = true
					names = append(names, name)
				}
				return nil
			}, doublestar.WithNoFollow(), doublestar.WithFailOnIOErrors())
			if err != nil {
				return cas.Ref{}, fmt.Errorf("local include %q: %w", pattern, err)
			}
			if !matched {
				return cas.Ref{}, fmt.Errorf("local include %q matched no files", pattern)
			}
		}
		return m.importTree(ctx, root, names, expected)
	}
	if s.Type == "file" {
		root, err := os.OpenRoot(m.Root)
		if err != nil {
			return cas.Ref{}, err
		}
		defer func() { _ = root.Close() }()
		f, err := root.Open(s.Path)
		if err != nil {
			return cas.Ref{}, err
		}
		defer func() { _ = f.Close() }()
		info, err := f.Stat()
		if err != nil {
			return cas.Ref{}, err
		}
		if !info.Mode().IsRegular() {
			if !info.IsDir() || !entry.Tree {
				return cas.Ref{}, errors.New("file source changed type or is not a regular file/directory")
			}
			tree, err := root.OpenRoot(s.Path)
			if err != nil {
				return cas.Ref{}, err
			}
			defer func() { _ = tree.Close() }()
			return m.importTree(ctx, tree, nil, expected)
		}
		if entry.Tree {
			return cas.Ref{}, errors.New("file source changed from directory to file")
		}
		return m.Store.Import(ctx, f, expected)
	}
	if err := config.ValidateHTTPURL(entry.URL); err != nil {
		return cas.Ref{}, fmt.Errorf("lockfile URL: %w", err)
	}
	u, _ := url.Parse(entry.URL)
	// A lockfile may pin a release asset, but must not redirect an HTTP source to a different origin.
	if s.Type == "http" && entry.URL != s.URL {
		return cas.Ref{}, errors.New("locked HTTP URL does not match configuration")
	}
	if s.Type == "github" && (u.Host != "github.com" || !strings.HasPrefix(u.Path, "/"+s.Repository+"/releases/download/")) {
		return cas.Ref{}, errors.New("locked asset does not belong to the configured GitHub repository")
	}
	req, err := m.request(ctx, entry.URL, s.TokenEnv)
	if err != nil {
		return cas.Ref{}, err
	}
	res, err := m.Client.Do(req)
	if err != nil {
		return cas.Ref{}, transportError("download", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return cas.Ref{}, fmt.Errorf("download returned HTTP %d", res.StatusCode)
	}
	if res.ContentLength > cas.MaxObjectSize {
		return cas.Ref{}, errors.New("download exceeds 16 GiB")
	}
	return m.Store.Import(ctx, res.Body, expected)
}

func (m *Manager) importTree(ctx context.Context, root *os.Root, names []string, expected string) (cas.Ref, error) {
	staging, err := os.CreateTemp(filepath.Join(m.Store.Dir, "work"), "source-*.tar")
	if err != nil {
		return cas.Ref{}, err
	}
	defer func() { _ = os.Remove(staging.Name()) }()
	packErr := archive.PackSelected(ctx, root, names, staging)
	closeErr := staging.Close()
	if packErr != nil {
		return cas.Ref{}, packErr
	}
	if closeErr != nil {
		return cas.Ref{}, closeErr
	}
	return m.Store.ImportFile(ctx, staging.Name(), expected)
}

func (m *Manager) request(ctx context.Context, address, tokenEnv string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "stemma/0.1")
	if tokenEnv != "" {
		token := os.Getenv(tokenEnv)
		if token == "" {
			return nil, fmt.Errorf("environment variable %s is required", tokenEnv)
		}
		req.Header.Set("Authorization", "Bearer "+token)
	}
	return req, nil
}

func (m *Manager) github(ctx context.Context, s config.Source, entry *Entry) error {
	endpoint := "https://api.github.com/repos/" + s.Repository + "/releases/latest"
	if s.Release != "" && s.Release != "latest" {
		endpoint = "https://api.github.com/repos/" + s.Repository + "/releases/tags/" + url.PathEscape(s.Release)
	}
	req, err := m.request(ctx, endpoint, s.TokenEnv)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	res, err := m.Client.Do(req)
	if err != nil {
		return transportError("GitHub release lookup", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub release lookup returned HTTP %d", res.StatusCode)
	}
	var release struct {
		ID     int64  `json:"id"`
		Tag    string `json:"tag_name"`
		Draft  bool   `json:"draft"`
		Assets []struct {
			ID   int64  `json:"id"`
			Name string `json:"name"`
			URL  string `json:"browser_download_url"`
		} `json:"assets"`
	}
	if err := json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(&release); err != nil {
		return err
	}
	if release.Draft {
		return errors.New("draft releases are not supported")
	}
	for _, asset := range release.Assets {
		if asset.Name == s.Asset {
			entry.URL = asset.URL
			entry.ReleaseID = release.ID
			entry.AssetID = asset.ID
			if entry.Filename == "" {
				entry.Filename = asset.Name
			}
			if entry.Version == "" {
				entry.Version = strings.TrimPrefix(release.Tag, "v")
			}
			return nil
		}
	}
	return fmt.Errorf("GitHub release %s has no asset %q", release.Tag, s.Asset)
}

func validFilename(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\\x00\r\n") && filepath.IsLocal(name)
}

// Redirect targets may contain temporary credentials. Do not include their URLs
// in reports or durable state when a request fails.
func transportError(operation string, err error) error {
	var requestError *url.Error
	for errors.As(err, &requestError) {
		err = requestError.Err
	}
	return fmt.Errorf("%s failed: %w", operation, err)
}
