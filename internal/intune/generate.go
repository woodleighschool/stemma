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

func generate() error {
	for _, target := range []struct {
		version string
		pkg     string
		types   []string
	}{
		{"v1.0", "graphstable", []string{"win32LobApp"}},
		{"beta", "graphbeta", []string{"macOSDmgApp", "macOSPkgApp"}},
	} {
		base := "/deviceAppManagement/mobileApps"
		paths := []string{
			base + "#GET", base + "#POST",
			base + "/{mobileApp-id}#GET", base + "/{mobileApp-id}#PATCH",
			base + "/{mobileApp-id}/assignments#GET", base + "/{mobileApp-id}/assign#POST",
		}
		for _, appType := range target.types {
			versions := base + "/{mobileApp-id}/graph." + appType + "/contentVersions"
			files := versions + "/{mobileAppContent-id}/files"
			file := files + "/{mobileAppContentFile-id}"
			paths = append(paths, versions+"#POST", files+"#GET", files+"#POST", file+"#GET", file+"/commit#POST", file+"/renewUpload#POST")
		}
		args := []string{
			"generate", "--language", "Go",
			"--openapi", "https://raw.githubusercontent.com/microsoftgraph/msgraph-metadata/" + metadataRevision + "/openapi/" + target.version + "/openapi.yaml",
			"--output", "internal/" + target.pkg,
			"--namespace-name", "github.com/woodleighschool/stemma/internal/" + target.pkg,
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
			return fmt.Errorf("generate Graph %s: %w", target.version, err)
		}
	}
	return nil
}
