package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	"github.com/bmatcuk/doublestar/v4"
	"go.yaml.in/yaml/v4"
)

// Load resolves Software documents from imported family files. Acquisition paths
// remain relative to the owning file; project identity survives directory moves.
func Load(filename string) (Project, error) {
	data, err := os.ReadFile(filename)
	if err != nil {
		return Project{}, err
	}
	var document ProjectDocument
	raw, err := parseConfig(data, &document)
	if err != nil {
		return Project{}, err
	}
	if err := validateHeader(document.APIVersion, document.Kind, "Project", document.Metadata); err != nil {
		return Project{}, err
	}
	p := Project{Project: document.Metadata.Name, Imports: document.Spec.Imports, Components: document.Spec.Components, Destinations: document.Spec.Destinations, Plugins: document.Spec.Plugins, Software: map[string]Software{}}
	spec, _ := raw["spec"].(map[string]any)
	components, _ := spec["components"].(map[string]any)
	root, err := os.OpenRoot(filepath.Dir(filename))
	if err != nil {
		return p, err
	}
	defer func() { _ = root.Close() }()
	seen := map[string]bool{}
	for _, pattern := range p.Imports {
		if !safeRelative(pattern) || !doublestar.ValidatePattern(pattern) {
			return p, fmt.Errorf("invalid import pattern %q", pattern)
		}
		matches, err := doublestar.Glob(root.FS(), pattern, doublestar.WithNoFollow(), doublestar.WithFailOnIOErrors())
		if err != nil {
			return p, fmt.Errorf("import %q: %w", pattern, err)
		}
		if len(matches) == 0 {
			return p, fmt.Errorf("import pattern %q matched no files", pattern)
		}
		for _, name := range matches {
			if seen[name] {
				continue
			}
			seen[name] = true
			if name == filepath.Base(filename) {
				return p, errors.New("project cannot import itself")
			}
			data, err := fs.ReadFile(root.FS(), name)
			if err != nil {
				return p, fmt.Errorf("import %s: %w", name, err)
			}
			documents, err := splitDocuments(data)
			if err != nil {
				return p, fmt.Errorf("import %s: %w", name, err)
			}
			for i, data := range documents {
				var software SoftwareDocument
				raw, err := parseConfig(data, &software)
				if err != nil {
					return p, fmt.Errorf("import %s document %d: %w", name, i+1, err)
				}
				if err := validateHeader(software.APIVersion, software.Kind, "Software", software.Metadata); err != nil {
					return p, fmt.Errorf("import %s document %d: %w", name, i+1, err)
				}
				if err := addSoftware(&p, software.Metadata.Name, raw["spec"], components, path.Dir(name)); err != nil {
					return p, fmt.Errorf("import %s document %d: %w", name, i+1, err)
				}
			}
		}
	}
	return p, p.Validate()
}

func validateHeader(version, kind, expected string, metadata Metadata) error {
	if version != "stemma/v1alpha1" {
		return errors.New("apiVersion must be stemma/v1alpha1")
	}
	if kind != expected {
		return fmt.Errorf("kind must be %s", expected)
	}
	if !namePattern.MatchString(metadata.Name) {
		return errors.New("metadata.name must be a stable name containing letters, digits, dots, underscores or hyphens")
	}
	return nil
}

// FindRoot climbs past Software documents to the nearest Project stemma.yaml.
// Discovery validates document structure without requiring credential expansion.
func FindRoot(startDir string) (string, error) {
	dir, err := filepath.Abs(startDir)
	if err != nil {
		return "", err
	}
	for {
		filename := filepath.Join(dir, "stemma.yaml")
		data, err := os.ReadFile(filename)
		if err == nil {
			documents, err := splitDocuments(data)
			if err != nil {
				return "", fmt.Errorf("%s: %w", filename, err)
			}
			seen := map[string]bool{}
			for i, data := range documents {
				var header struct {
					APIVersion string         `yaml:"apiVersion"`
					Kind       string         `yaml:"kind"`
					Metadata   Metadata       `yaml:"metadata"`
					Spec       map[string]any `yaml:"spec"`
				}
				if _, err := parseDocument(data, &header); err != nil {
					return "", fmt.Errorf("%s document %d: %w", filename, i+1, err)
				}
				var document any
				switch header.Kind {
				case "Project":
					if len(documents) != 1 {
						return "", fmt.Errorf("%s: Project requires one YAML document", filename)
					}
					document = &ProjectDocument{}
				case "Software":
					if seen[header.Metadata.Name] {
						return "", fmt.Errorf("%s: conflicting software ID %q", filename, header.Metadata.Name)
					}
					seen[header.Metadata.Name] = true
					document = &SoftwareDocument{}
				default:
					return "", fmt.Errorf("%s document %d: kind must be Project or Software", filename, i+1)
				}
				if err := validateHeader(header.APIVersion, header.Kind, header.Kind, header.Metadata); err != nil {
					return "", fmt.Errorf("%s document %d: %w", filename, i+1, err)
				}
				if _, err := parseDocument(data, document); err != nil {
					return "", fmt.Errorf("%s document %d: %w", filename, i+1, err)
				}
				if header.Kind == "Project" {
					return dir, nil
				}
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", errors.New("no root stemma.yaml with kind Project found")
		}
		dir = parent
	}
}

func splitDocuments(data []byte) ([][]byte, error) {
	if len(data) > 4<<20 {
		return nil, errors.New("configuration exceeds 4 MiB")
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	var documents [][]byte
	for {
		var node yaml.Node
		if err := dec.Decode(&node); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			return nil, fmt.Errorf("document %d: %w", len(documents)+1, err)
		}
		if len(node.Content) != 1 || node.Content[0].Kind != yaml.MappingNode {
			return nil, fmt.Errorf("document %d must be a resource mapping", len(documents)+1)
		}
		if err := checkNode(&node); err != nil {
			return nil, fmt.Errorf("document %d: %w", len(documents)+1, err)
		}
		encoded, err := yaml.Marshal(&node)
		if err != nil {
			return nil, err
		}
		documents = append(documents, encoded)
	}
	if len(documents) == 0 {
		return nil, errors.New("expected at least one YAML document")
	}
	return documents, nil
}

func safeRelative(name string) bool {
	if name == "" || strings.ContainsAny(name, "\\:\x00\r\n") || !filepath.IsLocal(filepath.FromSlash(name)) {
		return false
	}
	for part := range strings.SplitSeq(name, "/") {
		if part == ".." {
			return false
		}
	}
	return true
}
