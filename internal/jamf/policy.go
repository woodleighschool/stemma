package jamf

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"maps"
	"path"
	"slices"
	"strconv"
	"strings"

	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/constants"
	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/plugin"
)

// installPolicy declares a policy that installs the software's current
// package: at check-in or enrollment, from Self Service or on a custom event.
// Patch policies update computers that already have it.
type installPolicy struct {
	Name            string       `json:"name" jsonschema:"minLength=1" jsonschema_description:"Policy name, which finds the policy. Renaming a policy creates another one and leaves the old one in Jamf."`
	Enabled         *bool        `json:"enabled,omitempty" jsonschema_description:"Enable the policy. A policy Stemma creates starts disabled."`
	Category        categoryName `json:"category,omitzero" jsonschema_description:"Category name for the policy, or null for none. Defaults to the package category."`
	Triggers        *[]string    `json:"triggers,omitempty" jsonschema:"uniqueItems=true,enum=checkin,enum=enrollment_complete,enum=login,enum=startup,enum=network_state_change" jsonschema_description:"Events that run the policy automatically: recurring check-in, enrollment complete, login, startup or a network state change. With none, the policy runs from Self Service or its custom event."`
	Frequency       *string      `json:"frequency,omitempty" jsonschema:"enum=once_per_computer,enum=once_per_user_per_computer,enum=once_per_user,enum=once_every_day,enum=once_every_week,enum=once_every_month,enum=ongoing" jsonschema_description:"How often the policy runs on a computer. A policy that runs once per computer leaves Self Service after it runs, so a Self Service policy usually runs ongoing."`
	Retries         *int         `json:"retries,omitempty" jsonschema:"minimum=0,maximum=10" jsonschema_description:"Retry a failed run at the next check-in, up to this many times; it needs frequency: once_per_computer. Zero turns retries off."`
	Event           *string      `json:"event,omitempty" jsonschema_description:"Custom event that runs the policy, as in jamf policy -event NAME. An empty string removes it."`
	UpdateInventory *bool        `json:"update_inventory,omitempty" jsonschema_description:"Update inventory after the policy runs. A policy Stemma creates updates inventory."`
	SelfService     *selfService `json:"self_service,omitempty" jsonschema_description:"Offer the policy in Self Service: true, false, or its Self Service settings."`
	Scope           *policyScope `json:"scope,omitempty" jsonschema_description:"Scope objects named exactly as in Jamf. Each name must match exactly one object, and supplied lists replace their collections."`

	// category, selfServiceCategory and scope hold the objects the declaration
	// names, resolved against Jamf before planning; a nil category is unmanaged.
	category            *namedID
	selfServiceCategory *namedID
	scope               map[string][]string
	// id is the policy found by name, once planning or applying has looked.
	id string
}

// policyTriggers maps declared triggers to the Classic API flags that set them.
var policyTriggers = map[string]string{
	"checkin":              "trigger_checkin",
	"enrollment_complete":  "trigger_enrollment_complete",
	"login":                "trigger_login",
	"startup":              "trigger_startup",
	"network_state_change": "trigger_network_state_changed",
}

// frequencies maps declared frequencies to Classic API values.
var frequencies = map[string]string{
	"once_per_computer":          "Once per computer",
	"once_per_user_per_computer": "Once per user per computer",
	"once_per_user":              "Once per user",
	"once_every_day":             "Once every day",
	"once_every_week":            "Once every week",
	"once_every_month":           "Once every month",
	"ongoing":                    "Ongoing",
}

// selfService puts a policy in Self Service. True takes the derived settings;
// an object sets them.
type selfService struct {
	selfServiceSettings

	enabled bool
}

type selfServiceSettings struct {
	DisplayName *string      `json:"display_name,omitempty" jsonschema:"minLength=1" jsonschema_description:"Name shown in Self Service. Defaults to the selected application's name, else the software name."`
	Description *string      `json:"description,omitempty" jsonschema_description:"Description shown in Self Service, in Markdown. An empty string clears it."`
	Category    categoryName `json:"category,omitzero" jsonschema_description:"Self Service category name, or null for none. Defaults to the package category."`
	Featured    *bool        `json:"featured,omitempty" jsonschema_description:"Feature the item on the Self Service home page."`
}

func (s *selfService) UnmarshalJSON(data []byte) error {
	if err := json.Unmarshal(data, &s.enabled); err == nil {
		return nil
	}
	s.enabled = true
	return strictDecode(data, &s.selfServiceSettings)
}

func (selfService) JSONSchema() *jsonschema.Schema {
	settings := (&jsonschema.Reflector{DoNotReference: true}).Reflect(selfServiceSettings{})
	settings.ID, settings.Version = "", ""
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Type: "boolean"}, settings}}
}

func decodePolicies(metadata map[string]json.RawMessage) ([]*installPolicy, error) {
	data, ok := metadata["policies"]
	if !ok {
		return nil, nil
	}
	if err := rejectNulls(data, "category"); err != nil {
		return nil, fmt.Errorf("jamf policies: %w", err)
	}
	var declared []*installPolicy
	if err := strictDecode(data, &declared); err != nil {
		return nil, fmt.Errorf("jamf policies: %w", err)
	}
	seen := map[string]bool{}
	for _, policy := range declared {
		if err := policy.validate(); err != nil {
			return nil, err
		}
		if seen[policy.Name] {
			return nil, fmt.Errorf("jamf policy %q is declared more than once", policy.Name)
		}
		seen[policy.Name] = true
	}
	return declared, nil
}

func (p *installPolicy) validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return errors.New("jamf policies need a name")
	}
	names := map[string]*string{"category": p.Category.name}
	if s := p.SelfService; s != nil {
		names["self_service.display_name"], names["self_service.category"] = s.DisplayName, s.Category.name
	}
	for field, value := range names {
		if value != nil && strings.TrimSpace(*value) == "" {
			return fmt.Errorf("jamf policy %q %s cannot be empty", p.Name, field)
		}
	}
	if p.Triggers != nil {
		for i, trigger := range *p.Triggers {
			if policyTriggers[trigger] == "" || slices.Contains((*p.Triggers)[:i], trigger) {
				return fmt.Errorf("jamf policy %q triggers must be distinct values from %s", p.Name, strings.Join(slices.Sorted(maps.Keys(policyTriggers)), ", "))
			}
		}
	}
	if p.Frequency != nil && frequencies[*p.Frequency] == "" {
		return fmt.Errorf("jamf policy %q frequency must be one of %s", p.Name, strings.Join(slices.Sorted(maps.Keys(frequencies)), ", "))
	}
	if r := p.Retries; r != nil {
		if *r < 0 || *r > 10 {
			return fmt.Errorf("jamf policy %q retries must be 0 to 10", p.Name)
		}
		// Jamf retries only policies that run once per computer.
		if *r > 0 && (p.Frequency == nil || *p.Frequency != "once_per_computer") {
			return fmt.Errorf("jamf policy %q retries needs frequency: once_per_computer", p.Name)
		}
	}
	return p.Scope.validate("policy " + strconv.Quote(p.Name))
}

// resolvePolicy finds the categories and scope objects the declaration names.
// The package category is the default for both categories.
func (c *client) resolvePolicy(ctx context.Context, policy *installPolicy, packageCategory *namedID) error {
	var err error
	if policy.category, err = c.category(ctx, policy.Category, packageCategory); err != nil {
		return err
	}
	var selfServiceCategory categoryName
	if s := policy.SelfService; s != nil {
		selfServiceCategory = s.Category
	}
	if policy.selfServiceCategory, err = c.category(ctx, selfServiceCategory, packageCategory); err != nil {
		return err
	}
	policy.scope, err = c.resolveScope(ctx, policy.Scope)
	return err
}

// publication is what a policy installs and shows for one software.
type publication struct {
	software    string
	application string
	packageID   string
	filename    string
	icon        *selfServiceIcon
}

// applicationName is the selected application's name without its extension.
func applicationName(artifact plugin.Artifact) string {
	var selected plugin.Subject
	if json.Unmarshal(artifact.Evidence["macos.application"], &selected) != nil || selected.App == nil {
		return ""
	}
	location := selected.InstalledPath
	if location == "" {
		location = selected.Path
	}
	return strings.TrimSuffix(path.Base(location), ".app")
}

// field names the policy in plans.
func (p *installPolicy) field() string { return "policies[" + p.Name + "]" }

// settings are the values Stemma owns in the policy: the package it installs,
// and the declared settings with their derived Self Service defaults.
func (p *installPolicy) settings(c *client, pub publication) []setting {
	const root = "policy"
	var settings []setting
	if p.Enabled != nil {
		settings = append(settings, setting{field: "enabled", value: *p.Enabled, fragment: fragment(root, *p.Enabled, "general", "enabled"), current: flag("general", "enabled")})
	}
	if p.category != nil {
		settings = append(settings, setting{field: "category", value: p.category.name, fragment: whole(fragment(root, p.category.id, "general", "category", "id"), "general", "category"), current: func(doc *xmlNode) any {
			if doc.value("general", "category", "id") == "-1" {
				return nil
			}
			return doc.value("general", "category", "name")
		}})
	}
	if p.Triggers != nil {
		declared := slices.Sorted(slices.Values(*p.Triggers))
		flags := map[string]bool{}
		for trigger, native := range policyTriggers {
			flags[native] = slices.Contains(declared, trigger)
		}
		settings = append(settings, setting{field: "triggers", value: declared, fragment: fragment(root, flags, "general"), current: func(doc *xmlNode) any {
			enabled := []string{}
			for trigger, native := range policyTriggers {
				if doc.value("general", native) == "true" {
					enabled = append(enabled, trigger)
				}
			}
			slices.Sort(enabled)
			return enabled
		}})
	}
	if p.Frequency != nil {
		settings = append(settings, setting{field: "frequency", value: *p.Frequency, fragment: fragment(root, frequencies[*p.Frequency], "general", "frequency"), current: choice(frequencies, "general", "frequency")})
	}
	if p.Retries != nil {
		retry := map[string]any{"retry_event": "none", "retry_attempts": -1}
		if *p.Retries > 0 {
			retry = map[string]any{"retry_event": "check-in", "retry_attempts": *p.Retries}
		}
		settings = append(settings, setting{field: "retries", value: *p.Retries, fragment: fragment(root, retry, "general"), current: retries})
	}
	if p.Event != nil {
		settings = append(settings, setting{field: "event", value: *p.Event, fragment: fragment(root, *p.Event, "general", "trigger_other"), current: text("general", "trigger_other")})
	}
	settings = append(settings, setting{field: "package", value: []string{pub.filename}, fragment: fragment(root, []map[string]string{{"id": pub.packageID, "action": "Install"}}, "package_configuration", "packages"), current: policyPackages})
	if p.UpdateInventory != nil {
		settings = append(settings, setting{field: "update_inventory", value: *p.UpdateInventory, fragment: fragment(root, *p.UpdateInventory, "maintenance", "recon"), current: flag("maintenance", "recon")})
	}
	if s := p.SelfService; s != nil {
		settings = append(settings, setting{field: "self_service", value: s.enabled, fragment: fragment(root, s.enabled, "self_service", "use_for_self_service"), current: flag("self_service", "use_for_self_service")})
		if s.enabled {
			settings = append(settings, p.selfServiceSettings(c, pub)...)
		}
	}
	return append(settings, scopeSettings(root, p.Scope, p.scope)...)
}

func (p *installPolicy) selfServiceSettings(c *client, pub publication) []setting {
	const root = "policy"
	s := p.SelfService
	name := pub.application
	if name == "" {
		name = pub.software
	}
	if s.DisplayName != nil {
		name = *s.DisplayName
	}
	settings := []setting{{field: "self_service.display_name", value: name, fragment: fragment(root, name, "self_service", "self_service_display_name"), current: text("self_service", "self_service_display_name")}}
	if s.Description != nil {
		settings = append(settings, setting{field: "self_service.description", value: *s.Description, fragment: fragment(root, *s.Description, "self_service", "self_service_description"), current: text("self_service", "self_service_description")})
	}
	if category := p.selfServiceCategory; category != nil {
		listed := []map[string]any{}
		if category.name != nil {
			listed = append(listed, map[string]any{"id": category.id, "display_in": true, "feature_in": false})
		}
		settings = append(settings, setting{field: "self_service.category", value: category.name, fragment: fragment(root, listed, "self_service", "self_service_categories"), current: selfServiceCategories})
	}
	if s.Featured != nil {
		settings = append(settings, setting{field: "self_service.featured", value: *s.Featured, fragment: fragment(root, *s.Featured, "self_service", "feature_on_main_page"), current: flag("self_service", "feature_on_main_page")})
	}
	if pub.icon != nil {
		settings = append(settings, pub.icon.setting(c, root, "self_service"))
	}
	return settings
}

// retries reads a policy's retries at check-in as plans show them, or its
// native retry settings when it retries another way.
func retries(doc *xmlNode) any {
	event, attempts := doc.value("general", "retry_event"), doc.value("general", "retry_attempts")
	if event == "none" && attempts == "-1" {
		return 0
	}
	if n, err := strconv.Atoi(attempts); err == nil && event == "check-in" {
		return n
	}
	return map[string]string{"retry_event": event, "retry_attempts": attempts}
}

// policyPackages reads the packages a policy installs as plans show them:
// each name, with its action unless it installs.
func policyPackages(doc *xmlNode) any {
	found := []string{}
	if list := doc.child("package_configuration").child("packages"); list != nil {
		for _, pkg := range list.Children {
			if pkg.XMLName.Local != "package" {
				continue
			}
			name := pkg.value("name")
			if action := pkg.value("action"); action != "Install" {
				name += " (" + action + ")"
			}
			found = append(found, name)
		}
	}
	slices.Sort(found)
	return found
}

// selfServiceCategories reads a policy's Self Service category as plans show
// it: the one category it shows in, or each category and how it shows there.
func selfServiceCategories(doc *xmlNode) any {
	var shown []string
	if list := doc.child("self_service").child("self_service_categories"); list != nil {
		for _, category := range list.Children {
			if category.XMLName.Local != "category" {
				continue
			}
			name := category.value("name")
			if category.value("display_in") != "true" {
				name += " (hidden)"
			}
			if category.value("feature_in") == "true" {
				name += " (featured)"
			}
			shown = append(shown, name)
		}
	}
	switch len(shown) {
	case 0:
		return nil
	case 1:
		return shown[0]
	}
	slices.Sort(shown)
	return shown
}

// planPolicy reports the policy changes publishing needs. The package ID is
// empty while the package has yet to be created.
func (c *client) planPolicy(ctx context.Context, p *installPolicy, pub publication, response *plugin.ReconcileResponse) error {
	id, doc, err := c.findPolicy(ctx, p.Name)
	if err != nil {
		return err
	}
	p.id = id
	pub.icon.observe(doc, "self_service")
	if doc == nil {
		response.Changes = append(response.Changes, plugin.Change{Kind: "metadata", Field: p.field(), Action: "create"})
	}
	response.Changes = append(response.Changes, changes(p.field(), doc, p.settings(c, pub))...)
	return nil
}

func (c *client) applyPolicy(ctx context.Context, p *installPolicy, pub publication) error {
	id, doc, err := c.findPolicy(ctx, p.Name)
	if err != nil {
		return err
	}
	settings := p.settings(c, pub)
	if doc == nil {
		if id, doc, err = c.createPolicy(ctx, p.Name, settings); err != nil {
			return err
		}
	}
	p.id = id
	return c.update(ctx, policies, id, doc, settings)
}

// findPolicy returns the policy named name, or a nil document when the name is free.
func (c *client) findPolicy(ctx context.Context, name string) (string, *xmlNode, error) {
	listed, err := c.listPolicies(ctx)
	if err != nil {
		return "", nil, err
	}
	var found []string
	for _, entry := range listed {
		if entry.Name == name {
			found = append(found, entry.ID)
		}
	}
	switch len(found) {
	case 0:
		return "", nil, nil
	case 1:
	default:
		return "", nil, fmt.Errorf("multiple Jamf policies are named %q; policy names must be unique", name)
	}
	doc, err := c.read(ctx, policies, found[0])
	if err != nil {
		return "", nil, err
	}
	if doc == nil {
		return "", nil, fmt.Errorf("jamf policy %s does not exist", found[0])
	}
	return found[0], doc, nil
}

// listPolicies requires a complete enumeration before a name is considered free
// or a package is considered unreferenced. The SDK's list model reads a missing
// count as zero, so the document is read as it arrived.
func (c *client) listPolicies(ctx context.Context) ([]named, error) {
	listed, err := c.readXML(ctx, policies.path, "policies")
	if err != nil {
		return nil, err
	}
	count, err := strconv.Atoi(listed.value("size"))
	if err != nil || count < 0 {
		return nil, errors.New("jamf policy enumeration has no valid count")
	}
	seen := map[string]bool{}
	var objects []named
	for _, entry := range listed.Children {
		if entry.XMLName.Local != "policy" {
			continue
		}
		id, name := entry.value("id"), entry.value("name")
		if !validID(id) || seen[id] {
			return nil, errors.New("jamf policy enumeration contains invalid or duplicate IDs")
		}
		if strings.TrimSpace(name) == "" {
			return nil, errors.New("jamf policy enumeration contains a missing name")
		}
		seen[id] = true
		objects = append(objects, named{ID: id, Name: name})
	}
	if len(objects) != count {
		return nil, errors.New("jamf policy enumeration is incomplete")
	}
	return objects, nil
}

// createPolicy creates the policy disabled and unscoped, updating inventory
// after it runs; enablement, scope and the icon follow as an update, so a
// policy is never live before it is complete.
func (c *client) createPolicy(ctx context.Context, name string, settings []setting) (string, *xmlNode, error) {
	initial := fragment("policy", map[string]any{
		"general":     map[string]any{"name": name, "enabled": false, "frequency": "Ongoing"},
		"maintenance": map[string]any{"recon": true},
		"scope":       map[string]any{"all_computers": false},
	})
	for _, s := range settings {
		if s.write == nil && s.field != "enabled" && !strings.HasPrefix(s.field, "scope.") {
			mergeXML(initial, s.fragment)
		}
	}
	body, err := xml.Marshal(initial)
	if err != nil {
		return "", nil, err
	}
	result, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).SetHeader("Content-Type", constants.ApplicationXML).SetBody(body).DisableRetry().Post(policies.path + "/id/0")
	if createErr := requestError(ctx, result, err); createErr != nil {
		// A lost response can hide a policy Jamf did create; its name finds it.
		id, doc, err := c.findPolicy(ctx, name)
		if err != nil || doc == nil {
			return "", nil, createErr
		}
		return id, doc, nil
	}
	created, err := parseXML(result.Bytes(), "policy")
	if err != nil {
		return "", nil, err
	}
	id := created.value("id")
	doc, err := c.read(ctx, policies, id)
	if err != nil {
		return "", nil, err
	}
	if doc == nil {
		return "", nil, errors.New("created Jamf policy could not be read back")
	}
	return id, doc, nil
}
