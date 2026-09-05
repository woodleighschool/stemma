package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"github.com/gofrs/flock"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/intune"
	"github.com/woodleighschool/stemma/internal/jamf"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/munkirepo"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

// Options configures one finite execution of the CLI.
type Options struct {
	ConfigPath, CacheDir, StateDir string
	Method                         string
	Recipes                        []string
	Lock                           lockfile.Options
	Handlers                       map[string]plugin.Handler
}

// Report distinguishes source, preparation and each destination's work.
type Report struct {
	LockChanged bool           `json:"lock_changed"`
	Recipes     []RecipeReport `json:"recipes"`
}

// RecipeReport carries immutable facts and individual destination failures.
type RecipeReport struct {
	Name           string              `json:"name"`
	SourceCached   bool                `json:"source_cached"`
	Prepared       *Prepared           `json:"prepared,omitempty"`
	Artifacts      map[string]Prepared `json:"artifacts,omitempty"`
	ArtifactErrors map[string]string   `json:"artifact_errors,omitempty"`
	Destinations   []DestinationReport `json:"destinations,omitempty"`
	Error          string              `json:"error,omitempty"`
}

// DestinationReport describes semantic drift independently of cache hits.
type DestinationReport struct {
	Name            string          `json:"name"`
	SourceChanged   bool            `json:"source_changed"`
	PreparedChanged bool            `json:"prepared_changed"`
	Changes         []plugin.Change `json:"changes"`
	Applied         bool            `json:"applied"`
	Error           string          `json:"error,omitempty"`
}

type binding struct {
	Connection string          `json:"connection"`
	Source     string          `json:"source,omitempty"`
	Payload    string          `json:"payload,omitempty"`
	Binding    json.RawMessage `json:"binding"`
}
type state struct {
	Version  int                `json:"version"`
	Project  string             `json:"project"`
	Bindings map[string]binding `json:"bindings"`
}

// Run prepares locked inputs once and continues independent destinations after failures.
func Run(ctx context.Context, opts Options) (Report, error) {
	var report Report
	p, err := config.Load(opts.ConfigPath)
	if err != nil {
		return report, err
	}
	if err := Validate(ctx, p); err != nil {
		return report, err
	}
	root, err := filepath.Abs(filepath.Dir(opts.ConfigPath))
	if err != nil {
		return report, err
	}
	unlock, err := lockfile.Lock(ctx, root)
	if err != nil {
		return report, err
	}
	defer func() { _ = unlock() }()
	store, err := cas.Open(opts.CacheDir)
	if err != nil {
		return report, err
	}
	release, err := store.Lease(ctx)
	if err != nil {
		return report, err
	}
	defer func() { _ = release() }()
	m := source.New(store, root, opts.Lock.Offline)
	locked, err := lockfile.Prepare(ctx, p, m, opts.Lock)
	if err != nil {
		return report, err
	}
	report.LockChanged = locked.Changed
	if opts.Method == "update" {
		return report, nil
	}
	stateDir := opts.StateDir
	if stateDir == "" {
		stateDir = filepath.Join(root, ".stemma", "state")
	}
	statePath := filepath.Join(stateDir, p.Project+".json")
	if opts.Method == "apply" {
		if err := os.MkdirAll(stateDir, 0o700); err != nil {
			return report, err
		}
		lock := flock.New(filepath.Join(stateDir, p.Project+".lock"))
		ok, err := lock.TryLockContext(ctx, 50*time.Millisecond)
		if err != nil {
			return report, err
		}
		if !ok {
			return report, ctx.Err()
		}
		defer func() { _ = lock.Close() }()
	}
	current, err := loadState(statePath, p.Project)
	if err != nil {
		return report, err
	}
	selected := opts.Recipes
	if len(selected) == 0 {
		for name := range p.Recipes {
			selected = append(selected, name)
		}
		sort.Strings(selected)
	}
	for _, name := range selected {
		if _, exists := p.Recipes[name]; !exists {
			return report, fmt.Errorf("unknown recipe %q", name)
		}
	}
	var failures []error
	for _, name := range selected {
		item := RecipeReport{Name: name, SourceCached: locked.CacheHits[name]}
		work, err := os.MkdirTemp(filepath.Join(store.Dir, "work"), "run-*")
		if err != nil {
			return report, err
		}
		recipe := p.Recipes[name]
		preparation := recipe
		usesSource := len(recipe.Artifacts) == 0
		for _, metadata := range recipe.Destinations {
			if _, named := metadata["artifact"]; !named {
				usesSource = true
			}
		}
		if !usesSource && recipe.Verification.Subject != "source" {
			preparation.Verification = config.Verification{}
		}
		prepared, prepareErr := prepare(ctx, store, locked.File.Recipes[name], preparation, work)
		item.Prepared = &prepared
		if prepareErr == nil && len(recipe.Artifacts) > 0 {
			item.Artifacts = make(map[string]Prepared, len(recipe.Artifacts))
			artifactNames := make([]string, 0, len(recipe.Artifacts))
			for artifactName := range recipe.Artifacts {
				used := opts.Method == "prepare"
				for _, metadata := range recipe.Destinations {
					if metadata["artifact"] == artifactName {
						used = true
					}
				}
				if used {
					artifactNames = append(artifactNames, artifactName)
				}
			}
			sort.Strings(artifactNames)
			for _, artifactName := range artifactNames {
				artifact, err := derivePackage(ctx, store, prepared, recipe.Artifacts[artifactName], recipe.Verification, filepath.Join(work, "artifacts", artifactName))
				if err != nil {
					if item.ArtifactErrors == nil {
						item.ArtifactErrors = map[string]string{}
					}
					item.ArtifactErrors[artifactName] = err.Error()
					failures = append(failures, fmt.Errorf("%s/%s: %w", name, artifactName, err))
					continue
				}
				item.Artifacts[artifactName] = artifact
			}
		}
		if prepareErr != nil {
			item.Error = prepareErr.Error()
			failures = append(failures, fmt.Errorf("%s: %w", name, prepareErr))
		} else if opts.Method != "prepare" {
			destinations := make([]string, 0, len(p.Recipes[name].Destinations))
			for destination := range p.Recipes[name].Destinations {
				destinations = append(destinations, destination)
			}
			sort.Strings(destinations)
			for _, destination := range destinations {
				output := prepared
				if artifactName, ok := recipe.Destinations[destination]["artifact"].(string); ok {
					if failure, failed := item.ArtifactErrors[artifactName]; failed {
						item.Destinations = append(item.Destinations, DestinationReport{Name: destination, Error: failure})
						continue
					}
					output = item.Artifacts[artifactName]
				}
				entry, callErr := deliver(ctx, opts, p, locked.File, store, root, work, name, destination, output, &current, statePath)
				if callErr != nil {
					entry.Error = callErr.Error()
					failures = append(failures, fmt.Errorf("%s/%s: %w", name, destination, callErr))
				}
				item.Destinations = append(item.Destinations, entry)
			}
		}
		if err := os.RemoveAll(work); err != nil {
			failures = append(failures, err)
		}
		report.Recipes = append(report.Recipes, item)
	}
	return report, errors.Join(failures...)
}

func deliver(ctx context.Context, opts Options, p config.Project, locked lockfile.File, store *cas.Store, root, work, recipe, name string, prepared Prepared, current *state, statePath string) (DestinationReport, error) {
	destination := p.Destinations[name]
	identity := plugin.Identity{Project: p.Project, Recipe: recipe, Destination: name}
	key := recipe + "/" + name
	connection := config.Fingerprint(destination)
	previous := current.Bindings[key]
	if previous.Connection != connection {
		previous = binding{Connection: connection}
	}
	report := DestinationReport{Name: name, SourceChanged: previous.Source != prepared.Source.Artifact.SHA256, PreparedChanged: previous.Payload != prepared.Payload.SHA256}
	if prepared.Tree {
		return report, fmt.Errorf("destination %s requires a file representation; %s conversion is unsupported", name, prepared.Format)
	}
	metadata, err := json.Marshal(destinationMetadata(p.Recipes[recipe].Destinations[name]))
	if err != nil {
		return report, err
	}
	settings := destination.Config
	if destination.Type == "munki" {
		path := destination.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		settings = map[string]any{"path": path}
	}
	configData, err := json.Marshal(settings)
	if err != nil {
		return report, err
	}
	req := plugin.Request{Method: opts.Method, Identity: identity, Config: configData, Metadata: metadata, Artifact: plugin.Artifact{Path: prepared.Path, SHA256: prepared.Payload.SHA256, Size: prepared.Payload.Size, Filename: prepared.Filename, Version: prepared.Version}, Binding: previous.Binding}
	var response plugin.Response
	if destination.Type == "plugin" {
		platform := runtime.GOOS + "/" + runtime.GOARCH
		entry, exists := locked.Plugins[destination.Plugin][platform]
		if !exists {
			return report, fmt.Errorf("plugin %s does not support %s", destination.Plugin, platform)
		}
		executable := filepath.Join(work, "plugins", name, destination.Plugin+"-"+entry.Filename)
		if err := store.Materialize(ctx, entry.Artifact, executable); err != nil {
			return report, err
		}
		if err := os.Chmod(executable, 0o700); err != nil {
			return report, err
		}
		response, err = plugin.Run(ctx, executable, req)
	} else {
		handler := opts.Handlers[destination.Type]
		if handler == nil {
			handler = nativeHandler(destination.Type)
		}
		if handler == nil {
			return report, fmt.Errorf("destination type %s is not implemented", destination.Type)
		}
		response, err = handler(ctx, req)
	}
	report.Changes = response.Changes
	if opts.Method == "apply" && len(response.Binding) > 0 {
		previous.Binding = response.Binding
		if err == nil {
			previous.Source = prepared.Source.Artifact.SHA256
			previous.Payload = prepared.Payload.SHA256
			report.Applied = true
		}
		current.Bindings[key] = previous
		if saveErr := saveState(statePath, *current); saveErr != nil {
			return report, errors.Join(err, saveErr)
		}
	}
	return report, err
}

// Validate checks native connection and metadata contracts without credentials,
// acquisition or destination requests. Plugins validate their own protocol input.
func Validate(ctx context.Context, p config.Project) error {
	for name, recipe := range p.Recipes {
		for destination, metadata := range recipe.Destinations {
			d := p.Destinations[destination]
			handler := nativeHandler(d.Type)
			if handler == nil {
				continue
			}
			settings := d.Config
			if d.Type == "munki" {
				settings = map[string]any{"path": d.Path}
			}
			configData, err := json.Marshal(settings)
			if err != nil {
				return err
			}
			metadataData, err := json.Marshal(destinationMetadata(metadata))
			if err != nil {
				return err
			}
			_, err = handler(ctx, plugin.Request{Method: "validate", Config: configData, Metadata: metadataData})
			if err != nil {
				return fmt.Errorf("%s/%s: %w", name, destination, err)
			}
		}
	}
	return nil
}

func nativeHandler(kind string) plugin.Handler {
	switch kind {
	case "munki":
		return munkirepo.Handle
	case "intune":
		return intune.Handle
	case "jamf":
		return jamf.Handle
	default:
		return nil
	}
}

func loadState(path, project string) (state, error) {
	s := state{Version: 1, Project: project, Bindings: map[string]binding{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return s, err
	}
	if len(data) > 16<<20 {
		return s, errors.New("destination state exceeds 16 MiB")
	}
	if err := json.Unmarshal(data, &s); err != nil {
		return s, fmt.Errorf("destination state is corrupt; restore it before publication: %w", err)
	}
	if s.Version != 1 || s.Project != project || s.Bindings == nil {
		return s, errors.New("destination state has an unsupported version or different project identity")
	}
	return s, nil
}
func saveState(path string, s state) error {
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	return fileio.Write(path, append(data, '\n'), 0o600)
}
