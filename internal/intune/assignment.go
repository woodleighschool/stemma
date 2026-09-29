package intune

import (
	"context"
	"errors"
	"fmt"
	"maps"

	abs "github.com/microsoft/kiota-abstractions-go"
	"github.com/woodleighschool/stemma/plugin"
)

var (
	intents       = map[string]string{"required": "required", "available": "available", "uninstall": "uninstall", "available_without_enrollment": "availableWithoutEnrollment"}
	filterModes   = map[string]string{"include": "include", "exclude": "exclude"}
	notifications = map[string]string{"show_all": "showAll", "show_reboot": "showReboot", "hide_all": "hideAll"}
)

// assignments translates each deployment intent and its one target: an Entra
// group, an excluded group, all devices or all users. An included target may
// carry an assignment filter and a notification setting, properties of that
// assignment as in Graph; omitting them keeps what the assignment has.
func assignments(value any) (any, error) {
	list, ok := value.([]any)
	if !ok || len(list) > 1000 {
		return nil, errors.New("must be an array of at most 1000 items; [] clears assignments")
	}
	result := make([]any, 0, len(list))
	seen := map[string]bool{}
	for _, item := range list {
		assignment, ok := item.(object)
		if !ok {
			return nil, errors.New("assignment must be an object")
		}
		if err := fields(assignment, "intent", "group", "exclude_group", "all_devices", "all_users", "filter", "notifications"); err != nil {
			return nil, err
		}
		intent, err := choice(assignment["intent"], intents)
		if err != nil {
			return nil, fmt.Errorf("intent %w", err)
		}
		var targets []object
		for key, kind := range map[string]string{"group": "groupAssignmentTarget", "exclude_group": "exclusionGroupAssignmentTarget"} {
			if value, exists := assignment[key]; exists {
				if text(value) == "" {
					return nil, fmt.Errorf("%s must be an Entra group ID", key)
				}
				targets = append(targets, object{"@odata.type": "#microsoft.graph." + kind, "groupId": value})
			}
		}
		for key, kind := range map[string]string{"all_devices": "allDevicesAssignmentTarget", "all_users": "allLicensedUsersAssignmentTarget"} {
			if value, exists := assignment[key]; exists {
				if value != true {
					return nil, fmt.Errorf("%s must be true", key)
				}
				targets = append(targets, object{"@odata.type": "#microsoft.graph." + kind})
			}
		}
		if len(targets) != 1 {
			return nil, errors.New("assignment requires exactly one of group, exclude_group, all_devices or all_users")
		}
		native := object{"intent": intent, "target": targets[0]}
		_, excluded := assignment["exclude_group"]
		if value, exists := assignment["filter"]; exists {
			if excluded {
				return nil, errors.New("an excluded group takes no filter")
			}
			filter, err := assignmentFilter(value)
			if err != nil {
				return nil, fmt.Errorf("filter %w", err)
			}
			maps.Copy(targets[0], filter)
		}
		if value, exists := assignment["notifications"]; exists {
			if excluded {
				return nil, errors.New("an excluded group takes no notifications")
			}
			setting, err := choice(value, notifications)
			if err != nil {
				return nil, fmt.Errorf("notifications %w", err)
			}
			native["settings"] = object{"@odata.type": "#microsoft.graph.win32LobAppAssignmentSettings", "notifications": setting}
		}
		key := assignmentKey(native)
		if seen[key] {
			return nil, errors.New("duplicate assignment target and intent")
		}
		seen[key] = true
		result = append(result, native)
	}
	return result, nil
}

// assignmentFilter translates a filter reference into its target properties;
// null removes the filter.
func assignmentFilter(value any) (object, error) {
	if value == nil {
		return object{"deviceAndAppManagementAssignmentFilterId": nil, "deviceAndAppManagementAssignmentFilterType": "none"}, nil
	}
	filter, ok := value.(object)
	if !ok {
		return nil, errors.New("must be an object with id and mode, or null")
	}
	if err := fields(filter, "id", "mode"); err != nil {
		return nil, err
	}
	if text(filter["id"]) == "" {
		return nil, errors.New("id must be an Intune assignment filter ID")
	}
	mode, err := choice(filter["mode"], filterModes)
	if err != nil {
		return nil, fmt.Errorf("mode %w", err)
	}
	return object{"deviceAndAppManagementAssignmentFilterId": filter["id"], "deviceAndAppManagementAssignmentFilterType": mode}, nil
}

func assignmentKey(value object) string {
	target, _ := value["target"].(object)
	return string(raw([]any{value["intent"], target["@odata.type"], target["groupId"]}))
}

func reconcileAssignments(current, desired []any) ([]any, bool) {
	byKey := map[string]object{}
	for _, item := range current {
		if v, ok := item.(object); ok {
			byKey[assignmentKey(v)] = v
		}
	}
	result := make([]any, 0, len(desired))
	changed := len(current) != len(desired)
	for _, item := range desired {
		v := item.(object)
		previous, ok := byKey[assignmentKey(v)]
		if !ok || !ownedEqual(previous, v) {
			changed = true
		}
		merged := mergeOwned(previous, v)
		delete(merged, "id")
		delete(merged, "@odata.context")
		result = append(result, merged)
	}
	return result, changed
}

func (c *client) currentAssignments(ctx context.Context, appID string) ([]any, error) {
	items, err := c.list(ctx, c.assignments(appID))
	if err != nil {
		return nil, err
	}
	existing := make([]any, 0, len(items))
	for _, item := range items {
		existing = append(existing, item)
	}
	return existing, nil
}

func (c *client) planAssignments(ctx context.Context, current object, declared []any) (*plugin.Change, error) {
	var existing []any
	if current != nil {
		var err error
		if existing, err = c.currentAssignments(ctx, text(current["id"])); err != nil {
			return nil, err
		}
	}
	assignments, changed := reconcileAssignments(existing, declared)
	if !changed {
		return nil, nil
	}
	return &plugin.Change{Kind: "assignments", Field: "assignments", Action: "replace", Before: raw(existing), After: raw(assignments)}, nil
}

// applyAssignments replaces the app's assignments with the declared ones,
// merged over those it holds now, and verifies them by readback.
func (c *client) applyAssignments(ctx context.Context, appID string, declared []any) error {
	existing, err := c.currentAssignments(ctx, appID)
	if err != nil {
		return err
	}
	assignments, _ := reconcileAssignments(existing, declared)
	if err := c.request(ctx, abs.POST, c.assign(appID), object{"mobileAppAssignments": assignments}, nil); err != nil {
		return err
	}
	readback, err := c.currentAssignments(ctx, appID)
	if err != nil {
		return err
	}
	if _, changed := reconcileAssignments(readback, declared); changed {
		return errors.New("intune assignments readback differs from requested targeting")
	}
	return nil
}
