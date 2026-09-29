package intune

import (
	"context"
	"errors"
	"fmt"
	"slices"

	abs "github.com/microsoft/kiota-abstractions-go"
	"github.com/woodleighschool/stemma/plugin"
)

// Missing categories have no ID until apply creates them.
func (c *client) resolveCategories(ctx context.Context, names []any) ([]object, error) {
	tenant, err := c.list(ctx, c.categories())
	if err != nil {
		return nil, fmt.Errorf("list Intune app categories: %w", err)
	}
	resolved := make([]object, 0, len(names))
	for _, value := range names {
		name := text(value)
		var found []object
		for _, category := range tenant {
			if text(category["displayName"]) == name {
				found = append(found, category)
			}
		}
		switch len(found) {
		case 0:
			resolved = append(resolved, object{"id": "", "displayName": name})
		case 1:
			resolved = append(resolved, found[0])
		default:
			return nil, fmt.Errorf("multiple Intune app categories are named %q", name)
		}
	}
	return resolved, nil
}

func (c *client) reconcileCategories(ctx context.Context, appID string, names []any) error {
	resolved, err := c.resolveCategories(ctx, names)
	if err != nil {
		return err
	}
	for _, category := range resolved {
		if text(category["id"]) != "" {
			continue
		}
		var created object
		if err := c.request(ctx, abs.POST, c.categories(), object{"@odata.type": "#microsoft.graph.mobileAppCategory", "displayName": category["displayName"]}, &created); err != nil {
			return fmt.Errorf("create Intune app category %q: %w", category["displayName"], err)
		}
		if category["id"] = text(created["id"]); category["id"] == "" {
			return errors.New("intune category creation omitted ID")
		}
	}
	ids := categoryIDs(resolved)
	current, err := c.list(ctx, c.appCategories(appID))
	if err != nil {
		return err
	}
	held := categoryIDs(current)
	for _, id := range ids {
		if !slices.Contains(held, id) {
			ref := object{"@odata.id": c.beta.PathParameters["baseurl"] + "/deviceAppManagement/mobileAppCategories/" + id}
			if err := c.request(ctx, abs.POST, c.categoryRef(appID, ""), ref, nil); err != nil {
				return fmt.Errorf("add Intune app category: %w", err)
			}
		}
	}
	for _, id := range held {
		if !slices.Contains(ids, id) {
			if err := c.request(ctx, abs.DELETE, c.categoryRef(appID, id), nil, nil); err != nil {
				return fmt.Errorf("remove Intune app category: %w", err)
			}
		}
	}
	readback, err := c.list(ctx, c.appCategories(appID))
	if err != nil {
		return err
	}
	if !sameCategories(readback, ids) {
		return errors.New("intune category readback differs from requested categories")
	}
	return nil
}

func categoryIDs(categories []object) []string {
	ids := make([]string, 0, len(categories))
	for _, category := range categories {
		ids = append(ids, text(category["id"]))
	}
	return ids
}

func sameCategories(current []object, ids []string) bool {
	held := categoryIDs(current)
	slices.Sort(held)
	wanted := slices.Sorted(slices.Values(ids))
	return slices.Equal(held, wanted)
}

func categoryList(categories []object) []string {
	names := make([]string, 0, len(categories))
	for _, category := range categories {
		names = append(names, text(category["displayName"]))
	}
	slices.Sort(names)
	return names
}

func (c *client) planCategories(ctx context.Context, current object, names []any) ([]plugin.Change, bool, error) {
	categories, err := c.resolveCategories(ctx, names)
	if err != nil {
		return nil, false, err
	}
	var changes []plugin.Change
	for _, category := range categories {
		if text(category["id"]) == "" {
			changes = append(changes, plugin.Change{Kind: "categories", Field: "category", Action: "create", After: raw(category["displayName"])})
		}
	}
	var existing []object
	if current != nil {
		if existing, err = c.list(ctx, c.appCategories(text(current["id"]))); err != nil {
			return nil, false, err
		}
	}
	if sameCategories(existing, categoryIDs(categories)) {
		return changes, false, nil
	}
	changes = append(changes, plugin.Change{Kind: "categories", Field: "categories", Action: "replace", Before: raw(categoryList(existing)), After: raw(categoryList(categories))})
	return changes, true, nil
}
