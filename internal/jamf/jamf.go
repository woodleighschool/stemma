// Package jamf reconciles immutable packages and their native Jamf deployment links.
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
	URL          string `json:"url" jsonschema:"minLength=1" jsonschema_description:"Jamf Pro server origin, such as https://school.jamfcloud.com. HTTPS is required except loopback test servers. Paths, embedded credentials, queries and fragments are rejected."`
	ClientID     string `json:"client_id" jsonschema:"minLength=1" jsonschema_description:"Jamf API client ID. Use ${VAR} to supply it from the environment."`
	ClientSecret string `json:"client_secret" jsonschema:"minLength=1" jsonschema_description:"Jamf API client secret. Use ${VAR} to supply it from the environment."`
}

type binding struct {
	Server         string              `json:"server"`
	IdentitySHA256 string              `json:"identity_sha256"`
	PackageID      string              `json:"package_id"`
	PayloadSHA256  string              `json:"payload_sha256,omitempty"`
	PendingCreate  bool                `json:"pending_create,omitempty"`
	PendingPayload string              `json:"pending_payload,omitempty"`
	Revisions      map[string]revision `json:"revisions,omitempty"`
	Publications   plugin.Publications `json:"publications,omitzero"`
	Associations   []association       `json:"associations,omitempty"`
	PolicyID       string              `json:"policy_id,omitempty"`
	PolicyTitleID  string              `json:"policy_title_id,omitempty"`
	PolicyName     string              `json:"policy_name,omitempty"`
	PendingPolicy  bool                `json:"pending_policy,omitempty"`
}

type revision struct {
	PackageID string `json:"package_id"`
	Owned     bool   `json:"owned"`
	SHA3512   string `json:"sha3512,omitempty"`
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

// Handle reconciles immutable package revisions before advancing deployment links.
func Handle(ctx context.Context, request plugin.ReconcileRequest) (response plugin.ReconcileResponse, err error) {
	config, metadata, adopt, err := validate(request)
	if err != nil {
		return response, err
	}
	retention, err := decodeRetention(metadata)
	if err != nil {
		return response, err
	}
	delete(metadata, "retention")
	patch, err := decodePatch(metadata)
	if err != nil {
		return response, err
	}
	delete(metadata, "patch")
	if request.Method == "validate" {
		if request.Artifact.Path != "" {
			_, err = inspectPayload(ctx, request.Identity, request.Artifact)
		}
		return response, err
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
	identity := identityDigest(request.Identity)
	state := binding{Server: config.URL, IdentitySHA256: identity}
	if len(request.Binding) > 0 && string(request.Binding) != "null" {
		if err := strictDecode(request.Binding, &state); err != nil {
			return response, fmt.Errorf("jamf binding: %w", err)
		}
		if state.Server != config.URL || state.IdentitySHA256 != identity {
			return response, errors.New("jamf binding does not match this server and logical identity")
		}
	}
	if state.Revisions == nil {
		state.Revisions = make(map[string]revision)
	}
	// Older or recovered bindings identify records but cannot establish creation ownership.
	if state.PackageID != "" && state.PayloadSHA256 != "" {
		if _, ok := state.Revisions[state.PayloadSHA256]; !ok {
			state.Revisions[state.PayloadSHA256] = revision{PackageID: state.PackageID}
		}
	}
	defer func() { response.Binding = raw(state) }()
	rev := state.Revisions[content.sha256]
	rev.SHA3512 = content.sha3512
	if adopt != "" {
		if rev.PackageID != "" && rev.PackageID != adopt {
			return response, errors.New("package_id conflicts with the durable Jamf binding")
		}
		rev.PackageID = adopt
	}
	if state.PendingCreate && state.PendingPayload != content.sha256 {
		return response, errors.New("previous Jamf package creation must be resolved before publishing another payload")
	}
	c, err := newClient(ctx, config)
	if err != nil {
		return response, err
	}
	current, err := c.observe(ctx, rev.PackageID, content.filename)
	if err != nil {
		return response, err
	}
	if adopt != "" && current == nil {
		return response, fmt.Errorf("adopted Jamf package %s does not exist", adopt)
	}
	if current != nil {
		rev.PackageID = stringField(current.Fields, "id")
		state.PendingCreate, state.PendingPayload = false, ""
		state.Revisions[content.sha256] = rev
		// Published package IDs are immutable, including explicitly adopted records.
		if !contentDigestMatches(current, content) && stringField(current.Fields, "cloudTransferStatus") == "READY" {
			return response, errors.New("jamf package content conflicts with this immutable revision")
		}
	}
	state.PackageID = rev.PackageID
	response.Changes = plan(current, metadata, content, request.Artifact.Filename)
	if patch != nil {
		if err := c.checkPatch(ctx, patch, &state, &response); err != nil {
			return response, err
		}
	}
	if request.Method == "plan" {
		if retention.Keep > 0 {
			err = c.prune(ctx, &state, content, retention.Keep, false, &response)
		}
		return response, err
	}
	createdRecord := false
	if current == nil {
		if state.PendingCreate {
			return response, errors.New("previous jamf create outcome is still unresolved; restore visibility or set package_id for explicit adoption")
		}
		state.PendingCreate, state.PendingPayload = true, content.sha256
		created, createErr := c.create(ctx, defaults(content.filename, request.Artifact.Filename))
		if createErr != nil {
			current, err = c.discover(ctx, content.filename)
			if err != nil || current == nil {
				return response, fmt.Errorf("jamf create outcome unresolved; no create retry was sent: %w", createErr)
			}
		} else {
			createdRecord = true
			rev.PackageID, rev.Owned = created, true
			state.Revisions[content.sha256] = rev
			state.PackageID = created
			state.PendingCreate, state.PendingPayload = false, ""
			current, err = c.get(ctx, created)
			if err != nil || current == nil {
				return response, errors.New("created Jamf package could not be read back")
			}
		}
		rev.PackageID = stringField(current.Fields, "id")
		state.Revisions[content.sha256] = rev
		state.PendingCreate, state.PendingPayload = false, ""
	}
	state.PackageID = rev.PackageID
	if !contentMatches(current, content) {
		if stringField(current.Fields, "fileName") != content.filename {
			// Renaming a matching adopted artifact does not replace its content.
			current, err = c.merge(ctx, current, map[string]json.RawMessage{"fileName": raw(content.filename)}, nil)
			if err != nil {
				return response, err
			}
		}
		if !contentDigestMatches(current, content) {
			status := stringField(current.Fields, "cloudTransferStatus")
			if createdRecord || status != "PENDING" && status != "IN_PROGRESS" && status != "UPLOADING" {
				if uploadErr := c.upload(ctx, rev.PackageID, request.Artifact.Path, content); uploadErr != nil {
					current, err = c.get(ctx, rev.PackageID)
					if err != nil || current == nil {
						return response, c.uploadFailure(ctx, &state, content, adopt, nil, fmt.Errorf("jamf upload outcome unresolved; no upload retry was sent: %w", uploadErr))
					}
					status = stringField(current.Fields, "cloudTransferStatus")
					if !contentMatches(current, content) && status != "PENDING" && status != "IN_PROGRESS" && status != "UPLOADING" {
						return response, c.uploadFailure(ctx, &state, content, adopt, current, fmt.Errorf("jamf upload failed or remains unverified; no upload retry was sent: %w", uploadErr))
					}
				}
			}
		}
		current, err = c.awaitContent(ctx, rev.PackageID, content)
		if err != nil {
			return response, c.uploadFailure(ctx, &state, content, adopt, current, err)
		}
	}
	current, err = c.merge(ctx, current, metadata, &content)
	if err != nil {
		return response, err
	}
	if !contentMatches(current, content) {
		return response, errors.New("jamf content changed during metadata reconciliation")
	}
	if patch != nil {
		if err := c.applyPatch(ctx, patch, &state); err != nil {
			return response, err
		}
	}
	state.PayloadSHA256 = content.sha256
	state.Publications.Record(content.sha256)
	if retention.Keep > 0 {
		err = c.prune(ctx, &state, content, retention.Keep, true, &response)
	}
	return response, err
}

func validate(request plugin.ReconcileRequest) (configuration, map[string]json.RawMessage, string, error) {
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
	if config.ClientID == "" || config.ClientSecret == "" {
		return config, nil, "", errors.New("jamf client_id and client_secret are required")
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
		if key == "retention" {
			if _, err := decodeRetention(metadata); err != nil {
				return config, nil, "", err
			}
			continue
		}
		if key == "patch" {
			if _, err := decodePatch(metadata); err != nil {
				return config, nil, "", err
			}
			continue
		}
		rule, ok := managedFields[key]
		if !ok {
			return config, nil, "", fmt.Errorf("unsupported Jamf package metadata %q", key)
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
	if identity.Project == "" || identity.Software == "" || identity.Destination == "" {
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
	return current != nil && stringField(current.Fields, "fileName") == content.filename && contentDigestMatches(current, content)
}

func contentDigestMatches(current *observed, content payload) bool {
	if current == nil || stringField(current.Fields, "cloudTransferStatus") != "READY" {
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

func (c *client) discover(ctx context.Context, filename string) (*observed, error) {
	result, response, err := c.packages.ListV1(ctx, map[string]string{"page-size": "100", "sort": "id:asc", "filter": `fileName=="` + filename + `"`})
	if err := requestError(ctx, response, err); err != nil {
		return nil, err
	}
	if result == nil {
		return nil, errors.New("jamf package discovery returned no result")
	}
	var found string
	for _, entry := range result.Results {
		if entry.FileName != filename {
			continue
		}
		if found != "" {
			return nil, errors.New("multiple Jamf packages have this immutable identity; specify package_id for explicit adoption")
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
	plugin.Stage(ctx, "Uploading Jamf package")
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
	plugin.Stage(ctx, "Waiting for Jamf processing")
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
			return current, fmt.Errorf("jamf package cloud transfer %s", status)
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
