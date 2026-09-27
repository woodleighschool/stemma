//go:build ignore

// Regenerate with mise run generate-graph.
package main

import (
	"fmt"
	"os"
	"os/exec"
)

// Keep the OpenAPI input immutable so regeneration is reproducible.
const metadataRevision = "0d13ff1"

func main() {
	if err := generate(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// generate writes the scoped Graph beta client. Every app type uses beta:
// v1.0 cannot read back a Win32 app's combined allowed architectures, and the
// macOS app types exist only in beta.
func generate() error {
	base := "/deviceAppManagement/mobileApps"
	paths := []string{
		base + "#GET", base + "#POST",
		base + "/{mobileApp-id}#GET", base + "/{mobileApp-id}#PATCH",
		base + "/{mobileApp-id}/assignments#GET", base + "/{mobileApp-id}/assign#POST",
		base + "/{mobileApp-id}/relationships#GET", base + "/{mobileApp-id}/updateRelationships#POST",
	}
	for _, appType := range []string{"macOSDmgApp", "macOSLobApp", "macOSPkgApp", "win32LobApp"} {
		versions := base + "/{mobileApp-id}/graph." + appType + "/contentVersions"
		files := versions + "/{mobileAppContent-id}/files"
		file := files + "/{mobileAppContentFile-id}"
		paths = append(paths, versions+"#GET", versions+"#POST", versions+"/{mobileAppContent-id}#DELETE", files+"#GET", files+"#POST", file+"#GET", file+"/commit#POST", file+"/renewUpload#POST")
	}
	args := []string{
		"generate", "--language", "Go",
		"--openapi", "https://raw.githubusercontent.com/microsoftgraph/msgraph-metadata/" + metadataRevision + "/openapi/beta/openapi.yaml",
		"--output", "internal/intune/graph/beta",
		"--namespace-name", "github.com/woodleighschool/stemma/internal/intune/graph/beta",
		"--exclude-backward-compatible", "--additional-data", "--clean-output",
		// Exclude JSON from model generation while keeping application/json on the wire.
		"--structured-mime-types", "text/plain",
	}
	for _, path := range paths {
		args = append(args, "--include-path", path)
	}
	cmd := exec.Command("kiota", args...)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("generate Graph beta: %w", err)
	}
	return nil
}
