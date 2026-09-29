package jamf

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/constants"
	"github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/patch_policies"
	titles "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/patch_software_title_configurations"
	"github.com/invopop/jsonschema"
	"github.com/woodleighschool/stemma/plugin"
)

const (
	titlePath = constants.EndpointJamfProPatchSoftwareTitleConfigurationsV3
	// policyPath is the Classic API patch policy collection, which the SDK does not cover.
	policyPath = "/JSSResource/patchpolicies"
)

// patchConfig declares patch deployment: an existing title, found by its
// display name, and an optional patch policy. The version it deploys is the
// software's managed version, which the title's definitions must contain.
type patchConfig struct {
	Title  string       `json:"title" jsonschema:"minLength=1" jsonschema_description:"Display name of an existing patch software title. It must match exactly one title. Until the title defines the software's managed version, the package link, the policy's target version and a new policy wait for it."`
	Policy *patchPolicy `json:"policy,omitempty" jsonschema_description:"Patch policy found by name within the title, where the name defaults to the software name. It is updated in place, or created disabled and unscoped before the declared settings are applied. The title's definitions supply the apps it quits and its minimum OS."`

	// titleID, version and scope are the title, the prepared version and the
	// scope object IDs, resolved against Jamf before planning. waiting reports
	// that the title does not define the version.
	titleID string
	version string
	scope   map[string][]string
	waiting bool
}

type patchPolicy struct {
	Name           *string      `json:"name,omitempty" jsonschema:"minLength=1" jsonschema_description:"Policy name within the title. Defaults to the software name."`
	Enabled        *bool        `json:"enabled,omitempty" jsonschema_description:"Enable the policy. A policy Stemma creates starts disabled."`
	Distribution   *string      `json:"distribution,omitempty" jsonschema:"enum=automatic,enum=self_service" jsonschema_description:"Install updates automatically, or offer them in Self Service with a notification and reminders."`
	GraceMinutes   *int         `json:"grace_minutes,omitempty" jsonschema:"minimum=0" jsonschema_description:"Minutes users have to save their work before the title's apps quit for an automatic update."`
	DeadlineDays   days         `json:"deadline_days,omitzero" jsonschema_description:"Days an update is offered in Self Service before it installs automatically; it needs distribution: self_service. Null removes the deadline."`
	ReminderDays   days         `json:"reminder_days,omitzero" jsonschema_description:"Days between reminders about an update offered in Self Service; it needs distribution: self_service. Defaults to 1; null turns reminders off."`
	PatchUnknown   *bool        `json:"patch_unknown,omitempty" jsonschema_description:"Also update computers whose installed version the title does not define."`
	AllowDowngrade *bool        `json:"allow_downgrade,omitempty" jsonschema_description:"Also install the target version on computers that have a newer one, as when the catalog returns to an earlier release."`
	Scope          *policyScope `json:"scope,omitempty" jsonschema_description:"Scope objects named exactly as in Jamf. Each name must match exactly one object, and supplied lists replace their collections."`
}

// days is a number of days that null removes.
type days struct {
	set   bool
	count *int
}

func (d *days) UnmarshalJSON(data []byte) error {
	d.set = true
	return json.Unmarshal(data, &d.count)
}

func (days) JSONSchema() *jsonschema.Schema {
	return &jsonschema.Schema{AnyOf: []*jsonschema.Schema{{Type: "integer", Minimum: "1"}, {Type: "null"}}}
}

// distributions maps declared distributions to Classic API methods. Jamf
// reads back any other value as prompt, its automatic installation.
var distributions = map[string]string{"automatic": "prompt", "self_service": "selfservice"}

func decodePatch(metadata map[string]json.RawMessage) (*patchConfig, error) {
	data, ok := metadata["patch"]
	if !ok {
		return nil, nil
	}
	if err := rejectNulls(data, "deadline_days", "reminder_days"); err != nil {
		return nil, fmt.Errorf("jamf patch: %w", err)
	}
	var patch patchConfig
	if err := strictDecode(data, &patch); err != nil {
		return nil, fmt.Errorf("jamf patch: %w", err)
	}
	if strings.TrimSpace(patch.Title) == "" {
		return nil, errors.New("jamf patch requires the title's display name")
	}
	if p := patch.Policy; p != nil {
		if p.Name != nil && strings.TrimSpace(*p.Name) == "" {
			return nil, errors.New("jamf patch policy name cannot be empty")
		}
		if p.Distribution != nil && distributions[*p.Distribution] == "" {
			return nil, errors.New("jamf patch distribution must be automatic or self_service")
		}
		if p.GraceMinutes != nil && *p.GraceMinutes < 0 {
			return nil, errors.New("jamf patch grace_minutes cannot be negative")
		}
		for _, d := range []struct {
			field string
			days  days
		}{{"deadline_days", p.DeadlineDays}, {"reminder_days", p.ReminderDays}} {
			if d.days.count != nil && *d.days.count < 1 {
				return nil, fmt.Errorf("jamf patch %s must be at least 1", d.field)
			}
			// Jamf keeps these only for updates offered in Self Service.
			if d.days.set && (p.Distribution == nil || *p.Distribution != "self_service") {
				return nil, fmt.Errorf("jamf patch %s needs distribution: self_service", d.field)
			}
		}
		if err := p.Scope.validate("patch"); err != nil {
			return nil, err
		}
	}
	return &patch, nil
}

// resolvePatch finds the title and scope objects the declaration names and
// takes the version the prepared installer supplies.
func (c *client) resolvePatch(ctx context.Context, patch *patchConfig, version string) error {
	if version == "" {
		return errors.New("jamf patch needs the prepared installer's managed version; select an application")
	}
	id, err := c.titleID(ctx, patch.Title)
	if err != nil {
		return err
	}
	patch.titleID, patch.version = id, version
	if patch.Policy == nil {
		return nil
	}
	patch.scope, err = c.resolveScope(ctx, patch.Policy.Scope)
	return err
}

func (c *client) getTitle(ctx context.Context, id string) (*titles.ResourcePatchSoftwareTitleConfiguration, error) {
	result, response, err := titles.NewPatchSoftwareTitleConfigurations(c.transport).GetByIDV3(ctx, id)
	if err := requestError(ctx, response, err); err != nil {
		return nil, err
	}
	if result == nil || result.ID != id {
		return nil, errors.New("jamf title response does not match requested ID")
	}
	fields, err := decodeObject(response.Bytes())
	if err != nil {
		return nil, err
	}
	if data := fields["packages"]; len(data) == 0 || data[0] != '[' {
		return nil, errors.New("jamf title package associations were omitted")
	}
	return result, nil
}

func titlePackage(title *titles.ResourcePatchSoftwareTitleConfiguration, version string) (string, error) {
	var found string
	for _, pkg := range title.Packages {
		if pkg.Version != version {
			continue
		}
		if found != "" {
			return "", errors.New("jamf title has multiple package associations for one version")
		}
		found = pkg.PackageID
	}
	return found, nil
}

// policyName identifies the patch policy within its title.
func policyName(patch *patchConfig, software string) string {
	if patch.Policy.Name != nil {
		return *patch.Policy.Name
	}
	return software
}

// settings are the values Stemma owns in the patch policy: the target version,
// the declared settings, and the Self Service notification and icon an update
// offered there uses. The target waits while the title lacks the version.
func (patch *patchConfig) settings(c *client, icon *selfServiceIcon) []setting {
	const root = "patch_policy"
	p := patch.Policy
	var settings []setting
	if !patch.waiting {
		settings = append(settings, setting{field: "target_version", value: patch.version, fragment: fragment(root, patch.version, "general", "target_version"), current: text("general", "target_version")})
	}
	if p.Enabled != nil {
		settings = append(settings, setting{field: "enabled", value: *p.Enabled, fragment: fragment(root, *p.Enabled, "general", "enabled"), current: flag("general", "enabled")})
	}
	if p.Distribution != nil {
		settings = append(settings, setting{field: "distribution", value: *p.Distribution, fragment: fragment(root, distributions[*p.Distribution], "general", "distribution_method"), current: choice(distributions, "general", "distribution_method")})
	}
	if p.GraceMinutes != nil {
		settings = append(settings, setting{field: "grace_minutes", value: *p.GraceMinutes, fragment: fragment(root, *p.GraceMinutes, "user_interaction", "grace_period", "grace_period_duration"), current: number("user_interaction", "grace_period", "grace_period_duration")})
	}
	if p.DeadlineDays.set {
		settings = append(settings, daysSetting(root, "deadline_days", p.DeadlineDays.count, []string{"user_interaction", "deadlines"}, "deadline_enabled", "deadline_period"))
	}
	if p.PatchUnknown != nil {
		settings = append(settings, setting{field: "patch_unknown", value: *p.PatchUnknown, fragment: fragment(root, *p.PatchUnknown, "general", "patch_unknown"), current: flag("general", "patch_unknown")})
	}
	if p.AllowDowngrade != nil {
		settings = append(settings, setting{field: "allow_downgrade", value: *p.AllowDowngrade, fragment: fragment(root, *p.AllowDowngrade, "general", "allow_downgrade"), current: flag("general", "allow_downgrade")})
	}
	if p.Distribution != nil && *p.Distribution == "self_service" {
		reminders := new(1)
		if p.ReminderDays.set {
			reminders = p.ReminderDays.count
		}
		settings = append(settings,
			setting{field: "notifications", value: true, fragment: fragment(root, true, "user_interaction", "notifications", "notification_enabled"), current: flag("user_interaction", "notifications", "notification_enabled")},
			daysSetting(root, "reminder_days", reminders, []string{"user_interaction", "notifications", "reminders"}, "notification_reminders_enabled", "notification_reminder_frequency"),
		)
		if icon != nil {
			settings = append(settings, icon.setting(c, root, "user_interaction"))
		}
	}
	return append(settings, scopeSettings(root, p.Scope, patch.scope)...)
}

// daysSetting sets a number of days at path, where the element enabled turns
// it on and period holds it; nil turns it off.
func daysSetting(root, field string, count *int, path []string, enabled, period string) setting {
	values := map[string]any{enabled: count != nil}
	if count != nil {
		values[period] = *count
	}
	return setting{field: field, value: count, fragment: fragment(root, values, path...), current: func(doc *xmlNode) any {
		if doc.value(slices.Concat(path, []string{enabled})...) != "true" {
			return nil
		}
		return number(slices.Concat(path, []string{period})...)(doc)
	}}
}

// planPatch checks the title defines the version and reports the association
// and policy changes. packageID is empty while the package has yet to be
// created.
func (c *client) planPatch(ctx context.Context, patch *patchConfig, packageID, software string, icon *selfServiceIcon, response *plugin.ReconcileResponse) error {
	title, err := c.getTitle(ctx, patch.titleID)
	if err != nil {
		return fmt.Errorf("jamf patch title: %w", err)
	}
	definitions, result, err := titles.NewPatchSoftwareTitleConfigurations(c.transport).GetDefinitionsByIDV3(ctx, patch.titleID, nil)
	if err := requestError(ctx, result, err); err != nil {
		return err
	}
	if definitions == nil || !slices.ContainsFunc(definitions.Results, func(d titles.ResourceDefinition) bool { return d.Version == patch.version }) {
		var available []titles.ResourceDefinition
		if definitions != nil {
			available = definitions.Results
		}
		// Jamf defines new versions some time after vendors ship them, and a
		// title can name versions differently. The package publishes meanwhile.
		plugin.Logger(ctx).WarnContext(ctx, fmt.Sprintf("Jamf patch title %q does not define version %s, so its package link and patch policy target wait for it%s", patch.Title, patch.version, recentDefinitions(available)))
		patch.waiting = true
	} else {
		linked, err := titlePackage(title, patch.version)
		if err != nil {
			return err
		}
		if linked == "" || linked != packageID {
			change := plugin.Change{Kind: "metadata", Field: "patch.packages", Action: "associate"}
			if linked != "" {
				change.Before = raw(linked)
			}
			if packageID != "" {
				change.After = raw(packageID)
			}
			response.Changes = append(response.Changes, change)
		}
	}
	if patch.Policy == nil {
		return nil
	}
	name := policyName(patch, software)
	_, policy, err := c.observePolicy(ctx, patch, name)
	if err != nil {
		return err
	}
	icon.observe(policy, "user_interaction")
	if policy == nil {
		// A new policy needs its target version, so it waits with the link.
		if patch.waiting {
			return nil
		}
		response.Changes = append(response.Changes, plugin.Change{Kind: "metadata", Field: "patch.policy", Action: "create", After: raw(name)})
	}
	response.Changes = append(response.Changes, changes("patch.policy", policy, patch.settings(c, icon))...)
	return nil
}

func (c *client) applyPatch(ctx context.Context, patch *patchConfig, packageID, software string, icon *selfServiceIcon) error {
	if !patch.waiting {
		title, err := c.getTitle(ctx, patch.titleID)
		if err != nil {
			return err
		}
		linked, err := titlePackage(title, patch.version)
		if err != nil {
			return err
		}
		if linked != packageID {
			links := slices.DeleteFunc(slices.Clone(title.Packages), func(p titles.SubsetPackage) bool { return p.Version == patch.version })
			links = append(links, titles.SubsetPackage{PackageID: packageID, Version: patch.version})
			if err := c.setTitlePackages(ctx, title.ID, links); err != nil {
				return err
			}
		}
	}
	if patch.Policy == nil {
		return nil
	}
	name := policyName(patch, software)
	id, policy, err := c.observePolicy(ctx, patch, name)
	if err != nil {
		return err
	}
	settings := patch.settings(c, icon)
	if policy == nil {
		if patch.waiting {
			return nil
		}
		if id, policy, err = c.createPatchPolicy(ctx, patch, name, settings); err != nil {
			return err
		}
	}
	return c.update(ctx, patchPolicies, id, policy, settings)
}

// createPatchPolicy creates the policy disabled and unscoped; enablement,
// scope and the icon follow as an update, so a policy is never live before it
// is complete.
func (c *client) createPatchPolicy(ctx context.Context, patch *patchConfig, name string, settings []setting) (string, *xmlNode, error) {
	initial := fragment("patch_policy", map[string]any{
		"general":                         map[string]any{"name": name, "enabled": false},
		"scope":                           map[string]any{"all_computers": false},
		"software_title_configuration_id": patch.titleID,
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
	result, err := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationXML).SetHeader("Content-Type", constants.ApplicationXML).SetBody(body).DisableRetry().Post(policyPath + "/softwaretitleconfig/id/" + patch.titleID)
	if createErr := requestError(ctx, result, err); createErr != nil {
		// A lost response can hide a policy Jamf did create; its name finds it.
		id, policy, err := c.observePolicy(ctx, patch, name)
		if err != nil || policy == nil {
			return "", nil, createErr
		}
		return id, policy, nil
	}
	created, err := parseXML(result.Bytes(), "patch_policy")
	if err != nil {
		return "", nil, err
	}
	id := created.value("id")
	if id == "" {
		id = created.value("general", "id")
	}
	policy, err := c.read(ctx, patchPolicies, id)
	if err != nil {
		return "", nil, err
	}
	if policy == nil {
		return "", nil, errors.New("created Jamf patch policy could not be read back")
	}
	return id, policy, nil
}

func (c *client) setTitlePackages(ctx context.Context, id string, links []titles.SubsetPackage) error {
	// The SDK's update model emits unrelated required and response fields.
	body := make([]map[string]string, 0, len(links))
	for _, link := range links {
		body = append(body, map[string]string{"packageId": link.PackageID, "version": link.Version})
	}
	// Jamf accepts only a JSON merge patch here; plain JSON gets HTTP 415.
	result, updateErr := c.transport.NewRequest(ctx).SetHeader("Accept", constants.ApplicationJSON).SetHeader("Content-Type", constants.ApplicationMergePatchJSON).SetBody(map[string]any{"packages": body}).DisableRetry().Patch(titlePath + "/" + id)
	updateErr = requestError(ctx, result, updateErr)
	actual, err := c.getTitle(ctx, id)
	if err != nil {
		return err
	}
	if !sameAssociations(actual.Packages, links) {
		if updateErr != nil {
			return updateErr
		}
		return errors.New("jamf patch package associations did not match readback")
	}
	return nil
}

func sameAssociations(a, b []titles.SubsetPackage) bool {
	if len(a) != len(b) {
		return false
	}
	for _, link := range a {
		if !slices.ContainsFunc(b, func(other titles.SubsetPackage) bool {
			return other.Version == link.Version && other.PackageID == link.PackageID
		}) {
			return false
		}
	}
	return true
}

func (c *client) listPatchPolicies(ctx context.Context, filter string) ([]patch_policies.ResourcePatchPolicySummary, error) {
	objects, err := c.listObjects(ctx, constants.EndpointJamfProPatchPoliciesV2, map[string]string{"filter": filter})
	if err != nil {
		return nil, err
	}
	policies := make([]patch_policies.ResourcePatchPolicySummary, 0, len(objects))
	for _, data := range objects {
		var policy patch_policies.ResourcePatchPolicySummary
		if err := json.Unmarshal(data, &policy); err != nil {
			return nil, errors.New("invalid Jamf patch policy summary")
		}
		policies = append(policies, policy)
	}
	return policies, nil
}

// observePolicy finds the policy by name within the title. It returns a nil
// policy when that name is free.
func (c *client) observePolicy(ctx context.Context, patch *patchConfig, name string) (string, *xmlNode, error) {
	policies, err := c.listPatchPolicies(ctx, "softwareTitleConfigurationId=="+patch.titleID)
	if err != nil {
		return "", nil, err
	}
	id := ""
	for _, p := range policies {
		if p.SoftwareTitleConfigurationID != patch.titleID || p.PolicyName != name {
			continue
		}
		if id != "" {
			return "", nil, fmt.Errorf("multiple Jamf patch policies are named %q in patch title %q; policy names must be unique", name, patch.Title)
		}
		id = p.ID
	}
	if id == "" {
		return "", nil, nil
	}
	policy, err := c.read(ctx, patchPolicies, id)
	if err != nil {
		return "", nil, err
	}
	if policy == nil {
		return "", nil, fmt.Errorf("jamf patch policy %s does not exist", id)
	}
	if policy.value("software_title_configuration_id") != patch.titleID {
		return "", nil, errors.New("jamf patch policy belongs to a different patch title")
	}
	return id, policy, nil
}

// recentDefinitions names the newest definitions a title offers, for an error
// about a version it lacks.
func recentDefinitions(definitions []titles.ResourceDefinition) string {
	if len(definitions) == 0 {
		return ""
	}
	sorted := slices.Clone(definitions)
	slices.SortStableFunc(sorted, func(a, b titles.ResourceDefinition) int { return strings.Compare(b.ReleaseDate, a.ReleaseDate) })
	versions := make([]string, 0, 5)
	for _, definition := range sorted[:min(5, len(sorted))] {
		versions = append(versions, definition.Version)
	}
	return "; recent definitions: " + strings.Join(versions, ", ")
}
