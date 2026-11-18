package intune

import (
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/plugin"
)

func TestCategoriesOwnTheCompleteSet(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.categories = []object{{"id": "cat-1", "displayName": "Productivity"}, {"id": "cat-2", "displayName": "Other apps"}, {"id": "cat-3", "displayName": "Business"}}
	req := fixtureRequest(t)
	run := func(metadata object) (plugin.ReconcileResponse, error) {
		t.Helper()
		req.Metadata = raw(metadata)
		desired, err := compileRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		return reconcileFixture(t, c, req, desired)
	}
	held := func() []string {
		fake.mu.Lock()
		defer fake.mu.Unlock()
		return slices.Sorted(slices.Values(fake.appCategories))
	}
	created, err := decodeObject(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	created["categories"] = []any{"Productivity", "Other apps"}
	if _, err := run(created); err != nil {
		t.Fatal(err)
	}
	if got := held(); !slices.Equal(got, []string{"cat-1", "cat-2"}) {
		t.Fatalf("created app categories = %v", got)
	}

	response, err := run(object{"categories": []any{"Business", "Productivity"}})
	if err != nil {
		t.Fatal(err)
	}
	if got := held(); !slices.Equal(got, []string{"cat-1", "cat-3"}) {
		t.Fatalf("replaced app categories = %v", got)
	}
	if want := []plugin.FieldChange{{Field: "categories", Before: json.RawMessage(`["Other apps","Productivity"]`), After: json.RawMessage(`["Business","Productivity"]`)}}; !reflect.DeepEqual(response.Fields, want) {
		t.Fatalf("category fields = %+v", response.Fields)
	}

	fake.mu.Lock()
	writes := fake.writes
	fake.mu.Unlock()
	response, err = run(object{"categories": []any{"Productivity", "Business"}})
	if err != nil || response.Changed() {
		t.Fatalf("unchanged categories: %+v, %v", response, err)
	}
	if _, err := run(object{"display_name": "Renamed"}); err != nil {
		t.Fatal(err)
	}
	if got := held(); !slices.Equal(got, []string{"cat-1", "cat-3"}) {
		t.Fatalf("omitted categories changed to %v", got)
	}
	fake.mu.Lock()
	if fake.writes != writes+1 {
		t.Fatalf("unchanged categories wrote to the tenant: %d writes", fake.writes-writes)
	}
	fake.mu.Unlock()

	response, err = run(object{"categories": []any{"games", "Other apps"}})
	if err != nil {
		t.Fatal(err)
	}
	// A category the run creates is a member like any other.
	if want := []plugin.FieldChange{{Field: "categories", Before: json.RawMessage(`["Business","Productivity"]`), After: json.RawMessage(`["Other apps","games"]`)}}; !reflect.DeepEqual(response.Fields, want) {
		t.Fatalf("new category fields = %+v", response.Fields)
	}
	fake.mu.Lock()
	newest := fake.categories[len(fake.categories)-1]
	fake.mu.Unlock()
	if newest["displayName"] != "games" {
		t.Fatalf("created category %+v", newest)
	}
	if got := held(); !slices.Equal(got, []string{"cat-2", text(newest["id"])}) {
		t.Fatalf("categories after creation = %v", got)
	}
	response, err = run(object{"categories": []any{"games", "Other apps"}})
	if err != nil || response.Changed() {
		t.Fatalf("unchanged categories: %+v, %v", response, err)
	}

	fake.mu.Lock()
	fake.categories = append(fake.categories, object{"id": "cat-9", "displayName": "Business"})
	writes = fake.writes
	fake.mu.Unlock()
	if _, err := run(object{"categories": []any{"Business"}}); err == nil || !strings.Contains(err.Error(), `multiple Intune app categories are named "Business"`) {
		t.Fatalf("ambiguous category error = %v", err)
	}
	fake.mu.Lock()
	if fake.writes != writes {
		t.Fatal("an ambiguous category wrote to the tenant")
	}
	fake.mu.Unlock()

	if _, err := run(object{"categories": []any{}}); err != nil {
		t.Fatal(err)
	}
	if got := held(); len(got) != 0 {
		t.Fatalf("[] left categories %v", got)
	}
}

func TestCategoriesUseExactNames(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.categories = []object{{"id": "cat-1", "displayName": "Business"}, {"id": "cat-2", "displayName": "business"}}
	req := fixtureRequest(t)
	metadata, err := decodeObject(req.Metadata)
	if err != nil {
		t.Fatal(err)
	}
	metadata["categories"] = []any{"Business", "business", "BUSINESS"}
	req.Metadata = raw(metadata)
	desired, err := compileRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	req.Method = "plan"
	response, err := reconcileFixture(t, c, req, desired)
	if err != nil {
		t.Fatal(err)
	}
	if fake.writes != 0 || !slices.ContainsFunc(response.Fields, func(field plugin.FieldChange) bool {
		return field.Field == "categories" && field.Before == nil && string(field.After) == `["BUSINESS","Business","business"]`
	}) {
		t.Fatalf("category plan: %+v, writes=%d", response.Fields, fake.writes)
	}
	req.Method = "apply"
	if _, err := reconcileFixture(t, c, req, desired); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(fake.appCategories, []string{"cat-1", "cat-2", "cat-3"}) || fake.categories[2]["displayName"] != "BUSINESS" {
		t.Fatalf("case-distinct categories were merged: %+v", fake.categories)
	}
}

func TestCategoryNamesAreValidatedBeforeConnecting(t *testing.T) {
	for _, categories := range []any{"Productivity", []any{"Productivity", "Productivity"}, []any{""}, []any{1}} {
		req := fixtureRequest(t)
		req.Metadata = raw(object{"categories": categories})
		if _, err := compileRequest(req); err == nil {
			t.Fatalf("accepted categories %#v", categories)
		}
	}
}

func TestCategoryFailureRetainsConfirmedMembershipChanges(t *testing.T) {
	fake, c := newGraphFixture(t)
	fake.categories = []object{{"id": "old", "displayName": "Old"}, {"id": "new", "displayName": "New"}}
	req := fixtureRequest(t)
	if _, err := reconcileRequest(t, c, req); err != nil {
		t.Fatal(err)
	}
	fake.appCategories, fake.failCategoryRemoval = []string{"old"}, true
	req.Metadata = raw(object{"categories": []any{"New"}})
	result, err := reconcileRequest(t, c, req)
	if err == nil || !slices.Equal(fake.appCategories, []string{"old", "new"}) {
		t.Fatalf("fixture did not partially apply: %+v, %v", fake.appCategories, err)
	}
	want := []plugin.FieldChange{{Field: "categories", Before: raw([]string{"Old"}), After: raw([]string{"New", "Old"})}}
	if !reflect.DeepEqual(result.Fields, want) {
		t.Fatalf("partial membership: %+v", result)
	}
	fake.failCategoryRemoval = false
	if _, err := reconcileRequest(t, c, req); err != nil || !slices.Equal(fake.appCategories, []string{"new"}) {
		t.Fatalf("retry: %v %v", fake.appCategories, err)
	}
}
