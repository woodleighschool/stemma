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
	"mime/multipart"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/plugin"
)

const packagePath = "/api/v1/packages"
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
	http   *http.Client
	config configuration
	token  string
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
	c := &client{config: config, http: &http.Client{Timeout: 15 * time.Minute, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	if err := c.authenticate(ctx); err != nil {
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

var writableFields = []string{"packageName", "fileName", "categoryId", "info", "notes", "priority", "osRequirements", "fillUserTemplate", "fillExistingUsers", "swu", "rebootRequired", "selfHealNotify", "selfHealingAction", "osInstall", "serialNumber", "parentPackageId", "basePath", "suppressUpdates", "ignoreConflicts", "suppressFromDock", "suppressEula", "suppressRegistration", "installLanguage", "md5", "sha256", "sha3512", "hashType", "hashValue", "osInstallerVersion", "manifest", "manifestFileName", "format"}
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

func (c *client) authenticate(ctx context.Context) error {
	id, secret := os.Getenv(c.config.ClientIDEnv), os.Getenv(c.config.ClientSecretEnv)
	if id == "" || secret == "" {
		return errors.New("jamf client credential environment variables are unset")
	}
	form := url.Values{"grant_type": {"client_credentials"}, "client_id": {id}, "client_secret": {secret}}
	data, _, err := c.call(ctx, http.MethodPost, "/api/v1/oauth/token", "application/x-www-form-urlencoded", strings.NewReader(form.Encode()), nil)
	if err != nil {
		return fmt.Errorf("jamf authentication: %w", err)
	}
	var token struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(data, &token); err != nil || token.AccessToken == "" {
		return errors.New("jamf returned an invalid access token response")
	}
	c.token = token.AccessToken
	return nil
}

type httpError struct{ status int }

func (e *httpError) Error() string { return fmt.Sprintf("jamf HTTP %d", e.status) }

func (c *client) call(ctx context.Context, method, path, contentType string, body io.Reader, headers http.Header) ([]byte, http.Header, error) {
	request, err := http.NewRequestWithContext(ctx, method, c.config.URL+path, body)
	if err != nil {
		return nil, nil, err
	}
	request.Header = headers.Clone()
	if request.Header == nil {
		request.Header = make(http.Header)
	}
	request.Header.Set("Accept", "application/json")
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if c.token != "" {
		request.Header.Set("Authorization", "Bearer "+c.token)
	}
	if length := request.Header.Get("Content-Length"); length != "" {
		request.ContentLength, _ = strconv.ParseInt(length, 10, 64)
		request.Header.Del("Content-Length")
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errors.New("jamf request transport failed")
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		return nil, response.Header, errors.New("jamf response read failed")
	}
	if len(data) > responseLimit {
		return nil, response.Header, errors.New("jamf response exceeds 8 MiB")
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return nil, response.Header, &httpError{response.StatusCode}
	}
	return data, response.Header, nil
}

func (c *client) get(ctx context.Context, id string) (*observed, error) {
	if !validID(id) {
		return nil, errors.New("invalid Jamf package ID")
	}
	data, headers, err := c.call(ctx, http.MethodGet, packagePath+"/"+id, "", nil, nil)
	var status *httpError
	if errors.As(err, &status) && status.status == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	fields, err := decodeObject(data)
	if err != nil {
		return nil, err
	}
	if stringField(fields, "id") != id {
		return nil, errors.New("jamf response ID does not match requested package")
	}
	return &observed{Fields: fields, ETag: headers.Get("ETag")}, nil
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
	var found *observed
	for page := range 100 {
		query := url.Values{"page": {strconv.Itoa(page)}, "page-size": {"100"}, "sort": {"id:asc"}, "filter": {`fileName=="` + prefix + `*"`}}
		data, _, err := c.call(ctx, http.MethodGet, packagePath+"?"+query.Encode(), "", nil, nil)
		if err != nil {
			return nil, err
		}
		var result struct {
			TotalCount int               `json:"totalCount"`
			Results    []json.RawMessage `json:"results"`
		}
		if err := json.Unmarshal(data, &result); err != nil {
			return nil, err
		}
		for _, entry := range result.Results {
			fields, err := decodeObject(entry)
			if err != nil {
				return nil, err
			}
			name := stringField(fields, "fileName")
			if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".pkg") {
				continue
			}
			digest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".pkg")
			if decoded, err := hex.DecodeString(digest); err != nil || len(decoded) != sha256.Size {
				continue
			}
			if found != nil {
				return nil, errors.New("multiple Jamf packages have this logical identity; specify package_id for explicit adoption")
			}
			id := stringField(fields, "id")
			if !validID(id) {
				return nil, errors.New("jamf discovery returned an invalid package ID")
			}
			found, err = c.get(ctx, id)
			if err != nil {
				return nil, err
			}
		}
		if (page+1)*100 >= result.TotalCount {
			return found, nil
		}
		if len(result.Results) == 0 {
			return nil, errors.New("jamf package pagination ended before totalCount")
		}
	}
	return nil, errors.New("jamf package discovery exceeds 10000 records")
}

func (c *client) create(ctx context.Context, fields map[string]json.RawMessage) (string, error) {
	data, _, err := c.call(ctx, http.MethodPost, packagePath, "application/json", bytes.NewReader(raw(fields)), nil)
	if err != nil {
		return "", err
	}
	var result struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(data, &result); err != nil || !validID(result.ID) {
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
	body := make(map[string]json.RawMessage)
	for key, value := range fresh.Fields {
		switch {
		case slices.Contains(writableFields, key):
			body[key] = value
		case slices.Contains(readOnlyFields, key):
		default:
			return nil, fmt.Errorf("unknown Jamf package response field %q blocks a lossy full-object PUT", key)
		}
	}
	maps.Copy(body, changes)
	headers := make(http.Header)
	if fresh.ETag != "" {
		headers.Set("If-Match", fresh.ETag)
	}
	_, _, putErr := c.call(ctx, http.MethodPut, packagePath+"/"+id, "application/json", bytes.NewReader(raw(body)), headers)
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
	var envelope bytes.Buffer
	writer := multipart.NewWriter(&envelope)
	if _, err := writer.CreateFormFile("file", content.filename); err != nil {
		return err
	}
	headSize := envelope.Len()
	if err := writer.Close(); err != nil {
		return err
	}
	data := envelope.Bytes()
	body := io.MultiReader(bytes.NewReader(data[:headSize]), io.NewSectionReader(f, 0, content.size), bytes.NewReader(data[headSize:]))
	headers := make(http.Header)
	headers.Set("Content-Length", strconv.FormatInt(int64(len(data))+content.size, 10))
	_, _, err = c.call(ctx, http.MethodPost, packagePath+"/"+id+"/upload", writer.FormDataContentType(), body, headers)
	return err
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
