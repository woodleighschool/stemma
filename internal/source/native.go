package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"unicode"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/woodleighschool/stemma/plugin"
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
	Release            string            `json:"release,omitempty" jsonschema_description:"GitHub release tag, tag glob, or latest. Omitted or empty values select latest. A value containing *, ? or [ selects the newest published release whose tag matches, using doublestar semantics."`
	IncludePrereleases bool              `json:"include_prereleases,omitempty" jsonschema_description:"Include prereleases when discovering latest or a tag glob; select the newest published non-draft release. Explicit release tags are unchanged."`
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

// nativeObservation records what discovery found beyond the declaration: a
// matched link or a GitHub release asset. Declared HTTP URLs, files and local
// trees observe nothing, so a URL supplied as a credential stays out of the lock.
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

// fetchNative reads a local input or downloads its URL. A declared HTTP URL is
// fetched as written; a matched link or GitHub asset comes from the observation.
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
	address := observed.URL
	if s.Type == "http" && s.Match == "" {
		address = s.URL
	}
	return m.download(ctx, s, address, previous)
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
		if releasePattern(s.Release) && !doublestar.ValidatePattern(s.Release) {
			return fmt.Errorf("invalid GitHub release pattern %q", s.Release)
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

func validFilename(name string) bool {
	return name != "" && name != "." && name != ".." && !strings.ContainsAny(name, "/\\:") && strings.IndexFunc(name, unicode.IsControl) < 0 && filepath.IsLocal(name)
}
