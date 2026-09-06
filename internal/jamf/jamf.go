// Package jamf reconciles package records and content using the Jamf Pro v1 API.
package jamf

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/sha3"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	sdkclient "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/client"
	sdkconfig "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/config"
	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/constants"
	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/packages"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
	"go.uber.org/zap"
	"resty.dev/v3"
)

const packagePath = constants.EndpointJamfProPackagesV1
const responseLimit = 8 << 20

type configuration struct {
	URL             string `json:"url" jsonschema:"minLength=1" jsonschema_description:"Jamf Pro server origin, such as https://school.jamfcloud.com. HTTPS is required except loopback test servers. Paths, embedded credentials, queries and fragments are rejected."`
	ClientIDEnv     string `json:"client_id_env" jsonschema:"minLength=1" jsonschema_description:"Environment variable containing the Jamf API client ID. Credentials are read during plan and apply."`
	ClientSecretEnv string `json:"client_secret_env" jsonschema:"minLength=1" jsonschema_description:"Environment variable containing the Jamf API client secret. Store the variable name here, never the secret itself."`
}

type binding struct {
	Server         string `json:"server"`
	IdentitySHA256 string `json:"identity_sha256"`
	PackageID      string `json:"package_id"`
	PayloadSHA256  string `json:"payload_sha256,omitempty"`
	PendingCreate  bool   `json:"pending_create,omitempty"`
}

type client struct {
	transport *sdkclient.Transport
	packages  *packages.Packages
}

type observed struct {
	Fields map[string]json.RawMessage `json:"fields"`
	ETag   string                     `json:"etag,omitempty"`
}

type payload struct {
	filename string
	prefix   string
	sha256   string
	sha3512  string
	size     int64
}

// Handle validates, plans or applies package-only reconciliation. Metadata uses
// supported native writable names; package_id is an explicit adoption control.
// It never creates, reads or changes policies, scope, assignments or prestages.
func Handle(ctx context.Context, request plugin.Request) (plugin.Response, error) {
	response := plugin.Response{Protocol: plugin.ProtocolVersion}
	if request.Protocol != plugin.ProtocolVersion {
		return response, fmt.Errorf("unsupported Jamf protocol %d", request.Protocol)
	}
	config, metadata, adopt, err := validate(request)
	if err != nil {
		return response, err
	}
	if request.Method == "validate" {
		return response, nil
	}
	if request.Method != "plan" && request.Method != "apply" {
		return response, fmt.Errorf("unsupported Jamf method %q", request.Method)
	}
	content, err := inspectPayload(ctx, request.Identity, request.Artifact)
	if err != nil {
		return response, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	c, err := newClient(ctx, config)
	if err != nil {
		return response, err
	}
	identity := identityDigest(request.Identity)
	state := binding{Server: config.URL, IdentitySHA256: identity}
	if len(request.Binding) > 0 && string(request.Binding) != "null" {
		if err := strictDecode(request.Binding, &state); err != nil {
			return response, fmt.Errorf("jamf binding: %w", err)
		}
		if state.Server != config.URL || state.IdentitySHA256 != identity || !validID(state.PackageID) && (state.PackageID != "" || !state.PendingCreate) {
			return response, errors.New("jamf binding does not match this server and logical identity")
		}
	}
	if adopt != "" {
		if state.PackageID != "" && state.PackageID != adopt {
			return response, errors.New("package_id conflicts with the durable Jamf binding")
		}
		state.PackageID = adopt
	}
	current, err := c.observe(ctx, state.PackageID, content.prefix)
	if err != nil {
		return response, err
	}
	if adopt != "" && current == nil {
		return response, fmt.Errorf("adopted Jamf package %s does not exist", adopt)
	}
	if current != nil {
		state.PackageID = stringField(current.Fields, "id")
		state.PendingCreate = false
	}
	changes := plan(current, metadata, content, request.Artifact.Filename)
	response.Changes = changes
	response.Observation = raw(current)
	response.Binding = raw(state)
	if request.Method == "plan" {
		return response, nil
	}
	createdRecord := false
	if current == nil {
		if state.PendingCreate {
			return response, errors.New("previous jamf create outcome is still unresolved; restore visibility or set package_id for explicit adoption")
		}
		state.PendingCreate = true
		response.Binding = raw(state)
		body := defaults(content.filename, request.Artifact.Filename)
		// Jamf requires a package record before upload. Managed metadata is applied
		// after content verification, so publication cannot activate it early.
		created, createErr := c.create(ctx, body)
		if createErr != nil {
			current, err = c.discover(ctx, content.prefix)
			if err != nil || current == nil {
				return response, fmt.Errorf("jamf create outcome unresolved; no create retry was sent: %w", createErr)
			}
		} else {
			createdRecord = true
			state.PackageID, state.PendingCreate = created, false
			response.Binding = raw(state)
			current, err = c.get(ctx, created)
			if err != nil || current == nil {
				return response, errors.New("created Jamf package could not be read back")
			}
		}
		state.PackageID = stringField(current.Fields, "id")
		state.PendingCreate = false
		response.Binding = raw(state)
	}
	if !contentMatches(current, content) {
		if stringField(current.Fields, "fileName") != content.filename {
			current, err = c.merge(ctx, current, map[string]json.RawMessage{"fileName": raw(content.filename), "md5": raw(nil), "sha256": raw(nil), "sha3512": raw(nil), "hashType": raw(nil), "hashValue": raw(nil)}, nil)
			if err != nil {
				return response, err
			}
		}
		status := stringField(current.Fields, "cloudTransferStatus")
		if createdRecord || status != "PENDING" && status != "IN_PROGRESS" && status != "UPLOADING" {
			uploadErr := c.upload(ctx, state.PackageID, request.Artifact.Path, content)
			if uploadErr != nil {
				current, err = c.get(ctx, state.PackageID)
				if err != nil || current == nil {
					return response, fmt.Errorf("jamf upload outcome unresolved; no upload retry was sent: %w", uploadErr)
				}
				status = stringField(current.Fields, "cloudTransferStatus")
				if !contentMatches(current, content) && status != "PENDING" && status != "IN_PROGRESS" && status != "UPLOADING" {
					return response, fmt.Errorf("jamf upload failed or remains unverified; no upload retry was sent: %w", uploadErr)
				}
			}
		}
		current, err = c.awaitContent(ctx, state.PackageID, content)
		if err != nil {
			return response, err
		}
	}
	current, err = c.merge(ctx, current, metadata, &content)
	if err != nil {
		return response, err
	}
	if !contentMatches(current, content) {
		return response, errors.New("jamf content changed during metadata reconciliation")
	}
	state.PayloadSHA256 = content.sha256
	response.Binding = raw(state)
	response.Observation = raw(current)
	return response, nil
}

func validate(request plugin.Request) (configuration, map[string]json.RawMessage, string, error) {
	var config configuration
	if err := strictDecode(request.Config, &config); err != nil {
		return config, nil, "", fmt.Errorf("jamf config: %w", err)
	}
	u, err := url.Parse(config.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return config, nil, "", errors.New("jamf url must be a server origin without credentials, path, query or fragment")
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return config, nil, "", errors.New("jamf url must use HTTPS (HTTP is allowed only for loopback test servers)")
	}
	config.URL = strings.TrimRight(config.URL, "/")
	if config.ClientIDEnv == "" || config.ClientSecretEnv == "" {
		return config, nil, "", errors.New("jamf client_id_env and client_secret_env are required")
	}
	metadata, err := decodeObject(request.Metadata)
	if err != nil {
		return config, nil, "", fmt.Errorf("jamf metadata: %w", err)
	}
	adopt := ""
	if value, exists := metadata["package_id"]; exists {
		if err := json.Unmarshal(value, &adopt); err != nil || !validID(adopt) {
			return config, nil, "", errors.New("package_id must be a positive numeric string")
		}
		delete(metadata, "package_id")
	}
	for key, value := range metadata {
		rule, ok := managedFields[key]
		if !ok {
			return config, nil, "", fmt.Errorf("unsupported Jamf package metadata %q; policies and scope are not supported", key)
		}
		if err := rule.validate(value); err != nil {
			return config, nil, "", fmt.Errorf("jamf %s: %w", key, err)
		}
	}
	return config, metadata, adopt, nil
}

type fieldRule struct {
	kind        string
	nullable    bool
	description string
}

func (r fieldRule) validate(value json.RawMessage) error {
	if bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		if r.nullable {
			return nil
		}
		return errors.New("null is not allowed")
	}
	var target any
	switch r.kind {
	case "string":
		target = new(string)
	case "bool":
		target = new(bool)
	case "int":
		target = new(int64)
	}
	if err := json.Unmarshal(value, target); err != nil {
		return fmt.Errorf("expected %s", r.kind)
	}
	return nil
}

var managedFields = map[string]fieldRule{
	"packageName":          {"string", false, "Native package display name. Defaults to the artifact filename when creating a package; names are never used for identity matching."},
	"categoryId":           {"string", false, "Native category ID as a string. Use -1 for no category."},
	"info":                 {"string", true, "Native package information. Null clears the field."},
	"notes":                {"string", true, "Administrator notes. Null clears the field."},
	"priority":             {"int", false, "Native package installation priority. Defaults to 10 when creating a package."},
	"osRequirements":       {"string", true, "Native operating system requirement expression, such as 10.6.8, 10.7.x. Null clears the field."},
	"fillUserTemplate":     {"bool", false, "Native option to fill the user template. Explicit false is managed."},
	"fillExistingUsers":    {"bool", false, "Native option to fill existing user directories. Explicit false is managed."},
	"rebootRequired":       {"bool", false, "Native package restart requirement. Explicit false is managed."},
	"osInstall":            {"bool", false, "Native operating system installer flag. Explicit false is managed."},
	"suppressUpdates":      {"bool", false, "Native suppressUpdates package option. Explicit false is managed."},
	"suppressFromDock":     {"bool", false, "Native suppressFromDock package option. Explicit false is managed."},
	"suppressEula":         {"bool", false, "Native suppressEula package option. Explicit false is managed."},
	"suppressRegistration": {"bool", false, "Native suppressRegistration package option. Explicit false is managed."},
}

var readOnlyFields = []string{"id", "indexed", "cloudTransferStatus", "size"}

func defaults(filename, display string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"fileName": raw(filename), "packageName": raw(display), "categoryId": raw("-1"), "priority": raw(10), "fillUserTemplate": raw(false), "osInstall": raw(false), "rebootRequired": raw(false), "suppressEula": raw(false), "suppressFromDock": raw(false), "suppressRegistration": raw(false), "suppressUpdates": raw(false)}
}

func plan(current *observed, metadata map[string]json.RawMessage, content payload, display string) []plugin.Change {
	var changes []plugin.Change
	if current == nil {
		changes = append(changes, plugin.Change{Kind: "content", Field: "package", Action: "create", After: raw(content.filename)})
		current = &observed{Fields: defaults(content.filename, display)}
	} else if !contentMatches(current, content) {
		changes = append(changes, plugin.Change{Kind: "content", Field: "fileName", Action: "upload", Before: current.Fields["fileName"], After: raw(content.filename)})
	}
	keys := make([]string, 0, len(metadata))
	for key := range metadata {
		keys = append(keys, key)
	}
	slices.Sort(keys)
	for _, key := range keys {
		if !equalJSON(current.Fields[key], metadata[key]) {
			changes = append(changes, plugin.Change{Kind: "metadata", Field: key, Action: "set", Before: current.Fields[key], After: metadata[key]})
		}
	}
	return changes
}

func inspectPayload(ctx context.Context, identity plugin.Identity, artifact plugin.Artifact) (payload, error) {
	if identity.Project == "" || identity.Recipe == "" || identity.Destination == "" {
		return payload{}, errors.New("jamf requires a complete logical identity")
	}
	if strings.ToLower(filepath.Ext(artifact.Filename)) != ".pkg" {
		return payload{}, errors.New("jamf package adapter currently requires an original .pkg file")
	}
	f, err := os.Open(artifact.Path)
	if err != nil {
		return payload{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return payload{}, err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 {
		return payload{}, errors.New("jamf artifact must be a nonempty regular file")
	}
	h, hash3 := sha256.New(), sha3.New512()
	n, err := io.Copy(io.MultiWriter(h, hash3), fileio.Reader{Context: ctx, Reader: f})
	if err != nil {
		return payload{}, err
	}
	digest := hex.EncodeToString(h.Sum(nil))
	if digest != artifact.SHA256 || n != artifact.Size {
		return payload{}, errors.New("jamf artifact bytes disagree with the immutable artifact descriptor")
	}
	prefix := "stemma-" + identityDigest(identity) + "-"
	return payload{filename: prefix + digest + ".pkg", prefix: prefix, sha256: digest, sha3512: hex.EncodeToString(hash3.Sum(nil)), size: n}, nil
}

func identityDigest(identity plugin.Identity) string {
	sum := sha256.Sum256(raw(identity))
	return hex.EncodeToString(sum[:])
}

func contentMatches(current *observed, content payload) bool {
	if current == nil || stringField(current.Fields, "fileName") != content.filename || stringField(current.Fields, "cloudTransferStatus") != "READY" {
		return false
	}
	if digest := stringField(current.Fields, "sha3512"); digest != "" {
		return strings.EqualFold(digest, content.sha3512)
	}
	switch stringField(current.Fields, "hashType") {
	case "SHA3_512", "SHA3-512":
		return strings.EqualFold(stringField(current.Fields, "hashValue"), content.sha3512)
	case "SHA_256", "SHA256", "SHA-256":
		return strings.EqualFold(stringField(current.Fields, "hashValue"), content.sha256)
	}
	return strings.EqualFold(stringField(current.Fields, "sha256"), content.sha256)
}

func newClient(ctx context.Context, config configuration) (*client, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	id, secret := os.Getenv(config.ClientIDEnv), os.Getenv(config.ClientSecretEnv)
	if id == "" || secret == "" {
		return nil, errors.New("jamf client credential environment variables are unset")
	}
	transport, err := sdkclient.NewTransport(&sdkconfig.AuthConfig{
		InstanceDomain: config.URL, AuthMethod: constants.AuthMethodOAuth2,
		ClientID: id, ClientSecret: secret, HideSensitiveData: true,
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

func (c *client) get(ctx context.Context, id string) (*observed, error) {
	if !validID(id) {
		return nil, errors.New("invalid Jamf package ID")
	}
	// Typed package models cannot retain unknown fields or distinguish null
	// from empty strings when merging a full-object update.
	response, data, err := c.transport.NewRequest(ctx).
		SetHeader("Accept", constants.ApplicationJSON).
		GetBytes(packagePath + "/" + id)
	if response != nil && response.StatusCode() == http.StatusNotFound {
		return nil, nil
	}
	if err := requestError(ctx, response, err); err != nil {
		return nil, err
	}
	fields, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	if stringField(fields, "id") != id {
		return nil, errors.New("jamf response ID does not match requested package")
	}
	return &observed{Fields: fields, ETag: response.Header().Get("ETag")}, nil
}

func (c *client) observe(ctx context.Context, id, prefix string) (*observed, error) {
	if id != "" {
		current, err := c.get(ctx, id)
		if err != nil || current != nil {
			return current, err
		}
	}
	return c.discover(ctx, prefix)
}

func (c *client) discover(ctx context.Context, prefix string) (*observed, error) {
	result, response, err := c.packages.ListV1(ctx, map[string]string{
		"page-size": "100", "sort": "id:asc", "filter": `fileName=="` + prefix + `*"`,
	})
	if err := requestError(ctx, response, err); err != nil {
		return nil, err
	}
	var found string
	for _, entry := range result.Results {
		if !strings.HasPrefix(entry.FileName, prefix) || !strings.HasSuffix(entry.FileName, ".pkg") {
			continue
		}
		digest := strings.TrimSuffix(strings.TrimPrefix(entry.FileName, prefix), ".pkg")
		if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size {
			continue
		}
		if found != "" {
			return nil, errors.New("multiple Jamf packages have this logical identity; specify package_id for explicit adoption")
		}
		if !validID(entry.ID) {
			return nil, errors.New("jamf discovery returned an invalid package ID")
		}
		found = entry.ID
	}
	if found == "" {
		return nil, nil
	}
	return c.get(ctx, found)
}

func (c *client) create(ctx context.Context, fields map[string]json.RawMessage) (string, error) {
	var body packages.RequestPackage
	if err := json.Unmarshal(raw(fields), &body); err != nil {
		return "", err
	}
	// CreateV1 performs a distribution-point preflight inside the create call,
	// which would make a failed read indistinguishable from an ambiguous write.
	var result packages.CreateResponse
	response, err := c.transport.NewRequest(ctx).
		SetHeader("Accept", constants.ApplicationJSON).
		SetHeader("Content-Type", constants.ApplicationJSON).
		SetBody(&body).
		SetResult(&result).
		Post(packagePath)
	if err := requestError(ctx, response, err); err != nil {
		return "", err
	}
	if !validID(result.ID) {
		return "", errors.New("jamf create response has no valid package ID")
	}
	return result.ID, nil
}

func (c *client) merge(ctx context.Context, current *observed, changes map[string]json.RawMessage, expected *payload) (*observed, error) {
	id := stringField(current.Fields, "id")
	fresh, err := c.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		return nil, errors.New("jamf package disappeared before metadata update")
	}
	if expected != nil && !contentMatches(fresh, *expected) {
		return nil, errors.New("jamf content changed before managed metadata update")
	}
	changed := false
	for key, value := range changes {
		if !equalJSON(fresh.Fields[key], value) {
			changed = true
		}
	}
	if !changed {
		return fresh, nil
	}
	body := maps.Clone(fresh.Fields)
	for _, key := range readOnlyFields {
		delete(body, key)
	}
	maps.Copy(body, changes)
	response, putErr := c.transport.NewRequest(ctx).
		SetHeader("Accept", constants.ApplicationJSON).
		SetHeader("Content-Type", constants.ApplicationJSON).
		SetHeader("If-Match", fresh.ETag).
		SetBody(body).
		DisableRetry().
		Put(packagePath + "/" + id)
	putErr = requestError(ctx, response, putErr)
	readback, err := c.get(ctx, id)
	if err != nil || readback == nil {
		return nil, errors.New("jamf metadata update could not be read back")
	}
	for key, value := range changes {
		if !equalJSON(readback.Fields[key], value) {
			if putErr != nil {
				return nil, fmt.Errorf("jamf metadata update was not observed: %w", putErr)
			}
			return nil, fmt.Errorf("jamf metadata %s did not match readback", key)
		}
	}
	return readback, nil
}

func (c *client) upload(ctx context.Context, id, file string, content payload) error {
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// UploadV1 uses the local basename; Stemma's identity marker is the remote filename.
	response, err := c.transport.NewRequest(ctx).
		SetHeader("Accept", constants.ApplicationJSON).
		SetMultipartFile("file", content.filename, f, content.size, nil).
		Post(packagePath + "/" + id + "/upload")
	return requestError(ctx, response, err)
}

func (c *client) awaitContent(ctx context.Context, id string, content payload) (*observed, error) {
	for attempt := range 31 {
		current, err := c.get(ctx, id)
		if err != nil {
			return nil, err
		}
		if current == nil {
			return nil, errors.New("jamf package disappeared during upload verification")
		}
		if contentMatches(current, content) {
			return current, nil
		}
		status := stringField(current.Fields, "cloudTransferStatus")
		if status == "FAILED" || status == "ERROR" {
			return nil, fmt.Errorf("jamf package cloud transfer %s", status)
		}
		if attempt == 30 {
			break
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, errors.New("jamf content is not READY with a matching SHA-256 or SHA3-512 after 60 seconds; binding retained for reconciliation")
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

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}

func decodeObject(data []byte) (map[string]json.RawMessage, error) {
	if len(data) == 0 {
		return map[string]json.RawMessage{}, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	token, err := decoder.Token()
	if err != nil || token != json.Delim('{') {
		return nil, errors.New("expected a JSON object")
	}
	fields := make(map[string]json.RawMessage)
	for decoder.More() {
		key, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := key.(string)
		if !ok {
			return nil, errors.New("expected a JSON field name")
		}
		if _, exists := fields[name]; exists {
			return nil, fmt.Errorf("duplicate JSON field %q", name)
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		fields[name] = value
	}
	if _, err := decoder.Token(); err != nil {
		return nil, err
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing JSON data")
	}
	return fields, nil
}
