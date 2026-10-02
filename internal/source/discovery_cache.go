package source

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"sync"

	"golang.org/x/sync/singleflight"
)

const metadataLimit = 64 << 20

type metadataResponse struct {
	Status int         `json:"status"`
	Header http.Header `json:"header"`
	Body   []byte      `json:"body"`
	URL    string      `json:"url"`
}

type metadataCache struct {
	mu        sync.Mutex
	responses map[string]metadataResponse
	requests  singleflight.Group
}

type metadataTransport struct{ manager *Manager }

// metadataClient shares discoveries during a run and revalidates persisted
// responses on the next run. Artifact downloads use the ordinary client.
func (m *Manager) metadataClient() *http.Client {
	client := *m.Client
	client.Transport = metadataTransport{manager: m}
	return &client
}

func (transport metadataTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Method != http.MethodGet {
		return nil, fmt.Errorf("metadata requires GET")
	}
	m := transport.manager
	// Request headers include authorization and representation negotiation. Only
	// their hash enters a cache key; credentials never enter stored request data.
	key, err := fingerprint(struct {
		Kind   string      `json:"kind"`
		URL    string      `json:"url"`
		Header http.Header `json:"header"`
	}{"metadata/1", req.URL.String(), req.Header})
	if err != nil {
		return nil, err
	}
	cache := m.metadata
	result := cache.requests.DoChan(key, func() (any, error) {
		cache.mu.Lock()
		saved, ok := cache.responses[key]
		cache.mu.Unlock()
		if ok {
			return saved, nil
		}
		var previous metadataResponse
		known := m.Store.RecallSource(key, &previous)
		request := req.Clone(req.Context())
		if known {
			if etag := previous.Header.Get("ETag"); etag != "" {
				request.Header.Set("If-None-Match", etag)
			}
			if modified := previous.Header.Get("Last-Modified"); modified != "" {
				request.Header.Set("If-Modified-Since", modified)
			}
		}
		// #nosec G704 -- Source URLs are declared or resolved by validated resolvers; redirects strip private headers.
		response, err := m.Client.Do(request)
		if err != nil {
			return nil, transportError("metadata", err)
		}
		defer func() { _ = response.Body.Close() }()
		var current metadataResponse
		switch response.StatusCode {
		case http.StatusNotModified:
			if !known || request.Header.Get("If-None-Match") == "" && request.Header.Get("If-Modified-Since") == "" {
				return nil, fmt.Errorf("metadata returned an unsolicited HTTP 304")
			}
			current = previous
		default:
			data, err := io.ReadAll(io.LimitReader(response.Body, metadataLimit+1))
			if err != nil {
				return nil, transportError("metadata", err)
			}
			if len(data) > metadataLimit {
				return nil, fmt.Errorf("metadata exceeds 64 MiB")
			}
			current = metadataResponse{Status: response.StatusCode, Header: make(http.Header), Body: data, URL: req.URL.String()}
			// Do not persist response cookies or temporary redirected credentials.
			for _, name := range []string{"ETag", "Last-Modified", "Content-Type", "Link"} {
				if values := response.Header.Values(name); len(values) > 0 {
					current.Header[http.CanonicalHeaderKey(name)] = values
				}
			}
			if response.Request != nil && validateHTTPURL(response.Request.URL.String()) == nil {
				current.URL = response.Request.URL.String()
			}
		}
		if current.Status != http.StatusOK {
			return current, nil
		}
		if err := m.Store.RememberSource(key, current); err != nil {
			return nil, err
		}
		cache.mu.Lock()
		if cache.responses == nil {
			cache.responses = map[string]metadataResponse{}
		}
		cache.responses[key] = current
		cache.mu.Unlock()
		return current, nil
	})
	var received singleflight.Result
	select {
	case <-req.Context().Done():
		return nil, req.Context().Err()
	case received = <-result:
	}
	if received.Err != nil {
		return nil, received.Err
	}
	stored := received.Val.(metadataResponse)
	address, err := url.Parse(stored.URL)
	if err != nil {
		return nil, err
	}
	request := req.Clone(req.Context())
	request.URL = address
	header := stored.Header.Clone()
	header.Set("Content-Length", strconv.Itoa(len(stored.Body)))
	return &http.Response{StatusCode: stored.Status, Status: fmt.Sprintf("%d %s", stored.Status, http.StatusText(stored.Status)), Header: header, Body: io.NopCloser(bytes.NewReader(stored.Body)), ContentLength: int64(len(stored.Body)), Request: request}, nil
}
