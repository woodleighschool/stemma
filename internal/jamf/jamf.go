// Package jamf reconciles packages, patch deployment and retention with Jamf Pro.
// It keeps no state between runs: a marker in each package's notes identifies
// the packages that belong to a software.
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
	"regexp"
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

// markerPattern matches an identity marker that has a line of the notes to itself.
var markerPattern = regexp.MustCompile(`(?m)^\[stemma:v1 id=([0-9a-f]{64})\]$`)

type configuration struct {
	URL          string `json:"url" jsonschema:"minLength=1" jsonschema_description:"Jamf Pro server origin, such as https://school.jamfcloud.com. HTTPS is required except loopback test servers. Paths, embedded credentials, queries and fragments are rejected."`
	ClientID     string `json:"client_id" jsonschema:"minLength=1" jsonschema_description:"Jamf API client ID. Use ${VAR} to supply it from the environment."`
	ClientSecret string `json:"client_secret" jsonschema:"minLength=1" jsonschema_description:"Jamf API client secret. Use ${VAR} to supply it from the environment."`
}

type client struct {
	transport *sdkclient.Transport
	packages  *packages.Packages
}

// observed is a package exactly as Jamf returned it, so a whole-object update
// preserves the fields nothing here manages.
type observed struct {
	Fields map[string]json.RawMessage
	ETag   string
}

// summary is the part of a listed package that identity and retention read.
type summary struct {
	ID       uint64 `json:"id,string"`
	FileName string `json:"fileName"`
	Notes    string `json:"notes"`
}

func (s summary) id() string { return strconv.FormatUint(s.ID, 10) }

type payload struct {
	filename string
	sha256   string
	sha3512  string
	size     int64
}

// Handle validates, plans or applies one software's publication. It converges
// the package holding the artifact's file name, then patch deployment, then
// retention; plan reports the same changes without writing.
func Handle(ctx context.Context, request plugin.ReconcileRequest) (plugin.ReconcileResponse, error) {
	var response plugin.ReconcileResponse
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
	c, err := newClient(ctx, config)
	if err != nil {
		return response, err
	}
	identity := identityDigest(request.Identity)
	family, err := c.family(ctx, identity)
	if err != nil {
		return response, err
	}
	current, err := c.locate(ctx, family, adopt, identity, content.filename)
	if err != nil {
		return response, err
	}
	desired := maps.Clone(metadata)
	desired["fileName"] = raw(content.filename)
	response.Changes = plan(current, desired, content, identity)
	var id string
	if current != nil {
		id = stringField(current.Fields, "id")
	}
	if patch != nil {
		if err := c.planPatch(ctx, patch, id, request.Identity.Resource.Name, &response); err != nil {
			return response, err
		}
	}
	// The current record is never retired, so creating it below leaves this
	// selection as planned.
	var retiring []summary
	if retention.Keep > 0 {
		retiring = retired(family, id, retention.Keep)
	}
	if request.Method == "plan" {
		return response, c.prune(ctx, retiring, patch, false, &response)
	}
	if current == nil {
		if current, err = c.create(ctx, identity, content.filename); err != nil {
			return response, err
		}
		id = stringField(current.Fields, "id")
	}
	// An adopted package takes the file name and marker before any content, so an
	// interrupted run leaves a record the next one finds.
	claim := map[string]json.RawMessage{"fileName": desired["fileName"]}
	if current, err = c.merge(ctx, current, claim, identity); err != nil {
		return response, err
	}
	if !contentDigestMatches(current, content) {
		if err := c.upload(ctx, id, request.Artifact.Path, content); err != nil {
			return response, err
		}
		if current, err = c.awaitContent(ctx, id, content); err != nil {
			return response, err
		}
	}
	// Declared metadata follows verified content, so a failed upload never leaves
	// new settings over old bytes.
	if current, err = c.merge(ctx, current, desired, identity); err != nil {
		return response, err
	}
	if !contentMatches(current, content) {
		return response, errors.New("jamf content changed during metadata reconciliation")
	}
	if patch != nil {
		if err := c.applyPatch(ctx, patch, id, request.Identity.Resource.Name); err != nil {
			return response, err
		}
	}
	return response, c.prune(ctx, retiring, patch, true, &response)
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
	if strings.Contains(stringField(metadata, "notes"), "[stemma:v1 ") {
		return config, nil, "", errors.New("jamf notes contains a reserved Stemma marker")
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
	"packageName":          {"string", false, "Native package display name. Defaults to the artifact filename when creating a package. Identity comes from the notes marker, never this name."},
	"categoryId":           {"string", false, "Native category ID as a string. Use -1 for no category."},
	"info":                 {"string", true, "Native package information. Null clears the field."},
	"notes":                {"string", true, "Administrator notes. Null clears the text. Stemma keeps its identity marker on the final line and preserves the remote text when this is omitted."},
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

// defaults is a new package record. It carries the marker from creation, so an
// interrupted run leaves a record the next one finds.
func defaults(filename, identity string) map[string]json.RawMessage {
	return map[string]json.RawMessage{"fileName": raw(filename), "packageName": raw(filename), "notes": raw(marker(identity)), "categoryId": raw("-1"), "priority": raw(10), "fillUserTemplate": raw(false), "osInstall": raw(false), "rebootRequired": raw(false), "suppressEula": raw(false), "suppressFromDock": raw(false), "suppressRegistration": raw(false), "suppressUpdates": raw(false)}
}

func plan(current *observed, desired map[string]json.RawMessage, content payload, identity string) []plugin.Change {
	desired = markedFields(current, desired, identity)
	var changes []plugin.Change
	if current == nil {
		changes = append(changes, plugin.Change{Kind: "content", Field: "package", Action: "create", After: raw(content.filename)})
		current = &observed{Fields: defaults(content.filename, identity)}
	} else if !contentDigestMatches(current, content) {
		changes = append(changes, plugin.Change{Kind: "content", Field: "sha256", Action: "upload", Before: current.Fields["sha256"], After: raw(content.sha256)})
	}
	for _, key := range slices.Sorted(maps.Keys(desired)) {
		if !equalJSON(current.Fields[key], desired[key]) {
			changes = append(changes, plugin.Change{Kind: "metadata", Field: key, Action: "set", Before: current.Fields[key], After: desired[key]})
		}
	}
	return changes
}

func marker(identity string) string { return "[stemma:v1 id=" + identity + "]" }

// identities returns the identity of every marker line in notes.
func identities(notes string) []string {
	var found []string
	for _, match := range markerPattern.FindAllStringSubmatch(notes, -1) {
		found = append(found, match[1])
	}
	return found
}

// unmarked returns the text of notes: no marker lines and no trailing newlines,
// so marking it again reproduces the same notes.
func unmarked(notes string) string {
	lines := slices.DeleteFunc(strings.Split(notes, "\n"), markerPattern.MatchString)
	return strings.TrimRight(strings.Join(lines, "\n"), "\n")
}

// withMarker keeps the marker on a final line of its own below text.
func withMarker(text, identity string) string {
	if text == "" {
		return marker(identity)
	}
	return text + "\n" + marker(identity)
}

// noteText is the declared notes text, or the remote text while notes are omitted.
func noteText(current *observed, metadata map[string]json.RawMessage) string {
	if _, declared := metadata["notes"]; declared || current == nil {
		return stringField(metadata, "notes")
	}
	return unmarked(stringField(current.Fields, "notes"))
}

func markedFields(current *observed, declared map[string]json.RawMessage, identity string) map[string]json.RawMessage {
	fields := maps.Clone(declared)
	fields["notes"] = raw(withMarker(noteText(current, declared), identity))
	return fields
}

func inspectPayload(ctx context.Context, identity plugin.Identity, artifact plugin.Artifact) (payload, error) {
	if identity.Project == "" || identity.Resource.Name == "" || identity.Destination == "" {
		return payload{}, errors.New("jamf requires a complete logical identity")
	}
	// Jamf installs a DMG as a filesystem layout copied onto the startup disk,
	// which an application DMG is not.
	if strings.ToLower(filepath.Ext(artifact.Filename)) != ".pkg" {
		return payload{}, fmt.Errorf("jamf publishes PKG artifacts, not %s", artifact.Filename)
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
	return payload{filename: artifact.Filename, sha256: digest, sha3512: hex.EncodeToString(hash3.Sum(nil)), size: n}, nil
}

func identityDigest(identity plugin.Identity) string {
	sum := sha256.Sum256(raw([]string{identity.Project, identity.Resource.Key(), identity.Destination}))
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

// family lists the packages whose notes carry this identity's marker.
func (c *client) family(ctx context.Context, identity string) ([]summary, error) {
	listed, err := c.list(ctx, sdkclient.NewRSQLFilterBuilder().Contains("notes", marker(identity)).Build())
	if err != nil {
		return nil, err
	}
	// The filter finds the marker anywhere in the notes; only a marker line is identity.
	return slices.DeleteFunc(listed, func(pkg summary) bool { return !slices.Contains(identities(pkg.Notes), identity) }), nil
}

func (c *client) list(ctx context.Context, filter string) ([]summary, error) {
	objects, err := c.listObjects(ctx, packagePath, map[string]string{"filter": filter})
	if err != nil {
		return nil, err
	}
	listed := make([]summary, 0, len(objects))
	for _, data := range objects {
		var pkg summary
		if err := json.Unmarshal(data, &pkg); err != nil {
			return nil, errors.New("invalid Jamf package listing")
		}
		listed = append(listed, pkg)
	}
	return listed, nil
}

// locate selects the record this artifact publishes into: the package_id pin,
// or the family member holding the artifact's file name. It returns nil when
// that record has yet to be created.
func (c *client) locate(ctx context.Context, family []summary, adopt, identity, filename string) (*observed, error) {
	id := adopt
	if id == "" {
		for _, pkg := range family {
			if pkg.FileName != filename {
				continue
			}
			if id != "" {
				return nil, fmt.Errorf("multiple Jamf packages match file name %q for this software; set package_id to select one", filename)
			}
			id = pkg.id()
		}
	}
	var current *observed
	if id != "" {
		var err error
		if current, err = c.get(ctx, id); err != nil {
			return nil, err
		}
	}
	if adopt != "" {
		if current == nil {
			return nil, fmt.Errorf("jamf package_id %s does not exist", adopt)
		}
		if slices.ContainsFunc(identities(stringField(current.Fields, "notes")), func(found string) bool { return found != identity }) {
			return nil, fmt.Errorf("jamf package %s carries another Stemma identity", adopt)
		}
	}
	if current != nil && stringField(current.Fields, "fileName") == filename {
		return current, nil
	}
	// File names share one distribution point namespace, so taking a name another
	// record holds would replace that package's content.
	holders, err := c.list(ctx, sdkclient.NewRSQLFilterBuilder().EqualTo("fileName", filename).Build())
	if err != nil {
		return nil, err
	}
	for _, holder := range holders {
		if holder.FileName == filename && holder.id() != id {
			return nil, fmt.Errorf("jamf package %s already uses file name %q; set package_id to %s to adopt it", holder.id(), filename, holder.id())
		}
	}
	return current, nil
}

func (c *client) create(ctx context.Context, identity, filename string) (*observed, error) {
	var body packages.RequestPackage
	if err := json.Unmarshal(raw(defaults(filename, identity)), &body); err != nil {
		return nil, err
	}
	// CreateV1 performs a distribution-point preflight inside the create call,
	// so its failure would not say whether a package was written.
	var result packages.CreateResponse
	response, err := c.transport.NewRequest(ctx).
		SetHeader("Accept", constants.ApplicationJSON).
		SetHeader("Content-Type", constants.ApplicationJSON).
		SetBody(&body).
		SetResult(&result).
		Post(packagePath)
	createErr := requestError(ctx, response, err)
	if createErr == nil && !validID(result.ID) {
		createErr = errors.New("jamf create response has no valid package ID")
	}
	if createErr != nil {
		// A lost response can hide a package Jamf did create; its marker finds it.
		family, err := c.family(ctx, identity)
		if err != nil {
			return nil, createErr
		}
		created, err := c.locate(ctx, family, "", identity, filename)
		if err != nil || created == nil {
			return nil, createErr
		}
		return created, nil
	}
	created, err := c.get(ctx, result.ID)
	if err != nil {
		return nil, err
	}
	if created == nil {
		return nil, errors.New("created Jamf package could not be read back")
	}
	return created, nil
}

func differs(fields, desired map[string]json.RawMessage) bool {
	for key, value := range desired {
		if !equalJSON(fields[key], value) {
			return true
		}
	}
	return false
}

// merge writes the desired fields that differ and verifies them by readback.
func (c *client) merge(ctx context.Context, current *observed, desired map[string]json.RawMessage, identity string) (*observed, error) {
	if !differs(current.Fields, markedFields(current, desired, identity)) {
		return current, nil
	}
	id := stringField(current.Fields, "id")
	// The update replaces the whole object, so it merges over the newest read.
	fresh, err := c.get(ctx, id)
	if err != nil {
		return nil, err
	}
	if fresh == nil {
		return nil, errors.New("jamf package disappeared before metadata update")
	}
	desired = markedFields(fresh, desired, identity)
	if !differs(fresh.Fields, desired) {
		return fresh, nil
	}
	body := maps.Clone(fresh.Fields)
	for _, key := range readOnlyFields {
		delete(body, key)
	}
	maps.Copy(body, desired)
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
	for key, value := range desired {
		if !equalJSON(readback.Fields[key], value) {
			if putErr != nil {
				return nil, fmt.Errorf("jamf metadata update was not observed: %w", putErr)
			}
			return nil, fmt.Errorf("jamf metadata %s did not match readback", key)
		}
	}
	return readback, nil
}

func (c *client) upload(ctx context.Context, id, file string, content payload) (err error) {
	done := plugin.Stage(ctx, "Uploading Jamf package")
	defer func() { done(err) }()
	f, err := os.Open(file)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }()
	// UploadV1 names the upload after the local path, which need not be the
	// artifact's file name.
	var lastProgress time.Time
	response, err := c.transport.NewRequest(ctx).
		SetHeader("Accept", constants.ApplicationJSON).
		SetMultipartFile("file", content.filename, f, content.size, func(_, _ string, current, total int64) {
			if current == total || time.Since(lastProgress) >= 250*time.Millisecond {
				plugin.Logger(ctx).InfoContext(ctx, "Transfer progress", "progress", true, "current", current, "total", total, "unit", "bytes", "progress_final", current == total)
				lastProgress = time.Now()
			}
		}).
		Post(packagePath + "/" + id + "/upload")
	return requestError(ctx, response, err)
}

func (c *client) awaitContent(ctx context.Context, id string, content payload) (result *observed, err error) {
	done := plugin.Stage(ctx, "Waiting for Jamf processing")
	defer func() { done(err) }()
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
	return nil, errors.New("jamf content is not READY with a matching SHA-256 or SHA3-512 after 60 seconds")
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
