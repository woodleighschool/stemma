package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"regexp"

	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/plugin"
)

type httpConfig struct {
	URL      string            `json:"url" jsonschema_description:"Stable HTTP download URL. Redirects are followed without retaining temporary URLs in the lockfile."`
	Match    string            `json:"match,omitempty" jsonschema_description:"Download page regular expression whose full matches are URL references resolved against the page's base URL. HTML pages match element attribute values; other pages match the response body. Must identify one distinct stable HTTP(S) download URL."`
	Filename string            `json:"filename,omitempty" jsonschema_description:"Optional input basename override. Defaults to Content-Disposition, then final and original URL basenames. Independent of publication naming."`
	SHA256   string            `json:"sha256,omitempty" jsonschema_description:"Optional expected SHA-256 content digest, as 64 lowercase hexadecimal characters."`
	Token    string            `json:"token,omitempty" jsonschema_description:"Optional bearer token. Mutually exclusive with an Authorization header."`
	Headers  map[string]string `json:"headers,omitempty" jsonschema_description:"HTTP request headers, including optional User-Agent and Referer overrides. Credentials and custom headers are confined to the source origin. Only Accept, Accept-Encoding, Accept-Language and User-Agent participate in source identity; other headers are treated as credentials."`
}

type httpObservation struct {
	URL string `json:"url,omitempty"`
}

func (httpConfig) JSONSchemaExtend(schema *jsonschema.Schema) {
	token, _ := schema.Properties.Get("token")
	token.WriteOnly = true
}

func httpInput(input plugin.Input) (httpConfig, error) {
	s, err := configFor[httpConfig](input)
	if err != nil {
		return s, err
	}
	if err := validateContent(s.Filename, s.SHA256); err != nil {
		return s, err
	}
	if err := validateHTTPURL(s.URL); err != nil {
		return s, err
	}
	if s.Match != "" {
		if _, err := regexp.Compile(s.Match); err != nil {
			return s, fmt.Errorf("source match: %w", err)
		}
	}
	if err := validateHeaders(s.Headers); err != nil {
		return s, err
	}
	headers := make(map[string]string, len(s.Headers))
	for name, value := range s.Headers {
		name = http.CanonicalHeaderKey(name)
		if name == "Authorization" && s.Token != "" {
			return s, errors.New("use token or an Authorization header, not both")
		}
		headers[name] = value
	}
	s.Headers = headers
	if s.Token != "" {
		if err := validateHeaders(map[string]string{"Authorization": "Bearer " + s.Token}); err != nil {
			return s, err
		}
	}
	return s, nil
}

func (s httpConfig) sourceIdentity() any {
	s.Token = ""
	headers := make(map[string]string)
	for name, value := range s.Headers {
		switch name {
		case "Accept", "Accept-Encoding", "Accept-Language", "User-Agent":
			headers[name] = value
		}
	}
	s.Headers = headers
	return s
}

func (s httpConfig) headers(address string) map[string]string {
	headers := make(http.Header, len(s.Headers)+1)
	for name, value := range s.Headers {
		headers.Set(name, value)
	}
	if s.Token != "" {
		headers.Set("Authorization", "Bearer "+s.Token)
	}
	origin, _ := url.Parse(s.URL)
	target, _ := url.Parse(address)
	if !sameOrigin(origin, target) {
		stripPrivateHeaders(headers)
	}
	values := make(map[string]string, len(headers))
	for name := range headers {
		values[name] = headers.Get(name)
	}
	return values
}

func (m *Manager) httpResolver() Resolver {
	return resolverFor(httpInput, m.discoverHTTP, acquireHTTP)
}

func (m *Manager) discoverHTTP(ctx context.Context, s httpConfig) (Discovery, error) {
	observed := httpObservation{}
	if s.Match != "" {
		var err error
		observed.URL, err = m.discoverLink(ctx, s)
		if err != nil {
			return Discovery{}, err
		}
	}
	data, err := json.Marshal(observed)
	if err != nil {
		return Discovery{}, err
	}
	if _, err := acquireHTTP(ctx, s, data); err != nil {
		return Discovery{}, err
	}
	found := Discovery{Observation: data, Immutable: s.SHA256 != ""}
	// A response-selected filename needs an initial fetch even with a digest.
	if s.SHA256 != "" && s.Filename != "" {
		found.Content = &Content{SHA256: s.SHA256, Filename: s.Filename, Mode: 0o644}
	}
	return found, nil
}

func acquireHTTP(_ context.Context, s httpConfig, observation json.RawMessage) (Acquisition, error) {
	var observed httpObservation
	if err := decode(observation, &observed); err != nil {
		return Acquisition{}, fmt.Errorf("HTTP observation: %w", err)
	}
	address := s.URL
	if s.Match != "" {
		if err := validateHTTPURL(observed.URL); err != nil {
			return Acquisition{}, fmt.Errorf("matched HTTP observation: %w", err)
		}
		address = observed.URL
	} else if observed.URL != "" {
		return Acquisition{}, errors.New("declared HTTP source must not contain an observed URL")
	}
	return Acquisition{Download: &Download{URL: address, Headers: s.headers(address), Filename: s.Filename, SHA256: s.SHA256}}, nil
}
