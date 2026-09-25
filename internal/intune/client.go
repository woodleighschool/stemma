package intune

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
)

type Config struct {
	GraphURL     string `json:"graph_url,omitempty" jsonschema:"default=https://graph.microsoft.com" jsonschema_description:"Graph service origin, such as https://graph.microsoft.us for a national cloud. HTTPS is required; app management uses the beta API."`
	Token        string `json:"token,omitempty" jsonschema:"minLength=1,writeOnly=true" jsonschema_description:"Bearer token. Choose this or all three client credentials; supply secrets through environment references."`
	TenantID     string `json:"tenant_id,omitempty" jsonschema:"minLength=1" jsonschema_description:"Microsoft Entra tenant ID for client-credential authentication."`
	ClientID     string `json:"client_id,omitempty" jsonschema:"minLength=1" jsonschema_description:"App registration client ID for client-credential authentication."`
	ClientSecret string `json:"client_secret,omitempty" jsonschema:"minLength=1,writeOnly=true" jsonschema_description:"App registration secret. Supply it through an environment reference."`
}

type client struct {
	beta         *betadam.DeviceAppManagementRequestBuilder
	appType      string
	http         *http.Client
	pollInterval time.Duration
}

// Validate checks the Graph origin and exclusive authentication methods.
func (cfg Config) Validate() error {
	base, err := url.Parse(cfg.GraphURL)
	if err != nil || base.Host == "" || base.User != nil || base.RawQuery != "" || base.Fragment != "" ||
		base.Scheme != "https" || strings.Trim(base.Path, "/") != "" {
		return errors.New("graph_url must be an HTTPS origin without path, user info, query or fragment")
	}
	if cfg.Token == "" && (cfg.TenantID == "" || cfg.ClientID == "" || cfg.ClientSecret == "") {
		return errors.New("set token or tenant_id, client_id and client_secret")
	}
	if cfg.Token != "" && (cfg.TenantID != "" || cfg.ClientID != "" || cfg.ClientSecret != "") {
		return errors.New("choose one Intune authentication method")
	}
	return nil
}

func newClient(cfg Config) (*client, error) {
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

func newSDKClient(origin string, auth authentication.AuthenticationProvider, transport http.RoundTripper) (*client, error) {
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
	betaURL := strings.TrimRight(origin, "/") + "/beta"
	adapter, err := core.NewGraphRequestAdapterBaseWithParseNodeFactoryAndSerializationWriterFactoryAndHttpClient(
		auth, core.GraphClientOptions{}, kjson.NewJsonParseNodeFactory(), kjson.NewJsonSerializationWriterFactory(), graphHTTP,
	)
	if err != nil {
		return nil, err
	}
	adapter.SetBaseUrl(betaURL)

	return &client{
		beta:         betadam.NewDeviceAppManagementRequestBuilderInternal(map[string]string{"baseurl": betaURL}, adapter),
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
