package jamf

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/constants"
	titles "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/patch_software_title_configurations"
)

// A named object is a Jamf object that declarations address by its exact name.
type named struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// unique returns the ID of the one object carrying name. Names select objects
// only while they identify exactly one.
func unique(kind, name string, objects []named) (string, error) {
	var found []string
	for _, object := range objects {
		if object.Name == name {
			found = append(found, object.ID)
		}
	}
	switch len(found) {
	case 1:
		return found[0], nil
	case 0:
		return "", fmt.Errorf("jamf has no %s named %q", kind, name)
	}
	return "", fmt.Errorf("jamf has %d %ss named %q; names must be unique", len(found), kind, name)
}

// rsql quotes a value for an exact RSQL comparison.
func rsql(field, value string) string {
	return field + `=="` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(value) + `"`
}

// categoryID resolves a declared category: a name, or null for none.
func (c *client) categoryID(ctx context.Context, value json.RawMessage) (string, error) {
	var name *string
	if err := json.Unmarshal(value, &name); err != nil {
		return "", errors.New("jamf category must be a name or null")
	}
	if name == nil {
		return "-1", nil
	}
	return c.objectID(ctx, "category", *name)
}

// objectID resolves the name of a category or a scope object.
func (c *client) objectID(ctx context.Context, kind, name string) (string, error) {
	var objects []named
	var err error
	switch kind {
	case "category", "building", "department":
		path := map[string]string{"category": "/api/v1/categories", "building": "/api/v1/buildings", "department": "/api/v1/departments"}[kind]
		objects, err = c.filtered(ctx, path, "name", name, func(data json.RawMessage) (named, error) {
			var object named
			err := json.Unmarshal(data, &object)
			return object, err
		})
	case "computer":
		objects, err = c.filtered(ctx, "/api/v1/computers-inventory", "general.name", name, func(data json.RawMessage) (named, error) {
			var computer struct {
				ID      string `json:"id"`
				General struct {
					Name string `json:"name"`
				} `json:"general"`
			}
			err := json.Unmarshal(data, &computer)
			return named{ID: computer.ID, Name: computer.General.Name}, err
		})
	case "computer group":
		objects, err = c.computerGroups(ctx)
	case "network segment":
		objects, err = c.networkSegments(ctx)
	default:
		return "", fmt.Errorf("unsupported Jamf object kind %q", kind)
	}
	if err != nil {
		return "", fmt.Errorf("jamf %s %q: %w", kind, name, err)
	}
	return unique(kind, name, objects)
}

// filtered lists the objects a Jamf Pro collection holds under an exact name.
func (c *client) filtered(ctx context.Context, path, field, name string, decode func(json.RawMessage) (named, error)) ([]named, error) {
	query := map[string]string{"filter": rsql(field, name)}
	if path == "/api/v1/computers-inventory" {
		query["section"] = "GENERAL"
	}
	rows, err := c.listObjects(ctx, path, query)
	if err != nil {
		return nil, err
	}
	objects := make([]named, 0, len(rows))
	for _, row := range rows {
		object, err := decode(row)
		if err != nil {
			return nil, errors.New("invalid Jamf object in collection")
		}
		objects = append(objects, object)
	}
	return objects, nil
}

func (c *client) computerGroups(ctx context.Context) ([]named, error) {
	result, data, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationJSON).GetBytes("/api/v1/computer-groups")
	if err := requestError(ctx, result, err); err != nil {
		return nil, err
	}
	var groups []named
	if err := json.Unmarshal(data, &groups); err != nil {
		return nil, errors.New("invalid Jamf computer group collection")
	}
	return groups, nil
}

func (c *client) networkSegments(ctx context.Context) ([]named, error) {
	segments, err := c.readXML(ctx, "/JSSResource/networksegments", "network_segments")
	if err != nil {
		return nil, err
	}
	var objects []named
	for _, segment := range segments.Children {
		if segment.XMLName.Local == "network_segment" {
			objects = append(objects, named{ID: segment.value("id"), Name: segment.value("name")})
		}
	}
	return objects, nil
}

// listTitles lists every patch software title configuration.
func (c *client) listTitles(ctx context.Context) ([]titles.ResourcePatchSoftwareTitleConfiguration, error) {
	result, data, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationJSON).GetBytes(titlePath)
	if err := requestError(ctx, result, err); err != nil {
		return nil, err
	}
	var configurations *[]titles.ResourcePatchSoftwareTitleConfiguration
	if err := json.Unmarshal(data, &configurations); err != nil || configurations == nil {
		return nil, errors.New("jamf title enumeration returned an incomplete result")
	}
	seen := map[string]bool{}
	for _, listed := range *configurations {
		if !validID(listed.ID) || seen[listed.ID] {
			return nil, errors.New("invalid or duplicated Jamf title ID")
		}
		seen[listed.ID] = true
	}
	return *configurations, nil
}

// titleID resolves a patch title by its display name.
func (c *client) titleID(ctx context.Context, name string) (string, error) {
	configurations, err := c.listTitles(ctx)
	if err != nil {
		return "", err
	}
	objects := make([]named, len(configurations))
	for i, configuration := range configurations {
		objects[i] = named{ID: configuration.ID, Name: configuration.DisplayName}
	}
	return unique("patch title", name, objects)
}
