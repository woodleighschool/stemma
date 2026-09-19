package intune

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	abs "github.com/microsoft/kiota-abstractions-go"
	"github.com/microsoft/kiota-abstractions-go/authentication"
	khttp "github.com/microsoft/kiota-http-go"
	kjson "github.com/microsoft/kiota-serialization-json-go"
	core "github.com/microsoftgraph/msgraph-sdk-go-core"
	graphauth "github.com/microsoftgraph/msgraph-sdk-go-core/authentication"
	betadam "github.com/woodleighschool/stemma/internal/intune/graph/beta/deviceappmanagement"
	dam "github.com/woodleighschool/stemma/internal/intune/graph/stable/deviceappmanagement"
)

type configuration struct {
	GraphURL     string `json:"graph_url,omitempty"`
	Token        string `json:"token,omitempty"`
	TenantID     string `json:"tenant_id,omitempty"`
	ClientID     string `json:"client_id,omitempty"`
	ClientSecret string `json:"client_secret,omitempty"`
}

type client struct {
	stable       *dam.DeviceAppManagementRequestBuilder
	beta         *betadam.DeviceAppManagementRequestBuilder
	appType      string
	http         *http.Client
	pollInterval time.Duration
}

func parseConfiguration(data []byte) (configuration, error) {
	var cfg configuration
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&cfg); err != nil {
		return cfg, err
	}
	if err := decoder.Decode(new(any)); !errors.Is(err, io.EOF) {
		return cfg, errors.New("expected one connection object")
	}
	if cfg.GraphURL == "" {
		cfg.GraphURL = "https://graph.microsoft.com/v1.0"
	}
	base, err := url.Parse(cfg.GraphURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		base.Scheme != "https" {
		return cfg, errors.New("graph_url must be HTTPS without user info, query or fragment")
	}
	if !strings.HasSuffix(strings.TrimRight(base.Path, "/"), "/v1.0") {
		return cfg, errors.New("graph_url must end in /v1.0; macOS apps select /beta automatically")
	}
	if cfg.Token == "" && (cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "") {
		return cfg, errors.New("set token or tenant_id, client_id and client_secret")
	}
	if cfg.Token != "" && (cfg.TenantID != "" || cfg.ClientID != "" || cfg.ClientSecret != "") {
		return cfg, errors.New("choose one Intune authentication method")
	}
	return cfg, nil
}

func newClient(cfg configuration) (*client, error) {
	endpoint, _ := url.Parse(cfg.GraphURL)
	hosts := []string{endpoint.Hostname()}
	var auth authentication.AuthenticationProvider
	var err error
	if cfg.Token != "" {
		auth, err = authentication.NewApiKeyAuthenticationProviderWithValidHosts("Bearer "+cfg.Token, "Authorization", authentication.HEADER_KEYLOCATION, hosts)
	} else {
		credential, credentialErr := azidentity.NewClientSecretCredential(cfg.TenantID, cfg.ClientID, cfg.ClientSecret, nil)
		if credentialErr != nil {
			return nil, errors.New("cannot create Intune client credential")
		}
		auth, err = graphauth.NewAzureIdentityAuthenticationProviderWithScopesAndValidHosts(credential, []string{"https://graph.microsoft.com/.default"}, hosts)
	}
	if err != nil {
		return nil, errors.New("cannot configure Intune authentication")
	}
	return newSDKClient(cfg.GraphURL, auth, nil)
}

func newSDKClient(base string, auth authentication.AuthenticationProvider, transport http.RoundTripper) (*client, error) {
	middleware, err := khttp.GetDefaultMiddlewaresWithOptions(
		khttp.NewCompressionOptionsReference(false),
		&khttp.RetryHandlerOptions{ShouldRetry: func(_ time.Duration, _ int, req *http.Request, response *http.Response) bool {
			// Creation is not idempotent: only a definite throttle may repeat a POST.
			return req.Method != http.MethodPost || response.StatusCode == http.StatusTooManyRequests
		}},
		&khttp.RedirectHandlerOptions{ShouldRedirect: func(*http.Request, *http.Response) bool { return false }},
	)
	if err != nil {
		return nil, err
	}
	graphHTTP := khttp.GetDefaultClient(middleware...)
	if transport != nil {
		graphHTTP.Transport = khttp.NewCustomTransportWithParentTransport(transport, middleware...)
	}
	graphHTTP.Transport = boundedTransport{graphHTTP.Transport}
	graphHTTP.Timeout = 2 * time.Minute
	base = strings.TrimRight(base, "/")
	betaURL := strings.TrimSuffix(base, "/v1.0") + "/beta"
	newAdapter := func(endpoint string) (*core.GraphRequestAdapterBase, error) {
		adapter, err := core.NewGraphRequestAdapterBaseWithParseNodeFactoryAndSerializationWriterFactoryAndHttpClient(
			auth, core.GraphClientOptions{}, kjson.NewJsonParseNodeFactory(), kjson.NewJsonSerializationWriterFactory(), graphHTTP,
		)
		if err != nil {
			return nil, err
		}
		adapter.SetBaseUrl(endpoint)
		return adapter, nil
	}
	adapter, err := newAdapter(base)
	if err != nil {
		return nil, err
	}
	betaAdapter, err := newAdapter(betaURL)
	if err != nil {
		return nil, err
	}

	return &client{
		stable:       dam.NewDeviceAppManagementRequestBuilderInternal(map[string]string{"baseurl": base}, adapter),
		beta:         betadam.NewDeviceAppManagementRequestBuilderInternal(map[string]string{"baseurl": betaURL}, betaAdapter),
		http:         &http.Client{Transport: transport, Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }},
		pollInterval: 5 * time.Second,
	}, nil
}

// Native metadata uses Kiota's raw content surface so explicit nulls, empty
// collections and unknown remote fields survive without a second model schema.
func (c *client) request(ctx context.Context, method abs.HttpMethod, builder *abs.BaseRequestBuilder, body any, result any) error {
	info := abs.NewRequestInformationWithMethodAndUrlTemplateAndPathParameters(method, builder.UrlTemplate, builder.PathParameters)
	info.Headers.Add("Accept", "application/json")
	if body != nil {
		info.SetStreamContentAndContentType(raw(body), "application/json")
	}
	data, err := builder.RequestAdapter.SendPrimitive(ctx, info, "[]byte", nil)
	if err != nil {
		return graphError(ctx, err)
	}
	if result != nil && data != nil {
		return json.Unmarshal(data.([]byte), result)
	}
	return nil
}

type boundedTransport struct{ http.RoundTripper }

func (t boundedTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	response, err := t.RoundTripper.RoundTrip(request)
	if err == nil {
		response.Body = http.MaxBytesReader(nil, response.Body, 8<<20)
	}
	return response, err
}

// errNotFound lets a caller tell a missing resource from a failed request.
var errNotFound = errors.New("intune HTTP status 404")

func graphError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if _, ok := errors.AsType[*http.MaxBytesError](err); ok {
		return errors.New("intune response exceeds 8 MiB")
	}
	var apiErr interface{ GetStatusCode() int }
	if errors.As(err, &apiErr) && apiErr.GetStatusCode() != 0 {
		if apiErr.GetStatusCode() == http.StatusNotFound {
			return errNotFound
		}
		return fmt.Errorf("intune HTTP status %d", apiErr.GetStatusCode())
	}
	return errors.New("intune request failed; remote outcome may be uncertain")
}

func (c *client) list(ctx context.Context, builder *abs.BaseRequestBuilder) ([]object, error) {
	var result []object
	first := abs.NewRequestInformationWithMethodAndUrlTemplateAndPathParameters(abs.GET, builder.UrlTemplate, builder.PathParameters)
	endpoint, err := first.GetUri()
	if err != nil {
		return nil, err
	}
	base := builder.PathParameters["baseurl"]
	for range 100 {
		var page struct {
			Value []object `json:"value"`
			Next  string   `json:"@odata.nextLink"`
		}
		if err := c.request(ctx, abs.GET, builder, nil, &page); err != nil {
			return nil, err
		}
		result = append(result, page.Value...)
		if page.Next == "" {
			return result, nil
		}
		next, err := url.Parse(page.Next)
		if err != nil || next.Scheme != endpoint.Scheme || next.Host != endpoint.Host || !strings.HasPrefix(page.Next, base+"/") {
			return nil, errors.New("graph pagination escaped configured API endpoint")
		}
		builder = abs.NewBaseRequestBuilder(builder.RequestAdapter, builder.UrlTemplate, map[string]string{"request-raw-url": page.Next})
	}
	return nil, errors.New("intune pagination exceeds 100 pages")
}

func (c *client) pause(ctx context.Context) error {
	timer := time.NewTimer(c.pollInterval)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}
