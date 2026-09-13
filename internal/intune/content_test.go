package intune

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/woodleighschool/stemma/internal/archive"
	"github.com/woodleighschool/stemma/plugin"
)

func treeArtifact(t *testing.T) plugin.Artifact {
	t.Helper()
	root := t.TempDir()
	if err := os.Mkdir(filepath.Join(root, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	for name, body := range map[string]string{"bin/setup.cmd": "@echo off\r\n", "bin/repair.cmd": "@echo off\r\n", "payload.cab": "synthetic companion bytes"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	var packed bytes.Buffer
	if err := archive.Pack(t.Context(), root, &packed); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(packed.Bytes())
	return plugin.Artifact{Path: root, Tree: true, EntryPoint: "bin/setup.cmd", SHA256: hex.EncodeToString(digest[:]), Size: int64(packed.Len())}
}

func TestSetupTreePublicationAndEntrypointIdentity(t *testing.T) {
	fake, c := newGraphFixture(t)
	req := fixtureRequest(t)
	req.Artifact = treeArtifact(t)
	req.Artifact.Version = "4.2"
	desired, _ := validateMetadata(req.Metadata)
	delete(desired, "assignments")
	desired["content"] = object{"setup_file": `bin\setup.cmd`}
	response, err := c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	first := readBinding(t, response.Binding)
	fake.mu.Lock()
	payload := bytes.Clone(fake.plaintext)
	if fake.app["setupFilePath"] != `bin\setup.cmd` || fake.app["fileName"] != "test-4.2.intunewin" || fake.app["content"] != nil {
		t.Fatalf("provider content options leaked or incorrect Graph entrypoint: %+v", fake.app)
	}
	fake.mu.Unlock()
	reader, err := zip.NewReader(bytes.NewReader(payload), int64(len(payload)))
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"bin/setup.cmd", "bin/repair.cmd", "payload.cab"} {
		file, err := reader.Open(name)
		if err != nil {
			t.Fatalf("payload omitted %s: %v", name, err)
		}
		data, err := io.ReadAll(file)
		_ = file.Close()
		if err != nil || len(data) == 0 {
			t.Fatalf("payload member %s is incomplete: %v", name, err)
		}
	}
	req.Binding = response.Binding
	desired["displayName"] = "Metadata only"
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil || readBinding(t, response.Binding).Publications.Sequence != first.Publications.Sequence {
		t.Fatalf("metadata change advanced publication: %v", err)
	}
	req.Binding = response.Binding
	req.Artifact.EntryPoint = "bin/repair.cmd"
	delete(desired, "content")
	response, err = c.handle(t.Context(), req, configuration{}, desired)
	if err != nil {
		t.Fatal(err)
	}
	last := readBinding(t, response.Binding)
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if last.AppID != first.AppID || last.PayloadSHA256 == first.PayloadSHA256 || last.Publications.Sequence != 2 || fake.commits != 2 || fake.creates != 1 || fake.app["setupFilePath"] != `bin\repair.cmd` {
		t.Fatal("entrypoint change did not publish distinct content in the same app")
	}
}

func TestSetupTreeRejectsChangedBytesAndUnsafeEntrypoints(t *testing.T) {
	for _, name := range []string{"changed companion", "conflict", "escape", "missing", "Windows device name"} {
		t.Run(name, func(t *testing.T) {
			fake, c := newGraphFixture(t)
			req := fixtureRequest(t)
			req.Artifact = treeArtifact(t)
			desired, _ := validateMetadata(req.Metadata)
			switch name {
			case "changed companion":
				if err := os.WriteFile(filepath.Join(req.Artifact.Path, "payload.cab"), []byte("changed"), 0o644); err != nil {
					t.Fatal(err)
				}
			case "conflict":
				desired["content"] = object{"setup_file": "bin/repair.cmd"}
			case "escape":
				req.Artifact.EntryPoint = "../setup.cmd"
			case "missing":
				req.Artifact.EntryPoint = "missing.exe"
			case "Windows device name":
				if err := os.WriteFile(filepath.Join(req.Artifact.Path, "NUL.txt"), nil, 0o644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := c.handle(t.Context(), req, configuration{}, desired)
			if err == nil {
				t.Fatal("accepted invalid setup content")
			}
			if name == "changed companion" && !strings.Contains(err.Error(), "immutable digest") {
				t.Fatalf("expected immutable content rejection: %v", err)
			}
			fake.mu.Lock()
			defer fake.mu.Unlock()
			if fake.creates != 0 || fake.versions != 0 || fake.commits != 0 {
				t.Fatal("invalid content mutated remote state")
			}
		})
	}
}
