// Package lock records the reviewed inputs and plugins of a project, one
// document per resource, and separates update resolution from preparation.
package lock

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
const Version = 6

// File is a project's whole lock: the reviewed inputs of each resource and the
// content of its executable plugins. On disk each resource has its own
// document, so a change to one never touches another's.
type File struct {
	Version int                                `json:"version"`
	Inputs  map[string]map[string]source.Entry `json:"inputs"`
	Plugins map[string]plugins.Entry           `json:"plugins,omitempty"`
}

// Options controls lock consumption independently of cache use. A run consumes
// the reviewed lock as it is and fails on missing or stale entries; only
// Refresh and PluginsOnly write it.
type Options struct {
	// Refresh resolves inputs from their sources and records what they provide.
	Refresh bool
	Offline bool
	// IgnoreInputs resolves inputs as if the lock had no input entries and
	// saves nothing.
	IgnoreInputs bool
	// PluginsOnly records plugin entries and keeps the input entries.
	PluginsOnly bool
	// PreserveUnselected keeps every reviewed resource the run did not select.
	PreserveUnselected bool
	// Retain keeps these reviewed resources when the run did not select them.
	Retain []string
}

// frozen reports whether the run consumes the reviewed lock as it is.
func (o Options) frozen() bool {
	return !o.Refresh && !o.IgnoreInputs && !o.PluginsOnly
}

// Result reports acquisition separately from downstream metadata changes.
type Result struct {
	File    File
	Changed bool
	Changes []InputChange
	Plugins []PluginChange
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
		return "", fmt.Errorf("lock: resource %q has no lock document path", resource)
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
		return File{}, fmt.Errorf("lock: %w", err)
	}
	if len(files) == 0 {
		return File{}, fmt.Errorf("lock: %w", fs.ErrNotExist)
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
		if name == pluginDocument {
			pins, err := decodePlugins(name, files[name])
			if err != nil {
				return File{}, remedy(err, "run stemma update")
			}
			file.Plugins = pins
			continue
		}
		resource, ok := resourceOf(name)
		if !ok {
			return File{}, fmt.Errorf("lock: %s is not a lock document", name)
		}
		entries, err := decodeInputs(name, files[name])
		if err != nil {
			return File{}, remedy(err, "run stemma update")
		}
		file.Inputs[resource] = entries
	}
	return file, nil
}

// remedy names the command that replaces a document this Stemma cannot use,
// unless the document needs a newer one.
func remedy(err error, command string) error {
	if errors.As(err, new(newerVersionError)) {
		return err
	}
	return fmt.Errorf("%w; %s", err, command)
}

// decodeInputs reads one resource's document, named in its errors.
func decodeInputs(name string, data []byte) (map[string]source.Entry, error) {
	var document struct {
		Version int                      `yaml:"version"`
		Inputs  map[string]source.Entry  `yaml:"inputs,omitempty"`
		Plugins map[string]plugins.Entry `yaml:"plugins,omitempty"`
	}
	if err := decode(data, &document); err != nil {
		return nil, fmt.Errorf("lock: %s: %w", name, err)
	}
	if len(document.Inputs) == 0 || document.Plugins != nil {
		return nil, fmt.Errorf("lock: %s locks named inputs and nothing else", name)
	}
	for input, entry := range document.Inputs {
		if input == "" {
			return nil, fmt.Errorf("lock: %s has an empty input name", name)
		}
		if err := entry.Validate(); err != nil {
			return nil, fmt.Errorf("lock: %s input %s: %w", name, input, err)
		}
	}
	return document.Inputs, nil
}

// decodePlugins reads the plugin document, named in its errors.
func decodePlugins(name string, data []byte) (map[string]plugins.Entry, error) {
	var document struct {
		Version int                      `yaml:"version"`
		Inputs  map[string]source.Entry  `yaml:"inputs,omitempty"`
		Plugins map[string]plugins.Entry `yaml:"plugins,omitempty"`
	}
	if err := decode(data, &document); err != nil {
		return nil, fmt.Errorf("lock: %s: %w", name, err)
	}
	if len(document.Plugins) == 0 || document.Inputs != nil {
		return nil, fmt.Errorf("lock: %s locks plugins and nothing else", name)
	}
	return document.Plugins, nil
}

// LoadPlugins reads the plugin document alone. A project that locks no
// plugins has none.
func LoadPlugins(root string) (map[string]plugins.Entry, error) {
	data, err := readDocument(root, pluginDocument)
	if err != nil || data == nil {
		return nil, err
	}
	pins, err := decodePlugins(pluginDocument, data)
	if err != nil {
		return nil, remedy(err, "run stemma plugins update")
	}
	return pins, nil
}

// readDocument reads one document by its slash path below the root, or
// nothing when it does not exist.
func readDocument(root, name string) ([]byte, error) {
	filename := filepath.Join(root, filepath.FromSlash(name))
	info, err := os.Lstat(filename)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	if !info.Mode().IsRegular() || info.Size() > maxDocument {
		return nil, fmt.Errorf("lock: %s is not a regular file of at most 8 MiB", name)
	}
	data, err := os.ReadFile(filename)
	if err != nil {
		return nil, fmt.Errorf("lock: %w", err)
	}
	return data, nil
}

// documentNames lists the documents of a project's lock by their slash path
// below the root, without reading them.
func documentNames(root string) (map[string]bool, error) {
	existing := map[string]bool{}
	err := documents(root, func(name string, _ fs.DirEntry) error {
		existing[name] = true
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("lock: %w", err)
	}
	return existing, nil
}

// newerVersionError is a document written by a newer Stemma.
type newerVersionError int

func (v newerVersionError) Error() string {
	return fmt.Sprintf("version %d needs a newer stemma", int(v))
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
		return newerVersionError(header.Version)
	case header.Version != Version:
		return fmt.Errorf("version %d is not supported", header.Version)
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

// Update acquires input entries and saves resource or project lock documents.
// Callers hold the project lock for its lifetime.
type Update struct {
	result Result
	old    File
	// reviewed holds the lock's input entries, which a run that ignores
	// them still reports its entries against.
	reviewed map[string]map[string]source.Entry
	// acquired are the resources whose inputs this update obtained.
	acquired map[string]bool
	// existing are the lock's current documents, by name.
	existing map[string]bool
	// unreadable are handled resources whose documents an update replaces.
	unreadable        map[string]bool
	unreadablePlugins bool
	// undeclared are the resources whose documents a full update removes.
	undeclared []string
	root       string
	opts       Options
	inputs     map[string]map[string]plugin.Input
	acquire    func(ctx context.Context, resource string, input plugin.Input, entry source.Entry) (source.Entry, bool, error)
}

// Begin loads reviewed inputs without acquiring resource content.
func Begin(ctx context.Context, root string, inputs map[string]map[string]plugin.Input, pluginEntries map[string]plugins.Entry, m *source.Manager, opts Options) (*Update, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	result := Result{File: File{Version: Version, Inputs: map[string]map[string]source.Entry{}, Plugins: map[string]plugins.Entry{}}}
	opts.Offline = opts.Offline || m.Offline
	manager := *m
	manager.Offline = opts.Offline
	m = &manager
	if opts.Offline && (opts.Refresh || opts.IgnoreInputs) {
		return nil, errors.New("offline runs use input locks and cannot refresh or ignore them")
	}
	frozen := opts.frozen()
	existing, err := documentNames(root)
	if err != nil {
		return nil, err
	}
	if len(existing) == 0 && requiresLock(inputs) && frozen {
		return nil, errors.New("lock: missing; run stemma update")
	}
	// A run reads only the documents of the resources it handles, so another
	// resource's document never fails it. An update replaces a document it
	// cannot read.
	old := File{Version: Version, Inputs: map[string]map[string]source.Entry{}}
	if old.Plugins, err = LoadPlugins(root); err != nil && frozen {
		return nil, err
	}
	unreadablePlugins := err != nil
	unreadable := map[string]bool{}
	for _, resource := range names(inputs) {
		name, err := documentPath(resource)
		if err != nil {
			return nil, err
		}
		if !existing[name] {
			continue
		}
		entries, err := readResource(root, name)
		switch {
		case err != nil && frozen:
			return nil, remedy(err, "run stemma update "+selector(resource))
		case err != nil:
			unreadable[resource] = true
		default:
			old.Inputs[resource] = entries
		}
	}
	// Only a run of the whole catalog answers for the documents of resources
	// it no longer declares.
	var undeclared []string
	if !opts.PreserveUnselected && !opts.PluginsOnly {
		var stale []error
		for _, name := range slices.Sorted(maps.Keys(existing)) {
			resource, ok := resourceOf(name)
			if name == pluginDocument {
				continue
			}
			if !ok {
				return nil, fmt.Errorf("lock: %s is not a lock document", name)
			}
			if _, handled := inputs[resource]; handled || slices.Contains(opts.Retain, resource) {
				continue
			}
			if frozen {
				stale = append(stale, notDeclared(resource))
				continue
			}
			undeclared = append(undeclared, resource)
			if entries, err := readResource(root, name); err == nil {
				old.Inputs[resource] = entries
			}
		}
		if err := errors.Join(stale...); err != nil {
			return nil, err
		}
	}
	reviewed := old.Inputs
	if opts.IgnoreInputs {
		old.Inputs = nil
	}
	resolved := map[string]source.Entry{}
	resolve := func(ctx context.Context, input plugin.Input, previous source.Entry) (source.Entry, bool, error) {
		version, declaration, err := m.Declaration(input)
		if err != nil {
			return source.Entry{}, false, err
		}
		// Inputs sharing a declaration share one discovery.
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
		matches := entry.Validate() == nil && entry.Resolver == input.Resolver && entry.ResolverVersion == version && entry.Declaration == declaration
		if m.IsLocal(input.Resolver) {
			if !matches && frozen {
				return source.Entry{}, false, updateRequired(resource, staleReason(entry, input, version, true))
			}
			cached := matches && m.Store.HasDigest(entry.Content.SHA256)
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
	result.File.Plugins = pluginEntries
	return &Update{result: result, old: old, reviewed: reviewed, acquired: map[string]bool{}, existing: existing, unreadable: unreadable, unreadablePlugins: unreadablePlugins, undeclared: undeclared, root: root, opts: opts, inputs: inputs, acquire: acquire}, nil
}

// readResource reads one resource's document by its name.
func readResource(root, name string) (map[string]source.Entry, error) {
	data, err := readDocument(root, name)
	if err != nil {
		return nil, err
	}
	return decodeInputs(name, data)
}

// Check verifies the lock's shape without reading declared values: it
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
		return errors.New("lock: missing; run stemma update")
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
				stale = append(stale, fmt.Errorf("lock: %s input %s is no longer declared; run stemma update", resource, name))
			}
		}
		for _, name := range names(inputs[resource]) {
			if _, locked := old.Inputs[resource][name]; !locked {
				stale = append(stale, fmt.Errorf("lock: %s input %s is missing; run stemma update", resource, name))
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
	return fmt.Errorf("lock: %s locks a resource that is no longer declared; delete it or run stemma update", document)
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
	case entry.Resolver == "":
		return "input is missing from the lock"
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
		return "inputs are missing from the lock"
	}
	return "declared inputs changed since they were locked"
}

func updateRequired(resource, reason string) error {
	return fmt.Errorf("%s; run stemma update %s", reason, selector(resource))
}

// selector names a resource as the command line does.
func selector(resource string) string {
	parts := strings.Split(resource, "/")
	return strings.Join(parts[max(0, len(parts)-2):], "/")
}

// Acquire obtains one resource's inputs. Repeated calls reuse the same entries.
// Callers scope the logger to the resource.
func (u *Update) Acquire(ctx context.Context, resource string) (map[string]source.Entry, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if u.acquired[resource] {
		return u.result.File.Inputs[resource], nil
	}
	inputs, ok := u.inputs[resource]
	if !ok || resource == "" {
		return nil, fmt.Errorf("unknown input resource %q", resource)
	}
	if u.opts.frozen() && len(u.old.Inputs[resource]) != len(inputs) {
		return nil, updateRequired(resource, staleInputs(u.old.Inputs[resource]))
	}
	entries := map[string]source.Entry{}
	for _, name := range names(inputs) {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if name == "" {
			return nil, errors.New("input lock has an empty input name")
		}
		entry, err := u.ReadInput(ctx, resource, name)
		if err != nil {
			return nil, err
		}
		entries[name] = entry
	}
	if len(entries) > 0 {
		u.result.File.Inputs[resource] = entries
	}
	u.acquired[resource] = true
	return entries, nil
}

// ReadInput leases one declared input without advancing the resource's lock update.
func (u *Update) ReadInput(ctx context.Context, resource, name string) (source.Entry, error) {
	if err := ctx.Err(); err != nil {
		return source.Entry{}, err
	}
	input, ok := u.inputs[resource][name]
	if !ok {
		return source.Entry{}, fmt.Errorf("unknown input %s/%s", resource, name)
	}
	if u.opts.frozen() && len(u.old.Inputs[resource]) != len(u.inputs[resource]) {
		return source.Entry{}, updateRequired(resource, staleInputs(u.old.Inputs[resource]))
	}
	if entry, ok := u.result.File.Inputs[resource][name]; ok {
		return entry, nil
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
		return source.Entry{}, fmt.Errorf("input %s: %w", name, err)
	}
	return entry, nil
}

// Changes compares one acquired resource's inputs with its reviewed entries,
// as Commit records them unless the resource is rejected. A run that ignores
// input locks records nothing and reports what an update would record.
func (u *Update) Changes(resource string) []InputChange {
	if u.opts.PluginsOnly || !u.acquired[resource] {
		return nil
	}
	return DiffInputs(map[string]map[string]source.Entry{resource: u.reviewed[resource]}, map[string]map[string]source.Entry{resource: u.result.File.Inputs[resource]})
}

// CommitResource saves one acquired resource and the plugin pins it uses.
// Other resource documents, including undeclared ones, remain untouched.
func (u *Update) CommitResource(ctx context.Context, resource string) (Result, error) {
	if !u.acquired[resource] {
		return Result{}, fmt.Errorf("%s inputs were not acquired", resource)
	}
	part := *u
	part.inputs = map[string]map[string]plugin.Input{resource: u.inputs[resource]}
	part.undeclared = nil
	return part.Commit(ctx)
}

// Commit writes the documents of the resources the update handled, after
// every one was acquired or rejected, and the plugin document. Rejected
// resources keep their reviewed documents, including when acquisition
// succeeded but downstream preparation failed. Documents of other resources
// are not touched, except that a full update removes those of resources the
// catalog no longer declares.
func (u *Update) Commit(ctx context.Context, rejected ...string) (Result, error) {
	result, old, opts := u.result, u.old, u.opts
	result.File.Inputs = map[string]map[string]source.Entry{}
	if err := ctx.Err(); err != nil {
		return result, err
	}
	owned := map[string]map[string]source.Entry{}
	if !opts.PluginsOnly {
		for resource, inputs := range u.inputs {
			if slices.Contains(rejected, resource) {
				if entries, ok := old.Inputs[resource]; ok {
					result.File.Inputs[resource] = entries
				}
				continue
			}
			entries := u.result.File.Inputs[resource]
			if len(entries) != len(inputs) {
				return result, fmt.Errorf("%s inputs were not acquired", resource)
			}
			owned[resource] = entries
			if len(entries) > 0 {
				result.File.Inputs[resource] = entries
			}
		}
		for _, resource := range u.undeclared {
			owned[resource] = nil
		}
	}
	for resource, entries := range owned {
		if u.unreadable[resource] || !sameJSON(old.Inputs[resource], entries) {
			result.Changed = true
		}
	}
	if u.unreadablePlugins || !sameJSON(old.Plugins, result.File.Plugins) {
		result.Changed = true
	}
	if opts.frozen() && result.Changed {
		return result, errors.New("lock contains stale entries; run stemma update")
	}
	var writeErr error
	if result.Changed && !opts.IgnoreInputs {
		writeErr = u.write(owned, result.File.Plugins)
	}
	pluginsSaved := writeErr == nil
	// Individual documents are atomic; the set is not. On a failed save,
	// read back owned documents to retain only confirmed changes.
	if writeErr != nil {
		result.Changed = false
		for resource, entries := range owned {
			name, _ := documentPath(resource)
			actual, err := readResource(u.root, name)
			if len(entries) == 0 {
				if _, statErr := os.Lstat(filepath.Join(u.root, name)); errors.Is(statErr, fs.ErrNotExist) {
					actual, err = nil, nil
				}
			}
			if err != nil || !sameJSON(actual, entries) {
				delete(owned, resource)
				delete(result.File.Inputs, resource)
				if previous, ok := old.Inputs[resource]; ok {
					result.File.Inputs[resource] = previous
				}
				continue
			}
			result.Changed = result.Changed || u.unreadable[resource] || !sameJSON(old.Inputs[resource], entries)
		}
		pins, err := LoadPlugins(u.root)
		if err != nil || !sameJSON(pins, result.File.Plugins) {
			result.File.Plugins = old.Plugins
		} else {
			pluginsSaved = true
			result.Changed = result.Changed || u.unreadablePlugins || !sameJSON(old.Plugins, pins)
		}
	}
	if !opts.IgnoreInputs {
		before, after := map[string]map[string]source.Entry{}, map[string]map[string]source.Entry{}
		for resource, entries := range owned {
			name, _ := documentPath(resource)
			delete(u.existing, name)
			if previous, ok := old.Inputs[resource]; ok {
				before[resource] = previous
			}
			if len(entries) > 0 {
				u.existing[name] = true
				after[resource] = entries
			}
		}
		result.Changes = DiffInputs(before, after)
		if pluginsSaved {
			delete(u.existing, pluginDocument)
			if len(result.File.Plugins) > 0 {
				u.existing[pluginDocument] = true
			}
		}
	}
	result.Plugins = DiffPlugins(old.Plugins, result.File.Plugins)
	return result, writeErr
}

// sameJSON compares lock values as their documents record them; nothing and
// an empty collection are the same.
func sameJSON[T any](a, b map[string]T) bool {
	if len(a) == 0 || len(b) == 0 {
		return len(a) == len(b)
	}
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}

// write makes the owned documents and the plugin document match the update:
// it removes the documents of owned resources that lock nothing, then writes
// those that differ, each atomically.
func (u *Update) write(owned map[string]map[string]source.Entry, pins map[string]plugins.Entry) error {
	locked := map[string]map[string]source.Entry{}
	var stale []string
	for _, resource := range names(owned) {
		if len(owned[resource]) > 0 {
			locked[resource] = owned[resource]
			continue
		}
		name, err := documentPath(resource)
		if err != nil {
			return err
		}
		if u.existing[name] {
			stale = append(stale, name)
		}
	}
	files, err := Encode(File{Inputs: locked, Plugins: pins})
	if err != nil {
		return err
	}
	if len(pins) == 0 && u.existing[pluginDocument] {
		stale = append(stale, pluginDocument)
	}
	// The documents left afterwards must coexist on case-insensitive
	// filesystems, including those of resources the update did not handle.
	after := map[string][]byte{}
	for name := range u.existing {
		after[name] = nil
	}
	for _, name := range stale {
		delete(after, name)
	}
	maps.Copy(after, files)
	if err := checkPaths(after); err != nil {
		return err
	}
	return replace(u.root, files, stale)
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
				return fmt.Errorf("lock: %s and %s have the same case-insensitive path", previous, prefix)
			}
			if _, file := files[prefix]; file && prefix != name {
				return fmt.Errorf("lock: %s is both a document and a directory", prefix)
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
			stale = append(stale, name)
		}
		return nil
	})
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return replace(root, files, stale)
}

// replace removes the stale documents, then writes those of files that differ
// on disk. Stale paths go first so case-only renames cannot delete a document
// just written or leave its parent directories with the old identity.
func replace(root string, files map[string][]byte, stale []string) error {
	for _, name := range stale {
		filename := filepath.Join(root, filepath.FromSlash(name))
		if err := os.Remove(filename); err != nil {
			return err
		}
		// Removal stops at the first directory that still holds something.
		for dir := filepath.Dir(filename); dir != root; dir = filepath.Dir(dir) {
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
			if _, err := update.Acquire(ctx, resource); err != nil {
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
