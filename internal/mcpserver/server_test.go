package mcpserver

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

	gogit "github.com/go-git/go-git/v6"
	"github.com/go-git/go-git/v6/plumbing/object"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/woodleighschool/stemma/internal/engine"
)

const project = `apiVersion: stemma/v1alpha1
kind: Project
metadata: {name: catalog}
spec:
  imports: ['software/*.yaml']
  components:
    app:
      destinations:
        repo: {pkginfo: {catalogs: [testing]}}
  destinations:
    repo: {operation: munki, config: {path: repo}}
`

// policy is a source-free resource the committed catalog already has.
const policy = `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: policy}
spec:
  destinations:
    repo:
      pkginfo:
        installer_type: nopkg
        version: '1'
        installcheck_script: |
          #!/bin/sh
          exit 1
`

const draft = `apiVersion: stemma/v1alpha1
kind: MacSoftware
metadata: {name: app}
spec:
  extends: app
  source: {path: app.pkg}
`

func TestToolsTakeADraftToACheckedChange(t *testing.T) {
	root := committedProject(t)
	session := connect(t, root)

	var overview description
	call(t, session, "describe", map[string]any{}, &overview)
	if !slices.Contains(overview.Kinds, kindSummary{Kind: "MacSoftware"}) || overview.Components["app"] == nil || len(overview.Unavailable) > 0 {
		t.Fatalf("overview = %+v", overview)
	}
	if len(overview.Destinations) != 1 || overview.Destinations[0].Name != "repo" || overview.Destinations[0].Operation != "munki" {
		t.Fatalf("destinations = %+v", overview.Destinations)
	}
	if overview.Resolvers[0].Name != "http" || !slices.Contains(overview.Resolvers[0].Fields, "url*") {
		t.Fatalf("resolvers = %+v", overview.Resolvers)
	}
	var detail description
	call(t, session, "describe", map[string]any{"destination": "repo", "field": "pkginfo.catalogs"}, &detail)
	if len(detail.Fields) != 1 || !strings.HasPrefix(detail.Fields[0], "pkginfo.catalogs (list of string") {
		t.Fatalf("fields = %q", detail.Fields)
	}

	write(t, filepath.Join(root, "software", "app.yaml"), draft)
	lockfile := filepath.Join(root, "stemma.lock.yaml")
	var trial preparation
	call(t, session, "prepare", map[string]any{"resources": []string{"MacSoftware/app"}}, &trial)
	if len(trial.Resources) != 1 || trial.Resources[0].Resource != "MacSoftware/app" || trial.Resources[0].Error != "" {
		t.Fatalf("prepare = %+v", trial)
	}
	app := trial.Resources[0]
	if len(app.Inputs) != 1 || app.Inputs[0].Change != "added" || app.Inputs[0].Resolver != "file" || app.Inputs[0].SHA256 == "" {
		t.Fatalf("inputs = %+v", app.Inputs)
	}
	if len(app.Artifacts) != 1 || app.Artifacts[0].Version != "1.2.3" || !strings.Contains(app.Artifacts[0].Signatures, "signer: apple:developer-id:SMLKBTR495") || len(app.Artifacts[0].Subjects) == 0 {
		t.Fatalf("artifacts = %+v", app.Artifacts)
	}
	if _, err := os.Stat(lockfile); !os.IsNotExist(err) {
		t.Fatalf("prepare wrote the lockfile: %v", err)
	}

	var update lockUpdate
	call(t, session, "update", map[string]any{"resources": []string{"MacSoftware/app"}}, &update)
	if !update.Changed || len(update.Resources) != 1 || len(update.Resources[0].Inputs) != 1 || update.Resources[0].Inputs[0].Change != "added" {
		t.Fatalf("update = %+v", update)
	}
	var locked preparation
	call(t, session, "prepare", map[string]any{"resources": []string{"MacSoftware/app"}}, &locked)
	if len(locked.Resources) != 1 || len(locked.Resources[0].Inputs) > 0 {
		t.Fatalf("prepare after update = %+v", locked)
	}

	var outcome icons
	call(t, session, "icon", map[string]any{"resources": []string{"MacSoftware/app"}}, &outcome)
	if len(outcome.Resources) != 1 || outcome.Resources[0].Icon != "no icon declared" {
		t.Fatalf("icon = %+v", outcome)
	}

	// The check prepares from the lockfile what the trial prepared from the
	// same source, so it finds that preparation cached.
	var check checkResult
	call(t, session, "check", map[string]any{"since": "HEAD"}, &check)
	if !check.Valid || len(check.Resources) != 1 || check.Resources[0].Resource != "MacSoftware/app" || check.Resources[0].Status != "cached" {
		t.Fatalf("check = %+v", check)
	}
}

func TestToolsReportFailuresAsToolErrors(t *testing.T) {
	root := committedProject(t)
	session := connect(t, root)
	tests := []struct {
		name      string
		tool      string
		arguments map[string]any
		want      string
	}{
		{"no resources", "prepare", map[string]any{"resources": []string{}}, "name at least one resource"},
		{"two subjects", "describe", map[string]any{"kind": "MacSoftware", "destination": "repo"}, "name one of kind, resolver or destination"},
		{"unknown kind", "describe", map[string]any{"kind": "Nope"}, "unknown kind Nope; kinds are "},
		{"unknown destination", "describe", map[string]any{"destination": "nope"}, "unknown destination nope; destinations are repo"},
		{"unknown field", "describe", map[string]any{"kind": "MacSoftware", "field": "bogus"}, "no field bogus; fields are "},
		{"no revision", "check", map[string]any{"since": ""}, "since names the Git revision"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := callFailing(t, session, test.tool, test.arguments)
			if text := result.Content[0].(*mcp.TextContent).Text; !strings.Contains(text, test.want) {
				t.Fatalf("error = %q, want %q", text, test.want)
			}
		})
	}

	write(t, filepath.Join(root, "software", "app.yaml"), strings.Replace(draft, "extends: app", "extends: app\n  bogus: true", 1))
	var check checkResult
	decodeResult(t, callFailing(t, session, "check", map[string]any{"since": "HEAD"}), &check)
	if check.Valid || !strings.Contains(check.Error, "bogus") {
		t.Fatalf("check = %+v", check)
	}
	var overview description
	call(t, session, "describe", map[string]any{}, &overview)
	if !slices.Contains(overview.Kinds, kindSummary{Kind: "MacSoftware"}) {
		t.Fatalf("describe with a broken draft = %+v", overview)
	}
}

func TestCheckAcceptsPluginChanges(t *testing.T) {
	root := committedProject(t)
	session := connect(t, root)
	binary := filepath.Join(root, "local-plugin", "plugin")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../../plugin/testdata/echo")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build plugin: %v\n%s", err, output)
	}
	changed := strings.Replace(project, "  components:", "  plugins:\n    provider: {path: local-plugin}\n  components:", 1)
	changed = strings.Replace(changed, "operation: munki, config: {path: repo}", "operation: echo.reconcile", 1)
	write(t, filepath.Join(root, "stemma.yaml"), changed)
	if _, err := engine.UpdatePlugins(t.Context(), engine.Options{ConfigPath: filepath.Join(root, "stemma.yaml"), CacheDir: t.TempDir()}, nil); err != nil {
		t.Fatal(err)
	}
	var result checkResult
	call(t, session, "check", map[string]any{"since": "HEAD"}, &result)
	if !result.Valid || len(result.Resources) != 0 {
		t.Fatalf("check = %+v", result)
	}
}

// committedProject commits a project with one source-free resource as the Git
// revision HEAD, with the fixture installer beside the resource documents.
func committedProject(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	write(t, filepath.Join(root, "stemma.yaml"), project)
	write(t, filepath.Join(root, "software", "policy.yaml"), policy)
	installer, err := os.ReadFile("../apple/testdata/fixture.pkg")
	if err != nil {
		t.Fatal(err)
	}
	write(t, filepath.Join(root, "software", "app.pkg"), string(installer))
	repo, err := gogit.PlainInit(root, false)
	if err != nil {
		t.Fatal(err)
	}
	tree, err := repo.Worktree()
	if err != nil {
		t.Fatal(err)
	}
	if err := tree.AddWithOptions(&gogit.AddOptions{All: true}); err != nil {
		t.Fatal(err)
	}
	signature := &object.Signature{Name: "Human", Email: "human@example.com", When: time.Now()}
	if _, err := tree.Commit("catalog", &gogit.CommitOptions{Author: signature, Committer: signature}); err != nil {
		t.Fatal(err)
	}
	return root
}

func connect(t *testing.T, root string) *mcp.ClientSession {
	t.Helper()
	serverTransport, clientTransport := mcp.NewInMemoryTransports()
	server := newServer(Options{ConfigPath: filepath.Join(root, "stemma.yaml"), CacheDir: t.TempDir(), Version: "test"})
	serverSession, err := server.Connect(t.Context(), serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = serverSession.Close() })
	session, err := mcp.NewClient(&mcp.Implementation{Name: "test"}, nil).Connect(t.Context(), clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })
	return session
}

func call(t *testing.T, session *mcp.ClientSession, tool string, arguments, result any) {
	t.Helper()
	response := callTool(t, session, tool, arguments)
	if response.IsError {
		t.Fatalf("%s: %s", tool, response.Content[0].(*mcp.TextContent).Text)
	}
	decodeResult(t, response, result)
}

func callFailing(t *testing.T, session *mcp.ClientSession, tool string, arguments any) *mcp.CallToolResult {
	t.Helper()
	response := callTool(t, session, tool, arguments)
	if !response.IsError {
		t.Fatalf("%s succeeded: %v", tool, response.StructuredContent)
	}
	return response
}

func callTool(t *testing.T, session *mcp.ClientSession, tool string, arguments any) *mcp.CallToolResult {
	t.Helper()
	response, err := session.CallTool(t.Context(), &mcp.CallToolParams{Name: tool, Arguments: arguments})
	if err != nil {
		t.Fatalf("%s: %v", tool, err)
	}
	return response
}

func decodeResult(t *testing.T, response *mcp.CallToolResult, result any) {
	t.Helper()
	data, err := json.Marshal(response.StructuredContent)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, result); err != nil {
		t.Fatalf("decode %s: %v", data, err)
	}
}

func write(t *testing.T, name, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestArtifactDescriptionKeepsUnsignedObservations(t *testing.T) {
	artifact := describeArtifact("installer", engine.Prepared{Filename: "unsigned.pkg", Evidence: map[string]json.RawMessage{"signatures": json.RawMessage(`[{"subject":{"path":"."},"state":"unsigned","verifier":"stemma.signature/2"}]`)}})
	if !strings.Contains(artifact.Signatures, "unsigned: true") || !strings.Contains(artifact.Signatures, `path: "."`) {
		t.Fatalf("unsigned fragment: %s", artifact.Signatures)
	}
	observations, ok := artifact.Evidence["signatures"].([]any)
	if !ok || len(observations) != 1 || observations[0].(map[string]any)["state"] != "unsigned" {
		t.Fatalf("lost structured observations: %+v", artifact.Evidence)
	}
}
