package source

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"

	"github.com/woodleighschool/stemma/plugin"
)

// Acquisition describes exactly one transfer. Requests are transient; only the
// resolver's observation and the resulting content belong in durable state.
type Acquisition struct {
	Download *Download
	Artifact *plugin.Artifact
	File     *fileRequest
	Evidence map[string]json.RawMessage
}

// Download allows an in-process resolver to supply a transport, such as an OCI
// challenge client. Ordinary requests use the manager's redirect-safe client.
type Download struct {
	plugin.Download

	Do func(*http.Request) (*http.Response, error)

	// AllowHTTPRedirect permits public mirrors only with an independently pinned SHA-256.
	AllowHTTPRedirect bool
}

type fileRequest struct {
	Path     string
	Include  []string
	Filename string
	SHA256   string
}

func (m *Manager) acquire(ctx context.Context, resolver Resolver, input plugin.Input, entry Entry, previous *record) (record, bool, error) {
	request, err := resolver.Acquire(ctx, input, entry.Observation)
	if err != nil {
		return record{}, false, err
	}
	kinds := 0
	if request.Download != nil {
		kinds++
	}
	if request.Artifact != nil {
		kinds++
	}
	if request.File != nil {
		kinds++
	}
	if kinds != 1 {
		return record{}, false, errors.New("resolver must return exactly one acquisition request")
	}
	var result record
	var reused bool
	switch {
	case request.Download != nil:
		result, reused, err = m.download(ctx, *request.Download, previous)
	case request.Artifact != nil:
		result.Content, err = m.importArtifact(ctx, *request.Artifact)
		if request.Evidence == nil {
			request.Evidence = request.Artifact.Evidence
		}
	case request.File != nil:
		result.Content, err = m.readLocal(ctx, *request.File)
	}
	if err != nil {
		return record{}, false, err
	}
	result.Evidence, err = canonicalEvidence(request.Evidence)
	return result, reused, err
}

// validateRequestURL checks runtime requests, which may use signed URLs. Their
// credentials are never copied to a lock or included in transport errors.
func validateRequestURL(address string) error {
	u, err := url.Parse(address)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil || u.Fragment != "" {
		return errors.New("download requires an http(s) URL without user information or fragment")
	}
	return nil
}

func validateDownload(download plugin.Download) error {
	if err := validateRequestURL(download.URL); err != nil {
		return err
	}
	if download.Filename != "" && !validFilename(download.Filename) {
		return errors.New("download filename must be a basename")
	}
	if download.SHA256 != "" && !validDigest(download.SHA256) {
		return errors.New("download requires a valid sha256")
	}
	return validateHeaders(download.Headers)
}
