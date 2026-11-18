package lock

import (
	"encoding/json"
	"maps"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/source"
)

const unityHub = `version: 6
inputs:
  source:
    version: 4.0.0
    content:
      filename: UnityHubSetup-4.0.0-arm64.dmg
      sha256: 253ba613bb06e9e24a1f97d1ac194255ad4cf72207d2194f40b958bfd49f64cf
    origin:
      url: https://public-cdn.cloud.unity3d.com/hub/prod/4.0.0/UnityHubSetup-4.0.0-arm64.dmg
    resolver: homebrew
    resolver_version: "2"
    declaration: bcc1ec8256006de82d8cd0973378c496f98d5f067637e55be9713cbb8f22aca8
    evidence:
      homebrew:
        cask:
          artifacts:
            - app:
                - Unity Hub.app
              target: /Applications/Unity Hub.app
          macos: "27"
          name: unity-hub
          target_architecture: arm64
`

func TestFormatSixStatesEachFactOnce(t *testing.T) {
	const document = "stemma.lock/MacSoftware/unity-hub.yaml"
	file, err := Decode(map[string][]byte{document: []byte(unityHub)})
	if err != nil {
		t.Fatal(err)
	}
	locked := file.Inputs["stemma/v1alpha1/MacSoftware/unity-hub"]["source"]
	if locked.Version != "4.0.0" || locked.Content.Filename != "UnityHubSetup-4.0.0-arm64.dmg" || locked.Content.Mode != 0o644 || locked.Content.Tree || string(locked.Origin) != `{"url":"https://public-cdn.cloud.unity3d.com/hub/prod/4.0.0/UnityHubSetup-4.0.0-arm64.dmg"}` {
		t.Fatalf("decoded entry = %+v", locked)
	}
	files, err := Encode(file)
	if err != nil {
		t.Fatal(err)
	}
	if got := string(files[document]); got != unityHub {
		t.Fatalf("encoded document:\n%s\nwant:\n%s", got, unityHub)
	}
}

func TestContentModeIsOctalAndOmittedAtItsDefault(t *testing.T) {
	const document = "stemma.lock/Software/app.yaml"
	digest := strings.Repeat("b", 64)
	for _, tc := range []struct {
		content source.Content
		line    string
	}{
		{source.Content{Filename: "app.pkg", SHA256: digest, Mode: 0o644}, ""},
		{source.Content{Filename: "tree", SHA256: digest, Mode: 0o755, Tree: true}, ""},
		{source.Content{Filename: "tool", SHA256: digest, Mode: 0o755}, `      mode: "0755"` + "\n"},
		{source.Content{Filename: "tree", SHA256: digest, Mode: 0o700, Tree: true}, `      mode: "0700"` + "\n"},
	} {
		entry := source.Entry{Content: tc.content, Resolver: "file", ResolverVersion: "1", Declaration: strings.Repeat("a", 64)}
		files, err := Encode(File{Inputs: map[string]map[string]source.Entry{resource: {"source": entry}}})
		if err != nil {
			t.Fatal(err)
		}
		data := string(files[document])
		if tc.line == "" && strings.Contains(data, "mode:") || tc.line != "" && !strings.Contains(data, tc.line) {
			t.Errorf("mode %04o (tree %v) encoded as:\n%s", tc.content.Mode, tc.content.Tree, data)
		}
		decoded, err := Decode(map[string][]byte{document: []byte(data)})
		if err != nil {
			t.Fatal(err)
		}
		if got := decoded.Inputs[resource]["source"].Content; got != tc.content {
			t.Errorf("mode round trip = %+v, want %+v", got, tc.content)
		}
	}
}

func TestLockEvidenceNamesAreResolverNamespaces(t *testing.T) {
	const document = "stemma.lock/MacSoftware/unity-hub.yaml"
	for name, want := range map[string]string{
		"homebrew.cask": `resolver evidence "homebrew.cask" must be one namespace name`,
		"source":        `resolver evidence "source" is a namespace Stemma writes`,
	} {
		data := strings.Replace(unityHub, "      homebrew:\n        cask:\n", "      "+name+":\n        cask:\n", 1)
		if _, err := Decode(map[string][]byte{document: []byte(data)}); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("evidence %s: %v, want %q", name, err, want)
		}
	}
}

func TestDiffInputsNamesWhatChanged(t *testing.T) {
	base := source.Entry{
		Version:         "1.0",
		Content:         source.Content{Filename: "app.pkg", SHA256: strings.Repeat("a", 64), Mode: 0o644},
		Origin:          json.RawMessage(`{"url":"https://example.test/app.pkg"}`),
		Resolver:        "http",
		ResolverVersion: "1",
		Declaration:     strings.Repeat("d", 64),
	}
	newer := base
	newer.Version, newer.Content.SHA256 = "1.1", strings.Repeat("b", 64)
	moved := base
	moved.Origin = json.RawMessage(`{"url":"https://mirror.example.test/app.pkg"}`)
	before := map[string]map[string]source.Entry{resource: {"kept": base, "updated": base, "refreshed": base, "removed": base}}
	after := map[string]map[string]source.Entry{resource: {"kept": base, "updated": newer, "refreshed": moved, "added": base}}
	got := map[string]ChangeKind{}
	for _, change := range DiffInputs(before, after) {
		got[change.Input] = change.Kind
	}
	want := map[string]ChangeKind{"added": Added, "removed": Removed, "updated": Updated, "refreshed": Refreshed}
	if !maps.Equal(got, want) {
		t.Fatalf("changes = %v, want %v", got, want)
	}
}
