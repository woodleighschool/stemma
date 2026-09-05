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
	"os"
	"strings"
	"time"
)

type configuration struct {
	GraphURL        string `json:"graph_url,omitempty"`
	TokenEnv        string `json:"token_env,omitempty"`
	TenantIDEnv     string `json:"tenant_id_env,omitempty"`
	ClientIDEnv     string `json:"client_id_env,omitempty"`
	ClientSecretEnv string `json:"client_secret_env,omitempty"`
	AppID           string `json:"-"`
}

type client struct {
	base         string
	appType      string
	token        string
	http         *http.Client
	pollInterval time.Duration
}

type httpError struct{ status int }

func (e *httpError) Error() string { return fmt.Sprintf("Intune HTTP status %d", e.status) }

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
		(base.Scheme != "https" && (base.Scheme != "http" || (base.Hostname() != "127.0.0.1" && base.Hostname() != "localhost"))) {
		return cfg, errors.New("graph_url must be HTTPS without user info, query or fragment")
	}
	if !strings.HasSuffix(strings.TrimRight(base.Path, "/"), "/v1.0") {
		return cfg, errors.New("graph_url must end in /v1.0; macOSPkgApp selects /beta automatically")
	}
	if cfg.TokenEnv == "" && (cfg.TenantIDEnv == "" || cfg.ClientIDEnv == "" || cfg.ClientSecretEnv == "") {
		return cfg, errors.New("set token_env or all client credential environment names")
	}
	if cfg.TokenEnv != "" && (cfg.TenantIDEnv != "" || cfg.ClientIDEnv != "" || cfg.ClientSecretEnv != "") {
		return cfg, errors.New("choose one Intune authentication method")
	}
	return cfg, nil
}

func newClient(ctx context.Context, cfg configuration) (*client, error) {
	c := &client{base: strings.TrimRight(cfg.GraphURL, "/"), pollInterval: 5 * time.Second, http: &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	if cfg.TokenEnv != "" {
		c.token = os.Getenv(cfg.TokenEnv)
		if c.token == "" {
			return nil, fmt.Errorf("credential environment variable %s is empty", cfg.TokenEnv)
		}
		return c, nil
	}
	tenant, clientID, secret := os.Getenv(cfg.TenantIDEnv), os.Getenv(cfg.ClientIDEnv), os.Getenv(cfg.ClientSecretEnv)
	if tenant == "" || clientID == "" || secret == "" {
		return nil, errors.New("intune client credential environment variables are empty")
	}
	form := url.Values{"client_id": {clientID}, "client_secret": {secret}, "scope": {"https://graph.microsoft.com/.default"}, "grant_type": {"client_credentials"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "https://login.microsoftonline.com/"+url.PathEscape(tenant)+"/oauth2/v2.0/token", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	response, err := c.http.Do(req)
	if err != nil {
		return nil, errors.New("intune token request failed")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		return nil, &httpError{response.StatusCode}
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(response.Body, 1<<20)).Decode(&token); err != nil || token.AccessToken == "" {
		return nil, errors.New("invalid Intune token response")
	}
	c.token = token.AccessToken
	return c, nil
}

func (c *client) request(ctx context.Context, method, path string, body any, result any) error {
	endpoint := c.base + path
	if strings.HasPrefix(path, "https://") || strings.HasPrefix(path, "http://") {
		if !strings.HasPrefix(path, c.base+"/") {
			return errors.New("graph pagination escaped configured API endpoint")
		}
		endpoint = path
	}
	var input io.Reader
	if body != nil {
		input = bytes.NewReader(raw(body))
	}
	req, err := http.NewRequestWithContext(ctx, method, endpoint, input)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("intune request failed; remote outcome may be uncertain")
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return &httpError{response.StatusCode}
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if err != nil {
		return errors.New("intune response interrupted; remote outcome may be uncertain")
	}
	if len(data) > 8<<20 {
		return errors.New("intune response exceeds 8 MiB")
	}
	if result != nil && len(bytes.TrimSpace(data)) > 0 {
		return json.Unmarshal(data, result)
	}
	return nil
}

func (c *client) list(ctx context.Context, path string) ([]object, error) {
	var result []object
	for range 100 {
		var page struct {
			Value []object `json:"value"`
			Next  string   `json:"@odata.nextLink"`
		}
		if err := c.request(ctx, http.MethodGet, path, nil, &page); err != nil {
			return nil, err
		}
		result = append(result, page.Value...)
		if page.Next == "" {
			return result, nil
		}
		path = page.Next
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
