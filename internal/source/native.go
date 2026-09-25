package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"mime"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/net/html"
	"golang.org/x/net/http/httpguts"
)

type nativeConfig struct {
	Type               string            `json:"-"`
	Include            []string          `json:"include,omitempty" jsonschema_description:"Local tree globs using doublestar semantics. Each pattern must match at least one entry."`
	Base               string            `json:"base,omitempty" jsonschema_description:"Local tree root, relative to the resource file. Defaults to its directory."`
	URL                string            `json:"url,omitempty" jsonschema_description:"Stable HTTP download URL. Redirects are followed without retaining temporary URLs in the lockfile."`
	Match              string            `json:"match,omitempty" jsonschema_description:"Download page regular expression whose full matches are URL references resolved against the page's base URL. HTML pages match element attribute values; other pages match the response body. Must identify one distinct stable HTTP(S) download URL."`
	Path               string            `json:"path,omitempty" jsonschema_description:"Exact file or directory path. Relative paths resolve from the resource file; absolute paths select a host location."`
	Repository         string            `json:"repository,omitempty" jsonschema_description:"GitHub repository in owner/name form."`
	Release            string            `json:"release,omitempty" jsonschema_description:"GitHub release tag, or latest. Omitted or empty values select latest."`
	IncludePrereleases bool              `json:"include_prereleases,omitempty" jsonschema_description:"Include prereleases when discovering latest; select the newest published non-draft release. Explicit release tags are unchanged."`
	Asset              string            `json:"asset,omitempty" jsonschema_description:"GitHub asset-name glob using doublestar semantics. Must match exactly one release asset; exact names also work."`
	Filename           string            `json:"filename,omitempty" jsonschema_description:"Optional input basename override. HTTP defaults to Content-Disposition, then final and original URL basenames; GitHub uses the selected asset name; file and local use the selected path or base. Independent of publication naming."`
	SHA256             string            `json:"sha256,omitempty" jsonschema_description:"Optional expected SHA-256 content digest, as 64 lowercase hexadecimal characters."`
	Token              string            `json:"token,omitempty" jsonschema_description:"Optional bearer token. Mutually exclusive with an Authorization header."`
	Headers            map[string]string `json:"headers,omitempty" jsonschema_description:"HTTP request headers, including optional User-Agent and Referer overrides. Credentials and custom headers are confined to the source origin."`
}

// Native field sets also constrain the editor schema.
var nativeFields = map[string][]string{
	"http":   {"url", "match", "filename", "sha256", "token", "headers"},
	"github": {"repository", "release", "include_prereleases", "asset", "filename", "sha256", "token"},
	"file":   {"path", "filename", "sha256"},
	"local":  {"base", "include", "filename", "sha256"},
}

// nativeObservation records where remote content came from. File and local
// inputs observe nothing beyond their declaration.
type nativeObservation struct {
	URL       string `json:"url,omitempty"`
	Release   string `json:"release,omitempty"`
	ReleaseID int64  `json:"release_id,omitempty"`
	AssetID   int64  `json:"asset_id,omitempty"`
}

// NativeResolver reports whether the name is reserved for a built-in source.
func NativeResolver(name string) bool {
	_, ok := nativeFields[name]
	return ok
}

// ValidateInput checks a native declaration without acquiring its content.
func ValidateInput(input plugin.Input) error {
	_, err := native(input)
	return err
}

func native(input plugin.Input) (nativeConfig, error) {
	var s nativeConfig
	for field := range input.Config {
		if !slices.Contains(nativeFields[input.Resolver], field) {
			return s, fmt.Errorf("%s resolver does not support field %q", input.Resolver, field)
		}
	}
	data, err := json.Marshal(input.Config)
	if err != nil {
		return s, err
	}
	if err := decode(data, &s); err != nil {
		return s, fmt.Errorf("%s resolver config: %w", input.Resolver, err)
	}
	s.Type = input.Resolver
	if s.Type == "file" && s.Path != "" {
		s.Path, err = resolvePath(input.Base, s.Path)
	} else if s.Type == "local" {
		s.Base, err = projectPath(input.Base, s.Base)
	}
	if err != nil {
		return s, err
	}
	if err := s.Validate(); err != nil {
		return s, err
	}
	if len(s.Headers) > 0 {
		headers := make(map[string]string, len(s.Headers))
		for name, value := range s.Headers {
			headers[http.CanonicalHeaderKey(name)] = value
		}
		s.Headers = headers
	}
	return s, nil
}

// absolutePath reports whether a declared path names a host location instead of
// one resolved from the project. A slash-rooted path counts on every platform so
// that a catalog reads the same way everywhere.
func absolutePath(name string) bool { return path.IsAbs(name) || filepath.IsAbs(name) }

// resolvePath resolves a declared path with ordinary filesystem semantics:
// absolute paths name a host location, and relative paths resolve from the
// resource file's directory. Relative results stay relative to the project, so
// they identify the same input on any checkout.
func resolvePath(base, name string) (string, error) {
	if strings.ContainsAny(name, "\x00\r\n") {
		return "", errors.New("input path must not contain control characters")
	}
	if filepath.IsAbs(name) {
		return filepath.Clean(name), nil
	}
	if path.IsAbs(name) {
		return path.Clean(name), nil
	}
	if path.IsAbs(base) || strings.ContainsAny(base+name, "\\:") {
		return "", errors.New("input path must be absolute or a slash-separated relative path")
	}
	resolved := path.Join(base, name)
	if resolved == "" {
		resolved = "."
	}
	return resolved, nil
}

// projectPath resolves a tree root, which stays inside the project because its
// contents are selected by globs walked from that root without following links.
func projectPath(base, name string) (string, error) {
	resolved, err := resolvePath(base, name)
	if err != nil {
		return "", err
	}
	if absolutePath(resolved) || !safeRelative(resolved) {
		return "", errors.New("local input base must stay inside the project")
	}
	return resolved, nil
}

// hostPath locates a file input on the filesystem.
func (s nativeConfig) hostPath(root string) string {
	if absolutePath(s.Path) {
		return filepath.FromSlash(s.Path)
	}
	return filepath.Join(root, filepath.FromSlash(s.Path))
}

// discoverNative reports what a native declaration names now. A GitHub
// release asset never changes; an HTTP URL can serve new bytes at any time.
func (m *Manager) discoverNative(ctx context.Context, input plugin.Input) (Discovery, error) {
	s, err := native(input)
	if err != nil {
		return Discovery{}, err
	}
	var observed nativeObservation
	switch s.Type {
	case "github":
		err = m.github(ctx, s, &observed)
	case "http":
		observed.URL = s.URL
		if s.Match != "" {
			observed.URL, err = m.discoverLink(ctx, s)
		}
	}
	if err != nil {
		return Discovery{}, err
	}
	data, err := json.Marshal(observed)
	return Discovery{Observation: data, Immutable: s.Type == "github"}, err
}

// fetchNative reads a local input or downloads the observed URL.
func (m *Manager) fetchNative(ctx context.Context, input plugin.Input, observation json.RawMessage, previous *record) (record, bool, error) {
	s, err := native(input)
	if err != nil {
		return record{}, false, err
	}
	if s.Type == "file" || s.Type == "local" {
		content, err := m.readLocal(ctx, s)
		return record{Content: content}, false, err
	}
	var observed nativeObservation
	if err := decode(observation, &observed); err != nil {
		return record{}, false, fmt.Errorf("observation: %w", err)
	}
	return m.download(ctx, s, observed.URL, previous)
}

func safeRelative(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\:\x00\r\n") || !filepath.IsLocal(filepath.FromSlash(name)) {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}

func (s nativeConfig) Validate() error {
	if s.Filename != "" && !validFilename(s.Filename) {
		return errors.New("filename must be a basename")
	}
	seen := map[string]bool{}
	for name, value := range s.Headers {
		if !httpguts.ValidHeaderFieldName(name) || !httpguts.ValidHeaderFieldValue(value) {
			return errors.New("invalid HTTP header name or value")
		}
		name = http.CanonicalHeaderKey(name)
		if seen[name] {
			return errors.New("HTTP header names must be unique ignoring case")
		}
		seen[name] = true
		switch name {
		case "Host", "Content-Length", "Transfer-Encoding", "Trailer":
			return fmt.Errorf("HTTP header %s is managed by the downloader", name)
		case "Authorization":
			if s.Token != "" {
				return errors.New("use token or an Authorization header, not both")
			}
		}
	}
	if s.SHA256 != "" && !validDigest(s.SHA256) {
		return errors.New("sha256 must be 64 lowercase hexadecimal characters")
	}
	if s.Match != "" {
		if _, err := regexp.Compile(s.Match); err != nil {
			return fmt.Errorf("source match: %w", err)
		}
	}
	switch s.Type {
	case "http":
		if err := validateHTTPURL(s.URL); err != nil {
			return err
		}
	case "github":
		owner, repo, ok := strings.Cut(s.Repository, "/")
		if !ok || owner == "" || repo == "" || strings.ContainsAny(owner+repo, "/ ?#\\") || strings.IndexFunc(s.Repository, unicode.IsControl) >= 0 || owner == "." || owner == ".." || repo == "." || repo == ".." {
			return errors.New("GitHub source requires repository owner/name")
		}
		if s.Asset == "" || !doublestar.ValidatePattern(s.Asset) {
			return fmt.Errorf("invalid GitHub asset pattern %q", s.Asset)
		}
	case "file":
		if s.Path == "" {
			return errors.New("file source requires a file or directory path")
		}
	case "local":
		if len(s.Include) == 0 || (s.Base != "" && !safeRelative(s.Base)) {
			return errors.New("local source requires include patterns relative to its software-family file")
		}
		for _, pattern := range s.Include {
			if !safeRelative(pattern) || !doublestar.ValidatePattern(pattern) {
				return fmt.Errorf("invalid local include pattern %q", pattern)
			}
		}
	default:
		return fmt.Errorf("unsupported source type %q", s.Type)
	}
	return nil
}

// validateHTTPURL allows stable query identifiers, but excludes embedded
// credentials and commonly signed, expiring download references from locks.
func validateHTTPURL(address string) error {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("HTTP source requires an http(s) URL without credentials or fragment")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return errors.New("HTTP source contains an invalid query")
	}
	for key := range query {
		key = strings.ToLower(key)
		if strings.HasPrefix(key, "x-amz-") || strings.HasPrefix(key, "x-goog-") {
			return errors.New("HTTP source must use a stable URL, not an expiring signed download")
		}
		switch strings.ReplaceAll(strings.ReplaceAll(key, "_", ""), "-", "") {
		case "token", "accesstoken", "authtoken", "auth", "authorization", "apikey", "key", "signature", "sig", "expires", "expiry", "expiration", "credential", "credentials", "password", "secret":
			return errors.New("HTTP source query must not contain credentials or expiration")
		}
	}
	return nil
}

// readLocal hashes a file or local tree as it is now.
func (m *Manager) readLocal(ctx context.Context, s nativeConfig) (content Content, err error) {
	content.Filename = s.Filename
	if s.Type == "local" {
		done := plugin.Stage(ctx, "Reading local inputs", plugin.Detail(s.Base))
		defer func() { done(err) }()
		project, err := os.OpenRoot(m.Root)
		if err != nil {
			return content, err
		}
		defer func() { _ = project.Close() }()
		base := s.Base
		if base == "" {
			base = "."
		}
		info, err := project.Lstat(base)
		if err != nil {
			return content, err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return content, errors.New("local input base must not be a symlink")
		}
		content.Tree, content.Mode = info.IsDir(), uint32(info.Mode().Perm())
		if content.Filename == "" {
			content.Filename = path.Base(s.Base)
			if content.Filename == "." || content.Filename == "" {
				content.Filename = "local"
			}
		}
		if !validFilename(content.Filename) {
			return content, errors.New("input has no safe filename; set filename explicitly")
		}
		root, err := project.OpenRoot(base)
		if err != nil {
			return content, err
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
				return content, fmt.Errorf("local include %q: %w", pattern, err)
			}
			if !matched {
				return content, fmt.Errorf("local include %q matched no files", pattern)
			}
		}
		content.Artifact, err = m.importTree(ctx, root, names, s.SHA256)
		return content, err
	}
	done := plugin.Stage(ctx, "Reading local input", plugin.Detail(filepath.Base(s.Path)))
	defer func() { done(err) }()
	name := s.hostPath(m.Root)
	f, err := os.Open(name)
	if err != nil {
		return content, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return content, err
	}
	content.Tree, content.Mode = info.IsDir(), uint32(info.Mode().Perm())
	if content.Filename == "" {
		content.Filename = filepath.Base(s.Path)
	}
	if !validFilename(content.Filename) {
		return content, errors.New("input has no safe filename; set filename explicitly")
	}
	switch {
	case info.IsDir():
		tree, err := os.OpenRoot(name)
		if err != nil {
			return content, err
		}
		defer func() { _ = tree.Close() }()
		content.Artifact, err = m.importTree(ctx, tree, nil, s.SHA256)
		return content, err
	case info.Mode().IsRegular():
		content.Artifact, err = m.Store.Import(ctx, f, s.SHA256)
		return content, err
	}
	return content, errors.New("file source is not a regular file or directory")
}

// download fetches an observed URL into the cache. With a previous record it
// asks conditionally and reuses that record when the server confirms it.
func (m *Manager) download(ctx context.Context, s nativeConfig, address string, previous *record) (result record, reused bool, err error) {
	if err := validateHTTPURL(address); err != nil {
		return record{}, false, fmt.Errorf("observation URL: %w", err)
	}
	u, _ := url.Parse(address)
	if s.Type == "http" && s.Match == "" && address != s.URL {
		return record{}, false, errors.New("observed HTTP URL does not match configuration")
	}
	if s.Type == "github" && (u.Host != "github.com" || !strings.HasPrefix(u.Path, "/"+s.Repository+"/releases/download/")) {
		return record{}, false, errors.New("observed asset does not belong to the configured GitHub repository")
	}
	name := s.Filename
	if name == "" {
		name = urlName(u)
	}
	done := plugin.Stage(ctx, "Downloading input", plugin.Detail(name))
	defer func() {
		if reused {
			done(nil, plugin.Detail("unchanged"))
			return
		}
		done(err, plugin.Detail(result.Content.Filename))
	}()
	req, err := m.request(ctx, address, s)
	if err != nil {
		return record{}, false, err
	}
	validators := previous != nil && (previous.ETag != "" || previous.LastModified != "")
	if validators {
		if previous.ETag != "" {
			req.Header.Set("If-None-Match", previous.ETag)
		}
		if previous.LastModified != "" {
			req.Header.Set("If-Modified-Since", previous.LastModified)
		}
	}
	res, err := m.Client.Do(req)
	if err != nil {
		return record{}, false, transportError("download", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotModified && validators {
		return *previous, true, nil
	}
	if res.StatusCode != http.StatusOK {
		return record{}, false, fmt.Errorf("download returned HTTP %d", res.StatusCode)
	}
	// Hosts that ignore conditional requests still send validators; the same
	// strong ETag confirms the previous bytes without transferring them.
	if validators && previous.ETag != "" && !strings.HasPrefix(previous.ETag, "W/") && res.Header.Get("ETag") == previous.ETag && res.Header.Get("Last-Modified") == previous.LastModified {
		return *previous, true, nil
	}
	if res.ContentLength > cas.MaxObjectSize {
		return record{}, false, errors.New("download exceeds 16 GiB")
	}
	result.ETag, result.LastModified = res.Header.Get("ETag"), res.Header.Get("Last-Modified")
	result.Content = Content{Filename: s.Filename, Mode: 0o644}
	if result.Content.Filename == "" && s.Type == "github" {
		result.Content.Filename = path.Base(u.Path)
	}
	if result.Content.Filename == "" {
		result.Content.Filename = responseFilename(res, address)
	}
	if !validFilename(result.Content.Filename) {
		return record{}, false, errors.New("input has no safe filename; set filename explicitly")
	}
	plugin.Logger(ctx).DebugContext(ctx, "Download response", "bytes", res.ContentLength)
	result.Content.Artifact, err = m.Store.Import(ctx, plugin.ProgressReader(ctx, res.Body, res.ContentLength), s.SHA256)
	return result, false, err
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

func (m *Manager) request(ctx context.Context, address string, s nativeConfig) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "stemma/0.1")
	for name, value := range s.Headers {
		req.Header.Set(name, value)
	}
	if s.Token != "" {
		req.Header.Set("Authorization", "Bearer "+s.Token)
	}
	if s.Type == "http" {
		origin, _ := url.Parse(s.URL)
		if !sameOrigin(origin, req.URL) {
			stripPrivateHeaders(req.Header)
		}
	}
	return req, nil
}

func responseFilename(response *http.Response, original string) string {
	if _, params, err := mime.ParseMediaType(response.Header.Get("Content-Disposition")); err == nil && validFilename(params["filename"]) {
		return params["filename"]
	}
	if response.Request != nil {
		if name := path.Base(response.Request.URL.Path); validFilename(name) {
			return name
		}
	}
	address, _ := url.Parse(original)
	if name := path.Base(address.Path); validFilename(name) {
		return name
	}
	return ""
}

// urlName names a URL for progress displays by its final path segment, or its
// host. Queries may carry credentials, so they are never shown.
func urlName(u *url.URL) string {
	if u == nil {
		return ""
	}
	if name := path.Base(u.Path); name != "." && name != "/" {
		return name
	}
	return u.Hostname()
}

func sameOrigin(a, b *url.URL) bool {
	return a.Scheme == b.Scheme && strings.EqualFold(a.Host, b.Host)
}

func stripPrivateHeaders(headers http.Header) {
	for name := range headers {
		switch http.CanonicalHeaderKey(name) {
		case "Accept", "Accept-Encoding", "Accept-Language", "User-Agent", "If-None-Match", "If-Modified-Since":
		default:
			headers.Del(name)
		}
	}
}

// discoverLink finds the one download URL a page's match selects.
func (m *Manager) discoverLink(ctx context.Context, s nativeConfig) (link string, err error) {
	var host string
	if page, err := url.Parse(s.URL); err == nil {
		host = page.Hostname()
	}
	done := plugin.Stage(ctx, "Discovering source release", plugin.Detail(host))
	defer func() {
		found, _ := url.Parse(link)
		done(err, plugin.Detail(urlName(found)))
	}()
	req, err := m.request(ctx, s.URL, s)
	if err != nil {
		return "", err
	}
	res, err := m.Client.Do(req)
	if err != nil {
		return "", transportError("download page", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download page returned HTTP %d", res.StatusCode)
	}
	const limit = 4 << 20
	data, err := io.ReadAll(io.LimitReader(res.Body, limit+1))
	if err != nil {
		return "", transportError("download page", err)
	}
	if len(data) > limit {
		return "", errors.New("download page exceeds 4 MiB")
	}
	pattern, err := regexp.Compile(s.Match)
	if err != nil {
		return "", err
	}
	base := req.URL
	if res.Request != nil {
		base = res.Request.URL
	}
	// HTML carries links in element attributes; comments, text and script
	// content name URLs a browser never follows. Other pages are text.
	searched := []string{string(data)}
	if markup(res.Header.Get("Content-Type"), data) {
		searched, base = attributes(string(data), base)
	}
	matches := map[string]bool{}
	for _, text := range searched {
		for _, address := range pattern.FindAllString(text, -1) {
			reference, err := url.Parse(address)
			if err != nil || address == "" {
				return "", errors.New("download page match is not a valid URL reference")
			}
			resolved := base.ResolveReference(reference)
			address = resolved.String()
			if err := validateHTTPURL(address); err != nil {
				return "", fmt.Errorf("download page match must resolve to a stable HTTP(S) URL: %w", err)
			}
			matches[address] = true
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("download page matched %d distinct artifact URLs; expected one", len(matches))
	}
	for address := range matches {
		link = address
	}
	return link, nil
}

// markup reports whether a download page is HTML. Pages served without a
// declared type are sniffed the way a browser sniffs them.
func markup(declared string, page []byte) bool {
	media, _, err := mime.ParseMediaType(declared)
	if err != nil {
		media, _, _ = mime.ParseMediaType(http.DetectContentType(page))
	}
	return media == "text/html" || media == "application/xhtml+xml"
}

// attributes reports the decoded attribute values of an HTML page's elements
// and the base URL its references resolve against.
func attributes(page string, base *url.URL) ([]string, *url.URL) {
	var values []string
	document, located := base, false
	tokens := html.NewTokenizer(strings.NewReader(page))
	for {
		token := tokens.Next()
		if token == html.ErrorToken {
			return values, document
		}
		if token != html.StartTagToken && token != html.SelfClosingTagToken {
			continue
		}
		name, more := tokens.TagName()
		tag := string(name)
		// Stemma runs no scripts, so noscript content is ordinary markup.
		if tag == "noscript" {
			tokens.NextIsNotRawText()
		}
		for more {
			var key, value []byte
			key, value, more = tokens.TagAttr()
			values = append(values, string(value))
			if located || tag != "base" || string(key) != "href" {
				continue
			}
			if reference, err := url.Parse(strings.TrimSpace(string(value))); err == nil {
				document, located = base.ResolveReference(reference), true
			}
		}
	}
}

func (m *Manager) github(ctx context.Context, s nativeConfig, observed *nativeObservation) (err error) {
	done := plugin.Stage(ctx, "Discovering GitHub release", plugin.Detail(s.Repository))
	defer func() { done(err, plugin.Detail(observed.Release)) }()
	endpoint := "https://api.github.com/repos/" + s.Repository + "/releases/latest"
	if s.Release != "" && s.Release != "latest" {
		endpoint = "https://api.github.com/repos/" + s.Repository + "/releases/tags/" + url.PathEscape(s.Release)
	}
	var release githubRelease
	if s.IncludePrereleases && (s.Release == "" || s.Release == "latest") {
		// Publication time is independent of tag version and commit creation time.
		for page := 1; ; page++ {
			var releases []githubRelease
			endpoint := fmt.Sprintf("https://api.github.com/repos/%s/releases?per_page=100&page=%d", s.Repository, page)
			if err := m.githubResponse(ctx, s, endpoint, &releases); err != nil {
				return err
			}
			for _, candidate := range releases {
				if candidate.Draft {
					continue
				}
				if candidate.PublishedAt.IsZero() {
					return errors.New("published GitHub release is missing published_at")
				}
				if candidate.PublishedAt.After(release.PublishedAt) || candidate.PublishedAt.Equal(release.PublishedAt) && candidate.ID > release.ID {
					release = candidate
				}
			}
			if len(releases) < 100 {
				break
			}
		}
		if release.ID == 0 {
			return errors.New("no published GitHub releases found")
		}
	} else if err := m.githubResponse(ctx, s, endpoint, &release); err != nil {
		return err
	}
	if release.Draft {
		return errors.New("draft releases are not supported")
	}
	var matches []int
	var names []string
	for i, asset := range release.Assets {
		if doublestar.MatchUnvalidated(s.Asset, asset.Name) {
			matches = append(matches, i)
			names = append(names, asset.Name)
		}
	}
	if len(matches) != 1 {
		if len(matches) == 0 {
			for _, asset := range release.Assets {
				names = append(names, asset.Name)
			}
			return fmt.Errorf("GitHub release %s has no asset matching %q%s", release.Tag, s.Asset, assetNames("available assets", names))
		}
		return fmt.Errorf("GitHub release %s has %d assets matching %q; expected exactly one%s", release.Tag, len(matches), s.Asset, assetNames("matched assets", names))
	}
	asset := release.Assets[matches[0]]
	*observed = nativeObservation{URL: asset.URL, Release: release.Tag, ReleaseID: release.ID, AssetID: asset.ID}
	return nil
}

type githubRelease struct {
	ID          int64     `json:"id"`
	Tag         string    `json:"tag_name"`
	PublishedAt time.Time `json:"published_at"`
	Draft       bool      `json:"draft"`
	Assets      []struct {
		ID   int64  `json:"id"`
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

func (m *Manager) githubResponse(ctx context.Context, s nativeConfig, endpoint string, target any) error {
	req, err := m.request(ctx, endpoint, s)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-Github-Api-Version", "2022-11-28")
	res, err := m.Client.Do(req)
	if err != nil {
		return transportError("GitHub release lookup", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode != http.StatusOK {
		return fmt.Errorf("GitHub release lookup returned HTTP %d", res.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(res.Body, 4<<20)).Decode(target)
}

func assetNames(label string, names []string) string {
	if len(names) == 0 {
		return "; " + label + ": none"
	}
	if len(names) > 10 {
		return fmt.Sprintf("; %s: %d total", label, len(names))
	}
	return fmt.Sprintf("; %s: %q", label, names)
}

func validFilename(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\:") && strings.IndexFunc(name, unicode.IsControl) < 0 && filepath.IsLocal(name)
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
