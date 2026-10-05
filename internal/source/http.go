package source

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"

	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/plugin"
)

// download fetches a URL into the cache. With a previous record it asks
// conditionally and reuses that record when the server confirms it.
func (m *Manager) download(ctx context.Context, s Download, previous *record) (result record, reused bool, err error) {
	address := s.URL
	if err := validateDownload(s.Download); err != nil {
		return record{}, false, fmt.Errorf("observation URL: %w", err)
	}
	u, _ := url.Parse(address)
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
	req, err := m.request(ctx, address, s.Headers)
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
	do := s.Do
	if do == nil {
		do = m.Client.Do
	}
	res, err := do(req)
	if err != nil {
		return record{}, false, transportError("download", err)
	}
	defer func() { _ = res.Body.Close() }()
	if res.StatusCode == http.StatusNotModified && validators {
		return *previous, true, nil
	}
	if res.StatusCode != http.StatusOK {
		return record{}, false, httpStatusError("download", req, res)
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
	if result.Content.Filename == "" {
		result.Content.Filename = responseFilename(res, address)
	}
	if !validFilename(result.Content.Filename) {
		return record{}, false, errors.New("input has no safe filename; set filename explicitly")
	}
	plugin.Logger(ctx).DebugContext(ctx, "Download response", "bytes", res.ContentLength)
	var object cas.Ref
	object, err = m.Store.Import(ctx, plugin.ProgressReader(ctx, res.Body, res.ContentLength), s.SHA256)
	result.Content.SHA256 = object.SHA256
	return result, false, err
}

const userAgent = "stemma/0.1"

func (m *Manager) request(ctx context.Context, address string, headers map[string]string) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	for name, value := range headers {
		req.Header.Set(name, value)
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

// Redirect targets may contain temporary credentials. Do not include their URLs
// in reports or durable state when a request fails.
func transportError(operation string, err error) error {
	var requestError *url.Error
	for errors.As(err, &requestError) {
		err = requestError.Err
	}
	return fmt.Errorf("%s failed: %w", operation, err)
}

// httpStatusError names the responding endpoint without exposing signed query
// parameters or URL credentials. A redirect can fail on a different host.
func httpStatusError(operation string, request *http.Request, response *http.Response) error {
	address := request.URL
	if response.Request != nil && response.Request.URL != nil {
		address = response.Request.URL
	}
	status := fmt.Sprintf("HTTP %d", response.StatusCode)
	if description := http.StatusText(response.StatusCode); description != "" {
		status += " " + description
	}
	return fmt.Errorf("%s %s: %s", operation, diagnosticURL(address), status)
}

func diagnosticURL(address *url.URL) string {
	public := *address
	public.User = nil
	public.RawQuery = ""
	public.ForceQuery = false
	public.Fragment = ""
	public.RawFragment = ""
	return public.String()
}
