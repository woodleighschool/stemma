// Package lockfile separates update resolution from preparation of reviewed inputs.
package lockfile

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
	"go.yaml.in/yaml/v4"
)

// Version is the format of every lock document. It changes whenever older
// entries no longer read the same, so a mismatch fails before any entry is used.
const Version = 5

// File is a project's whole lock: the reviewed inputs of each resource and the
// content of its executable plugins. On disk each resource has its own
// document, so a change to one never touches another's.
type File struct {
	Version int                                `json:"version"`
	Inputs  map[string]map[string]source.Entry `json:"inputs"`
	Plugins map[string]plugins.Entry           `json:"plugins,omitempty"`
}

// Options controls lock consumption independently of cache use. A run consumes
// the reviewed lockfile as it is and fails on missing or stale entries; only
// Refresh and PluginsOnly write it.
type Options struct {
	// Refresh resolves inputs from their sources and records what they provide.
	Refresh bool
	Offline bool
	// IgnoreInputs resolves inputs as if the lockfile had no input entries and
	// saves nothing.
	IgnoreInputs bool
	// PluginsOnly records plugin entries and keeps the input entries.
	PluginsOnly bool
	// PreserveUnselected keeps every reviewed resource the run did not select.
	PreserveUnselected bool
	// Retain keeps these reviewed resources when the run did not select them.
	Retain []string
}

// frozen reports whether the run consumes the reviewed lockfile as it is.
func (o Options) frozen() bool {
	return !o.Refresh && !o.IgnoreInputs && !o.PluginsOnly
}

// Result reports acquisition separately from downstream metadata changes.
type Result struct {
	File      File
	Changed   bool
	Changes   []InputChange
	Plugins   []PluginChange
	CacheHits map[string]map[string]bool
}

// Lock serializes project operations and releases automatically after a process crash.
func Lock(ctx context.Context, root string) (unlock func() error, err error) {
	dir := filepath.Join(root, ".stemma")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	l := flock.New(filepath.Join(dir, "project.lock"))
	ok, err := l.TryLock()
	if err == nil && !ok {
		done := plugin.Stage(ctx, "Waiting for project lock")
		ok, err = l.TryLockContext(ctx, 50*time.Millisecond)
		done(err)
	}
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, ctx.Err()
	}
	return l.Close, nil
}

// directory holds a project's lock documents below its root: plugins.yaml
// and one file for each resource, at its key without the default API version.
const directory = "stemma.lock"

const pluginDocument = directory + "/plugins.yaml"

const defaultAPIVersion = "stemma/v1alpha1/"

// Path locates the lock directory of a project root.
func Path(root string) string { return filepath.Join(root, directory) }

// documentPath returns the slash path, below the project root, of the
// document that locks a resource's inputs.
func documentPath(resource string) (string, error) {
	name := directory + "/" + strings.TrimPrefix(resource, defaultAPIVersion) + ".yaml"
	if strings.Count(resource, "/") != 3 || !fs.ValidPath(name) || !filepath.IsLocal(filepath.FromSlash(name)) || strings.ContainsAny(name, "\\:") {
		return "", fmt.Errorf("lockfile: resource %q has no lock document path", resource)
	}
	return name, nil
}

// resourceOf names the resource a lock document belongs to.
func resourceOf(name string) (string, bool) {
	key, below := strings.CutPrefix(name, directory+"/")
	key, yaml := strings.CutSuffix(key, ".yaml")
	switch segments := strings.Count(key, "/") + 1; {
	case !below || !yaml || segments != 2 && segments != 4:
		return "", false
	case segments == 2:
		key = defaultAPIVersion + key
	}
	canonical, err := documentPath(key)
	return key, err == nil && canonical == name
}

// Load reads and validates a project's lock without resolving any providers.
// A project that locks nothing has no lock directory.
func Load(root string) (File, error) {
	files := map[string][]byte{}
	err := documents(root, func(name string, entry fs.DirEntry) error {
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || info.Size() > maxDocument {
			return fmt.Errorf("%s is not a regular file of at most 8 MiB", name)
		}
		files[name], err = os.ReadFile(filepath.Join(root, filepath.FromSlash(name)))
		return err
	})
	if err != nil {
		return File{}, fmt.Errorf("lockfile: %w", err)
	}
	if len(files) == 0 {
		return File{}, fmt.Errorf("lockfile: %w", fs.ErrNotExist)
	}
	return Decode(files)
}

// documents visits the files of a project's lock directory by their slash
// path below the root. Files the host leaves behind, such as .DS_Store, are
// not part of the lock.
func documents(root string, visit func(name string, entry fs.DirEntry) error) error {
	return filepath.WalkDir(Path(root), func(name string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if strings.HasPrefix(entry.Name(), ".") {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		relative, err := filepath.Rel(root, name)
		if err != nil {
			return err
		}
		return visit(filepath.ToSlash(relative), entry)
	})
}

const maxDocument = 8 << 20

// Decode validates lock documents read from somewhere other than the project,
// keyed by their slash path below the project root. No documents are no lock.
func Decode(files map[string][]byte) (File, error) {
	if len(files) == 0 {
		return File{}, nil
	}
	if err := checkPaths(files); err != nil {
		return File{}, err
	}
	file := File{Version: Version, Inputs: map[string]map[string]source.Entry{}}
	for _, name := range names(files) {
		var document struct {
			Version int                      `yaml:"version"`
			Inputs  map[string]source.Entry  `yaml:"inputs,omitempty"`
			Plugins map[string]plugins.Entry `yaml:"plugins,omitempty"`
		}
		if err := decode(files[name], &document); err != nil {
			return File{}, fmt.Errorf("lockfile: %s: %w", name, err)
		}
		if name == pluginDocument {
			if len(document.Plugins) == 0 || document.Inputs != nil {
				return File{}, fmt.Errorf("lockfile: %s locks plugins and nothing else", name)
			}
			file.Plugins = document.Plugins
			continue
		}
		resource, ok := resourceOf(name)
		if !ok {
			return File{}, fmt.Errorf("lockfile: %s is not a lock document", name)
		}
		if len(document.Inputs) == 0 || document.Plugins != nil {
			return File{}, fmt.Errorf("lockfile: %s locks named inputs and nothing else", name)
		}
		for input, entry := range document.Inputs {
			if input == "" {
				return File{}, fmt.Errorf("lockfile: %s has an empty input name", name)
			}
			if err := entry.Validate(); err != nil {
				return File{}, fmt.Errorf("lockfile: %s input %s: %w", name, input, err)
			}
		}
		file.Inputs[resource] = document.Inputs
	}
	return file, nil
}

// decode reads one strict YAML document of the current format.
func decode(data []byte, document any) error {
	if len(data) > maxDocument {
		return errors.New("exceeds 8 MiB")
	}
	var header struct {
		Version int `yaml:"version"`
	}
	if err := yaml.Unmarshal(data, &header); err != nil {
		return err
	}
	switch {
	case header.Version > Version:
		return fmt.Errorf("version %d needs a newer stemma", header.Version)
	case header.Version != Version:
		return fmt.Errorf("version %d is not supported; delete it and run stemma update", header.Version)
	}
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(document); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected one document")
	}
	return nil
}

// Update stages input observations until Commit writes the lock documents.
// Callers hold the project lock for its lifetime.
type Update struct {
	result Result
	old    File
	// reviewed holds the lockfile's input entries, which a run that ignores
	// them still reports its observations against.
	reviewed map[string]map[string]source.Entry
	root     string
	opts     Options
	inputs   map[string]map[string]plugin.Input
	acquire  func(ctx context.Context, resource string, input plugin.Input, entry source.Entry) (source.Entry, bool, error)
}

// Begin loads reviewed inputs without acquiring resource content.
func Begin(ctx context.Context, root string, inputs map[string]map[string]plugin.Input, pluginEntries map[string]plugins.Entry, m *source.Manager, opts Options) (*Update, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := Result{File: File{Version: Version, Inputs: map[string]map[string]source.Entry{}, Plugins: map[string]plugins.Entry{}}, CacheHits: map[string]map[string]bool{}}
	opts.Offline = opts.Offline || m.Offline
	manager := *m
	manager.Offline = opts.Offline
	m = &manager
	if opts.Offline && (opts.Refresh || opts.IgnoreInputs) {
		return nil, errors.New("offline runs use input locks and cannot refresh or ignore them")
	}
	frozen := opts.frozen()
	old, err := Load(root)
	if errors.Is(err, os.ErrNotExist) && requiresLock(inputs) && frozen {
		return nil, errors.New("lockfile: missing; run stemma update")
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	reviewed := old.Inputs
	if opts.IgnoreInputs {
		old.Inputs = nil
	}
	var undeclared []error
	for _, resource := range names(old.Inputs) {
		if _, selected := inputs[resource]; !selected && !opts.PluginsOnly {
			if opts.PreserveUnselected || slices.Contains(opts.Retain, resource) {
				result.File.Inputs[resource] = old.Inputs[resource]
			} else if frozen {
				undeclared = append(undeclared, notDeclared(resource))
			}
		}
	}
	if err := errors.Join(undeclared...); err != nil {
		return nil, err
	}
	resolved := map[string]source.Entry{}
	resolve := func(ctx context.Context, input plugin.Input, previous source.Entry) (source.Entry, bool, error) {
		version, declaration, err := m.Declaration(input)
		if err != nil {
			return source.Entry{}, false, err
		}
		// Inputs sharing a declaration share one observation.
		key := input.Resolver + "\x00" + version + "\x00" + declaration
		if current, ok := resolved[key]; ok {
			return current, m.Store.HasDigest(current.Content.SHA256), nil
		}
		current, cached, err := m.Refresh(ctx, input, previous)
		if err == nil {
			resolved[key] = current
		}
		return current, cached, err
	}
	acquire := func(ctx context.Context, resource string, input plugin.Input, entry source.Entry) (source.Entry, bool, error) {
		version, declaration, err := m.Declaration(input)
		if err != nil {
			return source.Entry{}, false, err
		}
		matches := entry.Version == 1 && entry.Resolver == input.Resolver && entry.ResolverVersion == version && entry.Declaration == declaration
		if m.IsLocal(input.Resolver) {
			if !matches && frozen {
				return source.Entry{}, false, updateRequired(resource, staleReason(entry, input, version, true))
			}
			cached := matches && m.Store.VerifyDigest(ctx, entry.Content.SHA256) == nil
			current, _, err := resolve(ctx, input, entry)
			if err != nil {
				return current, false, err
			}
			unchanged := matches && current.Equal(entry)
			if !unchanged && frozen {
				return source.Entry{}, false, updateRequired(resource, "local input content changed")
			}
			return current, unchanged && cached, nil
		}
		if matches && !opts.Refresh {
			hit, err := m.FetchLocked(ctx, input, entry)
			return entry, hit, err
		}
		if frozen {
			return source.Entry{}, false, updateRequired(resource, staleReason(entry, input, version, false))
		}
		current, cached, err := resolve(ctx, input, entry)
		if err != nil || opts.Refresh {
			return current, cached, err
		}
		// A resolution can name content from the source index without holding
		// its bytes, which this run prepares.
		if _, err := m.FetchLocked(ctx, input, current); err != nil {
			return source.Entry{}, false, err
		}
		return current, cached, nil
	}
	if opts.PluginsOnly {
		result.File.Inputs = old.Inputs
		if result.File.Inputs == nil {
			result.File.Inputs = map[string]map[string]source.Entry{}
		}
	}
	result.File.Plugins = pluginEntries
	return &Update{result: result, old: old, reviewed: reviewed, root: root, opts: opts, inputs: inputs, acquire: acquire}, nil
}

// Check verifies the lockfile's shape without reading declared values: it
// holds entries for exactly the declared inputs, besides whatever retained
// resources keep. Runs that acquire an input compare its entry with the
// declaration.
func Check(root string, inputs map[string]map[string]plugin.Input, retain []string) error {
	old, err := Load(root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return check(old, inputs, retain)
}

func check(old File, inputs map[string]map[string]plugin.Input, retain []string) error {
	if old.Version == 0 && requiresLock(inputs) {
		return errors.New("lockfile: missing; run stemma update")
	}
	var stale []error
	for _, resource := range names(old.Inputs) {
		if _, declared := inputs[resource]; !declared && !slices.Contains(retain, resource) {
			stale = append(stale, notDeclared(resource))
		}
	}
	for _, resource := range names(inputs) {
		for _, name := range names(old.Inputs[resource]) {
			if _, declared := inputs[resource][name]; !declared {
				stale = append(stale, fmt.Errorf("lockfile: %s input %s is no longer declared; run stemma update", resource, name))
			}
		}
		for _, name := range names(inputs[resource]) {
			if _, locked := old.Inputs[resource][name]; !locked {
				stale = append(stale, fmt.Errorf("lockfile: %s input %s is missing; run stemma update", resource, name))
			}
		}
	}
	return errors.Join(stale...)
}

// notDeclared names the document that locks a resource the catalog no longer
// declares, which goes with the declaration.
func notDeclared(resource string) error {
	document, err := documentPath(resource)
	if err != nil {
		return err
	}
	return fmt.Errorf("lockfile: %s locks a resource that is no longer declared; delete it or run stemma update", document)
}

func requiresLock(inputs map[string]map[string]plugin.Input) bool {
	for _, named := range inputs {
		if len(named) != 0 {
			return true
		}
	}
	return false
}

// staleReason says why a reviewed entry does not answer its declaration. A
// local declaration includes its resolved path, which follows the document.
func staleReason(entry source.Entry, input plugin.Input, version string, local bool) string {
	switch {
	case entry.Version == 0:
		return "input is missing from the lockfile"
	case entry.Resolver != input.Resolver || entry.ResolverVersion != version:
		return "input resolver changed since it was locked"
	case local:
		return "local input declaration changed since it was locked, as a new path or a moved document does"
	}
	return "input declaration changed since it was locked"
}

// staleInputs says why a resource's reviewed entries are not its declared inputs.
func staleInputs(locked map[string]source.Entry) string {
	if len(locked) == 0 {
		return "inputs are missing from the lockfile"
	}
	return "declared inputs changed since they were locked"
}

func updateRequired(resource, reason string) error {
	parts := strings.Split(resource, "/")
	selector := strings.Join(parts[max(0, len(parts)-2):], "/")
	return fmt.Errorf("%s; run stemma update %s", reason, selector)
}

// Acquire obtains one resource's inputs. Repeated calls reuse the same observation.
// Callers scope the logger to the resource.
func (u *Update) Acquire(ctx context.Context, resource string) (map[string]source.Entry, map[string]bool, error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	if hits, ok := u.result.CacheHits[resource]; ok {
		return u.result.File.Inputs[resource], hits, nil
	}
	inputs, ok := u.inputs[resource]
	if !ok || resource == "" {
		return nil, nil, fmt.Errorf("unknown input resource %q", resource)
	}
	if u.opts.frozen() && len(u.old.Inputs[resource]) != len(inputs) {
		return nil, nil, updateRequired(resource, staleInputs(u.old.Inputs[resource]))
	}
	entries := map[string]source.Entry{}
	hits := map[string]bool{}
	for _, name := range names(inputs) {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		if name == "" {
			return nil, nil, errors.New("input lock has an empty input name")
		}
		entry, hit, err := u.ReadInput(ctx, resource, name)
		if err != nil {
			return nil, nil, err
		}
		entries[name], hits[name] = entry, hit
	}
	if len(entries) > 0 {
		u.result.File.Inputs[resource] = entries
	}
	u.result.CacheHits[resource] = hits
	return entries, hits, nil
}

// ReadInput leases one declared input without advancing the resource's lock update.
func (u *Update) ReadInput(ctx context.Context, resource, name string) (source.Entry, bool, error) {
	if err := ctx.Err(); err != nil {
		return source.Entry{}, false, err
	}
	input, ok := u.inputs[resource][name]
	if !ok {
		return source.Entry{}, false, fmt.Errorf("unknown input %s/%s", resource, name)
	}
	if u.opts.frozen() && len(u.old.Inputs[resource]) != len(u.inputs[resource]) {
		return source.Entry{}, false, updateRequired(resource, staleInputs(u.old.Inputs[resource]))
	}
	if entry, ok := u.result.File.Inputs[resource][name]; ok {
		return entry, u.result.CacheHits[resource][name], nil
	}
	ctx = plugin.WithLogger(ctx, plugin.Logger(ctx).With("input", name))
	done := plugin.Stage(ctx, "Acquiring input")
	entry, hit, err := u.acquire(ctx, resource, input, u.old.Inputs[resource][name])
	detail := entry.Content.Filename
	if hit {
		detail = strings.TrimSpace(detail + " (cached)")
	}
	done(err, plugin.Detail(detail))
	if err != nil {
		return source.Entry{}, false, fmt.Errorf("input %s: %w", name, err)
	}
	return entry, hit, nil
}

// Changes compares one acquired resource's inputs with its reviewed entries,
// as Commit records them unless the resource is rejected. A run that ignores
// input locks records nothing and reports what an update would record.
func (u *Update) Changes(resource string) []InputChange {
	if _, acquired := u.result.CacheHits[resource]; u.opts.PluginsOnly || !acquired {
		return nil
	}
	return DiffInputs(map[string]map[string]source.Entry{resource: u.reviewed[resource]}, map[string]map[string]source.Entry{resource: u.result.File.Inputs[resource]})
}

// Commit replaces the lockfile after every selected resource was acquired or
// rejected. Rejected resources retain their complete reviewed entries, including
// when acquisition succeeded but downstream preparation failed.
func (u *Update) Commit(ctx context.Context, rejected ...string) (Result, error) {
	result, old, opts := u.result, u.old, u.opts
	result.File.Inputs = maps.Clone(result.File.Inputs)
	result.CacheHits = maps.Clone(result.CacheHits)
	if err := ctx.Err(); err != nil {
		return result, err
	}
	if !opts.PluginsOnly {
		for resource, inputs := range u.inputs {
			if slices.Contains(rejected, resource) {
				delete(result.File.Inputs, resource)
				delete(result.CacheHits, resource)
				if entries, ok := old.Inputs[resource]; ok {
					result.File.Inputs[resource] = entries
				}
				continue
			}
			if len(result.File.Inputs[resource]) != len(inputs) {
				return result, fmt.Errorf("%s inputs were not acquired", resource)
			}
		}
	}

	empty := len(result.File.Inputs) == 0 && len(result.File.Plugins) == 0
	if empty && old.Version == 0 {
		return result, nil
	}
	before, err := json.Marshal(old)
	if err != nil {
		return result, err
	}
	after, err := json.Marshal(result.File)
	if err != nil {
		return result, err
	}
	result.Changed = !bytes.Equal(before, after)
	if opts.frozen() && result.Changed {
		return result, errors.New("lockfile contains stale entries; run stemma update")
	}
	if result.Changed && !opts.IgnoreInputs {
		if err := Save(u.root, result.File); err != nil {
			return result, err
		}
	}
	if !opts.IgnoreInputs {
		result.Changes = DiffInputs(old.Inputs, result.File.Inputs)
	}
	result.Plugins = DiffPlugins(old.Plugins, result.File.Plugins)
	return result, nil
}

// Encode renders the documents Save leaves in a project, keyed by their slash
// path below its root: one for each resource that locks inputs, and one for
// the plugins. A lock that holds nothing has no documents.
func Encode(file File) (map[string][]byte, error) {
	files := map[string][]byte{}
	render := func(name string, document any) error {
		var data bytes.Buffer
		encoder := yaml.NewEncoder(&data)
		encoder.SetIndent(2)
		if err := encoder.Encode(document); err != nil {
			return err
		}
		if err := encoder.Close(); err != nil {
			return err
		}
		files[name] = data.Bytes()
		return nil
	}
	for _, resource := range names(file.Inputs) {
		inputs := file.Inputs[resource]
		if len(inputs) == 0 {
			continue
		}
		name, err := documentPath(resource)
		if err != nil {
			return nil, err
		}
		err = render(name, struct {
			Version int                     `yaml:"version"`
			Inputs  map[string]source.Entry `yaml:"inputs"`
		}{Version, inputs})
		if err != nil {
			return nil, err
		}
	}
	if len(file.Plugins) > 0 {
		err := render(pluginDocument, struct {
			Version int                      `yaml:"version"`
			Plugins map[string]plugins.Entry `yaml:"plugins"`
		}{Version, file.Plugins})
		if err != nil {
			return nil, err
		}
	}
	return files, checkPaths(files)
}

// Lock paths must coexist on case-insensitive filesystems, including when a
// custom API version uses a directory that a built-in resource locks as a file.
func checkPaths(files map[string][]byte) error {
	paths := map[string]string{}
	for _, name := range names(files) {
		for parts := strings.Split(name, "/"); len(parts) > 0; parts = parts[:len(parts)-1] {
			prefix := strings.Join(parts, "/")
			folded := strings.ToLower(prefix)
			if previous, exists := paths[folded]; exists && previous != prefix {
				return fmt.Errorf("lockfile: %s and %s have the same case-insensitive path", previous, prefix)
			}
			if _, file := files[prefix]; file && prefix != name {
				return fmt.Errorf("lockfile: %s is both a document and a directory", prefix)
			}
			paths[folded] = prefix
		}
	}
	return nil
}

// Save makes a project's lock documents those of file: it writes the ones
// that differ, each atomically, and removes the ones file no longer holds.
func Save(root string, file File) error {
	files, err := Encode(file)
	if err != nil {
		return err
	}
	var stale []string
	err = documents(root, func(name string, _ fs.DirEntry) error {
		if _, kept := files[name]; !kept {
			stale = append(stale, filepath.Join(root, filepath.FromSlash(name)))
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	// Remove stale paths first so case-only renames cannot delete a document
	// just written or leave its parent directories with the old identity.
	for _, name := range stale {
		if err := os.Remove(name); err != nil {
			return err
		}
		// Removal stops at the first directory that still holds something.
		for dir := filepath.Dir(name); dir != root; dir = filepath.Dir(dir) {
			if os.Remove(dir) != nil {
				break
			}
		}
	}
	for _, name := range names(files) {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if current, err := os.ReadFile(filename); err == nil && bytes.Equal(current, files[name]) {
			continue
		}
		if err := os.MkdirAll(filepath.Dir(filename), 0o755); err != nil {
			return err
		}
		if err := fileio.Write(filename, files[name], 0o644); err != nil {
			return err
		}
	}
	return nil
}

// Prepare acquires all selected inputs before writing their lock documents.
func Prepare(ctx context.Context, root string, inputs map[string]map[string]plugin.Input, pluginEntries map[string]plugins.Entry, m *source.Manager, opts Options) (Result, error) {
	update, err := Begin(ctx, root, inputs, pluginEntries, m, opts)
	if err != nil {
		return Result{}, err
	}
	if !opts.PluginsOnly {
		for _, resource := range names(inputs) {
			if _, _, err := update.Acquire(ctx, resource); err != nil {
				return Result{}, fmt.Errorf("%s: %w", resource, err)
			}
		}
	}
	return update.Commit(ctx)
}

func names[T any](m map[string]T) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
