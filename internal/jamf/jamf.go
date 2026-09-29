// Package jamf reconciles packages, install and patch policies, and retention
// with Jamf Pro. It keeps no state between runs: a marker in each package's
// notes identifies the packages that belong to a software, and policies are
// found by name.
package jamf

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/woodleighschool/stemma/plugin"
)

// Config is the Jamf Pro server and the API client that publishes to it.
type Config struct {
	URL          string `json:"url" jsonschema:"minLength=1" jsonschema_description:"Jamf Pro server origin, such as https://school.jamfcloud.com. HTTPS is required except loopback test servers. Paths, embedded credentials, queries and fragments are rejected."`
	ClientID     string `json:"client_id" jsonschema:"minLength=1" jsonschema_description:"Jamf API client ID. Use an env expression to supply it from the environment."`
	ClientSecret string `json:"client_secret" jsonschema:"minLength=1,writeOnly=true" jsonschema_description:"Jamf API client secret. Use an env expression to supply it from the environment."`
}

// Handle validates, plans or applies one software's publication. It converges
// the package holding the artifact's file name, then the install policies,
// patch deployment and retention; plan reports the same changes without
// writing.
func Handle(ctx context.Context, request plugin.ReconcileRequest[Config]) (plugin.ReconcileResponse, error) {
	var response plugin.ReconcileResponse
	config := request.Config
	// Validation also runs before connection settings are available.
	if request.Method != "validate" {
		if err := config.Validate(); err != nil {
			return response, err
		}
	}
	config.URL = strings.TrimRight(config.URL, "/")
	metadata, err := decodeMetadata(request.Metadata)
	if err != nil {
		return response, err
	}
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
	icon, err := newSelfServiceIcon(request.Inputs["icon"])
	if err != nil {
		return response, err
	}
	ctx, cancel := context.WithTimeout(ctx, 15*time.Minute)
	defer cancel()
	c, err := newClient(ctx, config)
	if err != nil {
		return response, err
	}
	// Names resolve before anything is written, so a missing or ambiguous one
	// changes nothing.
	packageCategory, err := c.category(ctx, metadata.category, nil)
	if err != nil {
		return response, err
	}
	if packageCategory != nil {
		metadata.fields["categoryId"] = raw(packageCategory.id)
	}
	for _, policy := range metadata.policies {
		if err := c.resolvePolicy(ctx, policy, packageCategory); err != nil {
			return response, err
		}
	}
	if metadata.patch != nil {
		if err := c.resolvePatch(ctx, metadata.patch, request.Artifact.Version); err != nil {
			return response, err
		}
	}
	identity := request.Identity.Digest()
	family, err := c.family(ctx, identity)
	if err != nil {
		return response, err
	}
	current, err := c.locate(ctx, family, metadata.packageID, identity, content.filename)
	if err != nil {
		return response, err
	}
	desired := maps.Clone(metadata.fields)
	desired["fileName"] = raw(content.filename)
	response.Changes = plan(current, desired, content, identity)
	software := request.Identity.Resource.Name
	pub := publication{software: software, application: applicationName(request.Artifact), filename: content.filename, icon: icon}
	if current != nil {
		pub.packageID = stringField(current.Fields, "id")
	}
	for _, policy := range metadata.policies {
		if err := c.planPolicy(ctx, policy, pub, &response); err != nil {
			return response, err
		}
	}
	if metadata.patch != nil {
		if err := c.planPatch(ctx, metadata.patch, pub.packageID, software, icon, &response); err != nil {
			return response, err
		}
	}
	// The current record is never retired, so creating it below leaves this
	// selection as planned.
	var retiring []summary
	if metadata.retention.Keep > 0 {
		retiring = retired(family, pub.packageID, metadata.retention.Keep)
	}
	if request.Method == "plan" {
		return response, c.prune(ctx, retiring, metadata.patch, metadata.policies, false, &response)
	}
	if pub.packageID, err = c.publishPackage(ctx, current, desired, identity, request.Artifact.Path, content); err != nil {
		return response, err
	}
	for _, policy := range metadata.policies {
		if err := c.applyPolicy(ctx, policy, pub); err != nil {
			return response, err
		}
	}
	if metadata.patch != nil {
		if err := c.applyPatch(ctx, metadata.patch, pub.packageID, software, icon); err != nil {
			return response, err
		}
	}
	return response, c.prune(ctx, retiring, metadata.patch, metadata.policies, true, &response)
}

// Validate checks the server origin and credentials.
func (config Config) Validate() error {
	u, err := url.Parse(config.URL)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.Trim(u.Path, "/") != "" {
		return errors.New("jamf url must be a server origin without credentials, path, query or fragment")
	}
	loopback := u.Hostname() == "localhost"
	if ip := net.ParseIP(u.Hostname()); ip != nil {
		loopback = ip.IsLoopback()
	}
	if u.Scheme != "https" && (u.Scheme != "http" || !loopback) {
		return errors.New("jamf url must use HTTPS (HTTP is allowed only for loopback test servers)")
	}
	if config.ClientID == "" || config.ClientSecret == "" {
		return errors.New("jamf client_id and client_secret are required")
	}

	return nil
}
