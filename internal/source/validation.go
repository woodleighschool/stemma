package source

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"unicode"

	"github.com/woodleighschool/stemma/plugin"
	"golang.org/x/net/http/httpguts"
)

func configFor[C any](input plugin.Input) (C, error) {
	var config C
	data, err := json.Marshal(input.Config)
	if err != nil {
		return config, err
	}
	if err := decode(data, &config); err != nil {
		return config, fmt.Errorf("%s resolver config: %w", input.Resolver, err)
	}
	return config, nil
}

func validateContent(filename, digest string) error {
	if filename != "" && !validFilename(filename) {
		return errors.New("filename must be a basename")
	}
	if digest != "" && !validDigest(digest) {
		return errors.New("sha256 must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func validateHeaders(headers map[string]string) error {
	seen := map[string]bool{}
	for name, value := range headers {
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
		}
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
