package jamf

import (
	"slices"
	"testing"

	titles "github.com/deploymenttheory/go-sdk-jamfpro-v2/jamfpro/jamf_pro_api/patch_software_title_configurations"
	"github.com/woodleighschool/stemma/plugin"
)

func TestRetentionApplyHonorsObservedReferences(t *testing.T) {
	for _, kind := range []string{"install_policy", "patch_title"} {
		t.Run(kind, func(t *testing.T) {
			server, request := newNativeFixture(t)
			server.seed(map[string]any{"fileName": "vendor-old.pkg", "notes": fixtureMarker})
			var patch *patchConfig
			var installs []*installPolicy
			switch kind {
			case "install_policy":
				server.native.seedPolicy("20", "Managed install", "1")
				installs = []*installPolicy{{id: "20"}}
			case "patch_title":
				server.native.titles["5"].Packages = []titles.SubsetPackage{{PackageID: "1", Version: "1.0"}}
				patch = &patchConfig{titleID: "5", version: "1.0"}
			}
			c, err := newClient(t.Context(), request.Config)
			if err != nil {
				t.Fatal(err)
			}
			retiring := []summary{{ID: 1, FileName: "vendor-old.pkg"}}
			var plan plugin.ReconcileResponse
			if err := c.prune(t.Context(), retiring, patch, installs, false, &plan); err != nil {
				t.Fatal(err)
			}
			if !slices.Equal(deletions(plan), []string{"1"}) {
				t.Fatalf("plan did not release the managed reference: %+v", plan.Changes)
			}
			// Another writer can restore the reference after reconciliation.
			// Pruning must honor the reference returned by its own fresh read.
			var applied plugin.ReconcileResponse
			if err := c.prune(t.Context(), retiring, patch, installs, true, &applied); err != nil {
				t.Fatal(err)
			}
			if len(deletions(applied)) != 0 || !server.exists("1") || server.count("DELETE "+packagePath+"/1") != 0 {
				t.Fatalf("apply deleted the package still referenced by %s: %+v", kind, applied.Changes)
			}
		})
	}
}
