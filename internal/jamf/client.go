package jamf

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"strconv"
	"time"

	sdkclient "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/client"
	sdkconfig "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/config"
	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/constants"
	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/packages"
	"go.uber.org/zap"
	"resty.dev/v3"
)

const responseLimit = 8 << 20

type client struct {
	transport *sdkclient.Transport
	packages  *packages.Packages
}

func newClient(ctx context.Context, config Config) (*client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	transport, err := sdkclient.NewTransport(&sdkconfig.AuthConfig{
		InstanceDomain: config.URL, AuthMethod: constants.AuthMethodOAuth2,
		ClientID: config.ClientID, ClientSecret: config.ClientSecret, HideSensitiveData: true,
	}, func(settings *sdkclient.TransportSettings) error {
		settings.Logger = zap.NewNop()
		settings.Timeout = 15 * time.Minute
		settings.HTTPTransport = operationTransport{ctx, config.URL}
		return nil
	})
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, errors.New("jamf authentication failed")
	}
	transport.GetHTTPClient().SetRedirectPolicy(resty.RedirectNoPolicy())
	transport.GetHTTPClient().SetResponseBodyLimit(responseLimit)
	return &client{transport: transport, packages: packages.NewPackages(transport)}, nil
}

// The SDK's separate authentication client does not inherit request contexts.
// Keep token fetches within the operation lifetime and the configured origin.
type operationTransport struct {
	ctx    context.Context
	origin string
}

func (t operationTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	if request.URL.Scheme+"://"+request.URL.Host != t.origin {
		return nil, errors.New("jamf request left the configured origin")
	}
	if request.URL.Path == constants.EndpointOAuthToken {
		request = request.Clone(t.ctx)
	}
	return http.DefaultTransport.RoundTrip(request)
}

func requestError(ctx context.Context, response *resty.Response, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if response != nil && !response.IsStatusSuccess() {
		return fmt.Errorf("jamf HTTP %d", response.StatusCode())
	}
	if err != nil {
		return errors.New("jamf request failed")
	}
	return nil
}

func (c *client) listObjects(ctx context.Context, path string, query map[string]string) ([]json.RawMessage, error) {
	params := maps.Clone(query)
	if params == nil {
		params = map[string]string{}
	}
	params["page-size"], params["sort"] = "100", "id:asc"
	var objects []json.RawMessage
	expected := -1
	seen := map[string]bool{}
	for page := 0; ; page++ {
		params["page"] = strconv.Itoa(page)
		result, data, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationJSON).SetQueryParams(params).GetBytes(path)
		if err := requestError(ctx, result, err); err != nil {
			return nil, err
		}
		var batch struct {
			TotalCount *int               `json:"totalCount"`
			Results    *[]json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(data, &batch); err != nil || batch.TotalCount == nil || *batch.TotalCount < 0 || batch.Results == nil {
			return nil, errors.New("jamf collection response is incomplete")
		}
		if expected >= 0 && expected != *batch.TotalCount {
			return nil, errors.New("jamf collection changed during enumeration")
		}
		expected = *batch.TotalCount
		for _, object := range *batch.Results {
			var row struct {
				ID string `json:"id"`
			}
			if err := json.Unmarshal(object, &row); err != nil || !validID(row.ID) || seen[row.ID] {
				return nil, errors.New("jamf collection IDs are invalid or duplicated")
			}
			seen[row.ID] = true
		}
		objects = append(objects, (*batch.Results)...)
		if len(objects) == expected {
			return objects, nil
		}
		if len(*batch.Results) == 0 || len(objects) > expected {
			return nil, errors.New("jamf collection enumeration is incomplete")
		}
	}
}

func validID(id string) bool {
	n, err := strconv.ParseUint(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatUint(n, 10) == id
}
func raw(value any) json.RawMessage { data, _ := json.Marshal(value); return data }
func stringField(fields map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(fields[key], &value)
	return value
}

func equalJSON(a, b json.RawMessage) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	var left, right bytes.Buffer
	if json.Compact(&left, a) != nil || json.Compact(&right, b) != nil {
		return false
	}
	return bytes.Equal(left.Bytes(), right.Bytes())
}
