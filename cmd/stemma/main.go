// Stemma is a finite, reproducible software artifact pipeline.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/intunewin"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	pluginstore "github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/reconcile"
	"github.com/woodleighschool/stemma/internal/source"
	"github.com/woodleighschool/stemma/plugin"
)

var version = "dev"
var commit = "unknown"
var date = "unknown"

func main() {
	ctx, cancel := interruptContext()
	cmd, finish := command(os.Stdout, os.Stderr)
	err := cmd.ExecuteContext(ctx)
	finish(err)
	cancel()
	if errors.Is(err, context.Canceled) {
		os.Exit(130)
	}
	if err != nil {
		os.Exit(1)
	}
}

func interruptContext() (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(context.Background())
	signals := make(chan os.Signal, 1)
	signal.Notify(signals, os.Interrupt)
	go func() {
		select {
		case <-signals:
			// Restore the OS action before publishing cancellation: a second
			// interrupt can exit even if cleanup or a native call is blocked.
			signal.Stop(signals)
			cancel()
		case <-ctx.Done():
			signal.Stop(signals)
		}
	}()
	return ctx, cancel
}

func command(out, errOut io.Writer) (*cobra.Command, func(error)) {
	var rootDir, configPath, cacheDir, stateDir string
	root := &cobra.Command{Use: "stemma", Short: "Resolve, prepare and publish reviewed software artifacts", SilenceErrors: true, SilenceUsage: true, Version: version}
	display := newCommandOutput(root, errOut)
	out = reportWriter{Writer: out, output: display}
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		return display.start(cmd)
	}
	root.SetOut(out)
	root.SetErr(errOut)
	root.PersistentFlags().StringVar(&rootDir, "root", "", "Stemma project directory (discovered from the current directory)")
	root.PersistentFlags().StringVar(&configPath, "config", "", "Path to stemma.yaml")
	root.PersistentFlags().StringVar(&cacheDir, "cache-dir", os.Getenv("STEMMA_CACHE_DIR"), "Disposable content cache directory")
	resolve := func() (string, error) { return findConfig(rootDir, configPath) }
	build := &cobra.Command{Use: "version", Short: "Print build information", Args: cobra.NoArgs}
	buildJSON := jsonFlag(build)
	build.RunE = func(_ *cobra.Command, _ []string) error {
		if *buildJSON {
			return writeJSON(out, map[string]string{"version": version, "commit": commit, "date": date})
		}
		_, err := fmt.Fprintf(out, "stemma %s (commit %s, built %s)\n", version, commit, date)
		return err
	}
	root.AddCommand(build)
	var schemaOutput string
	var schemaOffline, schemaBuiltins bool
	schema := &cobra.Command{Use: "schema --output-file PATH", Short: "Write the catalog JSON Schema for the loaded plugin registry", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if schemaOutput == "" {
			return errors.New("output path is required; use --output-file - for stdout")
		}
		var data []byte
		var err error
		if schemaBuiltins {
			data, err = engine.BuiltinSchema()
		} else {
			path, resolveErr := resolve()
			if resolveErr != nil {
				return resolveErr
			}
			data, err = engine.ProjectSchema(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: cacheDir, Lock: lockfile.Options{Offline: schemaOffline}})
		}
		if err != nil {
			return err
		}
		if schemaOutput == "-" {
			_, err = out.Write(data)
			return err
		}
		return fileio.Write(schemaOutput, data, 0o644)
	}}
	schema.Flags().StringVar(&schemaOutput, "output-file", "", "Required output path; - writes to stdout")
	_ = schema.MarkFlagRequired("output-file")
	schema.Flags().BoolVar(&schemaBuiltins, "builtins", false, "Describe built-in operations without loading a project")
	schema.Flags().BoolVar(&schemaOffline, "offline", false, "Require verified cached plugin bundles")
	schema.MarkFlagsMutuallyExclusive("builtins", "offline")
	root.AddCommand(schema)
	var resolved, validateOffline bool
	validate := &cobra.Command{Use: "validate", Short: "Validate configuration and operation contracts before software acquisition", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := resolve()
		if err != nil {
			return err
		}
		p, err := engine.ValidateProject(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: cacheDir, Lock: lockfile.Options{Offline: validateOffline}})
		if err != nil {
			return err
		}
		if resolved {
			return writeJSON(out, p)
		}
		_, err = fmt.Fprintln(out, "Configuration is valid.")
		return err
	}}
	validate.Flags().BoolVar(&resolved, "resolved", false, "Print the fully resolved software composition as JSON")
	validate.Flags().BoolVar(&validateOffline, "offline", false, "Require verified cached plugin bundles")
	root.AddCommand(validate)
	var operationsOffline bool
	operations := &cobra.Command{Use: "operations", Short: "Print built-in and trusted plugin operation contracts as JSON", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := resolve()
		if err != nil {
			return err
		}
		descriptor, err := engine.Catalog(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: cacheDir, Lock: lockfile.Options{Offline: operationsOffline}})
		if err != nil {
			return err
		}
		return writeJSON(out, descriptor)
	}}
	operations.Flags().BoolVar(&operationsOffline, "offline", false, "Require verified cached plugin bundles")
	root.AddCommand(operations)
	for _, method := range []string{"update", "prepare", "signature", "icon", "plan", "apply"} {
		var offline bool
		var icons engine.IconOptions
		var presentation string
		cmd := &cobra.Command{Use: method + " [Kind/name...]"}
		asJSON := jsonFlag(cmd)
		cmd.Short = map[string]string{"update": "Resolve current sources and atomically update the lockfile", "prepare": "Lock and prepare inputs without publication", "signature": "Derive the verified signer of each published artifact", "icon": "Create declared icon assets from the artwork prepared software carries", "plan": "Observe destinations and report changes without writing them", "apply": "Re-observe and reconcile destinations once"}[method]
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			path, err := resolve()
			if err != nil {
				return err
			}
			if method == "icon" {
				if icons.Presentation, err = icon.ParsePresentation(presentation); err != nil {
					return err
				}
			}
			report, runErr := engine.Run(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: cacheDir, Method: method, Resources: args, Icons: icons, ResourceDone: func(resource engine.ResourceReport) error {
				return display.resourceDone(out, *asJSON, method, resource)
			}, Lock: lockfile.Options{Frozen: method == "plan" || method == "apply" || method == "icon", Refresh: method == "update", Offline: offline}})
			if err := display.report(out, *asJSON, method, report, runErr); err != nil {
				return errors.Join(runErr, err)
			}
			return runErr
		}

		cmd.Flags().BoolVar(&offline, "offline", false, "Use verified cached locked inputs without source network access")
		if method == "icon" {
			cmd.Flags().BoolVar(&icons.Force, "force", false, "Replace icon assets that already exist")
			cmd.Flags().StringVar(&presentation, "presentation", string(icon.Auto), "Icon presentation: auto (glassy on macOS, raw elsewhere), raw or glassy")
			cmd.Flags().IntVar(&icons.Size, "size", icon.Size, "Glassy icon width and height in pixels")
		}
		root.AddCommand(cmd)
	}
	reconciler := &cobra.Command{Use: "reconcile", Short: "Apply the reviewed branch of this checkout and propose lock updates as pull requests", Args: cobra.NoArgs}
	reconcileJSON := jsonFlag(reconciler)
	reconciler.Flags().StringVar(&stateDir, "state-dir", os.Getenv("STEMMA_STATE_DIR"), "Directory recording the last reviewed commit applied in full")
	reconciler.RunE = func(cmd *cobra.Command, _ []string) error {
		path, err := resolve()
		if err != nil {
			return err
		}
		report, runErr := reconcile.Run(cmd.Context(), reconcile.Options{ConfigPath: path, CacheDir: cacheDir, StateDir: stateDir, ResourceDone: func(method string, resource engine.ResourceReport) error {
			return display.resourceDone(out, *reconcileJSON, method, resource)
		}})
		if err := display.reconciled(out, *reconcileJSON, report, runErr); err != nil {
			return errors.Join(runErr, err)
		}
		return runErr
	}
	root.AddCommand(reconciler)
	root.AddCommand(&cobra.Command{Use: "inspect FILE", Short: "Read artifact metadata as JSON without executing it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		switch strings.ToLower(filepath.Ext(args[0])) {
		case ".intunewin":
			done := plugin.Stage(cmd.Context(), "Inspecting artifact")
			value, err := intunewin.Inspect(cmd.Context(), args[0])
			done(err)
			if err != nil {
				return err
			}
			return writeJSON(out, value)
		default:
			value, err := engine.Inspect(cmd.Context(), args[0])
			if err != nil {
				return err
			}
			return writeJSON(out, value)
		}
	}})
	root.AddCommand(packageCommand(out))
	cache := &cobra.Command{Use: "cache", Short: "Manage disposable cached content"}
	cache.AddCommand(&cobra.Command{Use: "path", Short: "Print the cache location", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		store, err := cas.Open(cacheDir)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(out, store.Dir)
		return err
	}})
	cache.AddCommand(&cobra.Command{Use: "prune", Short: "Remove cached objects after active runs finish", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) (runErr error) {
		done := plugin.Stage(cmd.Context(), "Pruning cache")
		defer func() { done(runErr) }()
		store, err := cas.Open(cacheDir)
		if err != nil {
			return err
		}
		return store.Prune(cmd.Context())
	}})
	root.AddCommand(cache)
	plugins := &cobra.Command{Use: "plugins", Short: "Install, update and publish executable plugins"}
	list := &cobra.Command{Use: "list", Short: "Show configured plugins", Args: cobra.NoArgs}
	listJSON := jsonFlag(list)
	list.RunE = func(_ *cobra.Command, _ []string) error {
		path, err := resolve()
		if err != nil {
			return err
		}
		p, err := config.Load(path)
		if err != nil {
			return err
		}
		if *listJSON {
			return writeJSON(out, p.Plugins)
		}
		return printPlugins(out, p.Plugins)
	}
	plugins.AddCommand(list)
	for _, method := range []string{"install", "update"} {
		install := &cobra.Command{Use: method, Short: map[string]string{"install": "Lock configured plugins, keeping locked images", "update": "Lock configured plugins, resolving images again"}[method], Args: cobra.NoArgs}
		installJSON := jsonFlag(install)
		install.RunE = func(cmd *cobra.Command, _ []string) error {
			plugin.Logger(cmd.Context()).DebugContext(cmd.Context(), "Loading plugin declarations")
			path, err := resolve()
			if err != nil {
				return err
			}
			p, err := config.Load(path)
			if err != nil {
				return err
			}
			projectRoot := filepath.Dir(path)
			unlock, err := lockfile.Lock(cmd.Context(), projectRoot)
			if err != nil {
				return err
			}
			defer func() { _ = unlock() }()
			store, err := cas.Open(cacheDir)
			if err != nil {
				return err
			}
			release, err := store.Lease(cmd.Context())
			if err != nil {
				return err
			}
			defer func() { _ = release() }()
			previous, err := lockfile.Load(lockfile.Filename(projectRoot))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
			entries, err := pluginstore.New(store, false).Install(cmd.Context(), projectRoot, p.Plugins, previous.Plugins, method == "update")
			if err != nil {
				return err
			}
			result, err := lockfile.Prepare(cmd.Context(), projectRoot, nil, entries, source.New(store, projectRoot, false), lockfile.Options{PluginsOnly: true})
			if err != nil {
				return err
			}
			if *installJSON {
				return writeJSON(out, result.File.Plugins)
			}
			return printLockedPlugins(out, previous.Plugins, result.File.Plugins, result.Changed)
		}
		plugins.AddCommand(install)
	}
	plugins.AddCommand(publishCommand(out))
	root.AddCommand(plugins)
	return root, display.finish
}

func publishCommand(out io.Writer) *cobra.Command {
	var dist, archiveID string
	var annotations []string
	cmd := &cobra.Command{Use: "publish IMAGE --goreleaser DIST", Short: "Publish GoReleaser plugin bundles as an OCI platform index", Args: cobra.ExactArgs(1)}
	publishJSON := jsonFlag(cmd)
	cmd.Flags().StringVar(&dist, "goreleaser", "", "GoReleaser dist directory whose artifacts.json lists the bundles; run in the directory GoReleaser ran in")
	cmd.Flags().StringVar(&archiveID, "goreleaser-id", "", "GoReleaser archive id of the bundles, when more than one archive id builds tar.zst")
	cmd.Flags().StringArrayVar(&annotations, "annotation", nil, "Index annotation as KEY=VALUE; repeat for more")
	_ = cmd.MarkFlagRequired("goreleaser")
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		values := map[string]string{}
		for _, annotation := range annotations {
			key, value, ok := strings.Cut(annotation, "=")
			if !ok || key == "" {
				return fmt.Errorf("annotation %q must be KEY=VALUE", annotation)
			}
			values[key] = value
		}
		done := plugin.Stage(cmd.Context(), "Publishing plugin")
		bundles, err := pluginstore.GoReleaserBundles(dist, archiveID)
		var entry pluginstore.Entry
		if err == nil {
			entry, err = pluginstore.Publish(cmd.Context(), args[0], bundles, values)
		}
		done(err)
		if err != nil {
			return err
		}
		published := publishedPlugin{Entry: entry}
		for _, bundle := range bundles {
			published.Platforms = append(published.Platforms, bundle.Platform.OS+"/"+bundle.Platform.Architecture)
		}
		slices.Sort(published.Platforms)
		if *publishJSON {
			return writeJSON(out, published)
		}
		_, err = fmt.Fprintf(out, "Published %s (%s) for %s.\n", published.Image, published.Digest, strings.Join(published.Platforms, ", "))
		return err
	}
	return cmd
}

func packageCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "package", Short: "Build destination transport containers"}
	envelope := &cobra.Command{Use: "intunewin SOURCE_DIRECTORY SETUP_FILE OUTPUT", Short: "Build a randomized Intune Windows envelope", Args: cobra.ExactArgs(3)}
	envelopeJSON := jsonFlag(envelope)
	envelope.RunE = func(cmd *cobra.Command, args []string) error {
		result, err := intunewin.Write(cmd.Context(), args[0], args[1], args[2])
		if err != nil {
			return err
		}
		if *envelopeJSON {
			return writeJSON(out, result)
		}
		_, err = fmt.Fprintf(out, "Packaged %s: setup file %s, %s encrypted.\n", args[2], result.SetupFile, humanize.IBytes(uint64(max(0, result.EncryptedContentSize))))
		return err
	}
	cmd.AddCommand(envelope)
	var options pkgbuild.Options
	pkg := &cobra.Command{Use: "pkg SOURCE_DIRECTORY OUTPUT", Short: "Build a portable payload or scripts-only Apple package", Long: "Build a portable Apple package, preserving source modification times.\nSet SOURCE_DATE_EPOCH to normalize timestamps for reproducible standalone builds.", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if value, ok := os.LookupEnv("SOURCE_DATE_EPOCH"); ok {
			seconds, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return errors.New("SOURCE_DATE_EPOCH must be an integer from 0 to 4294967295")
			}
			options.Timestamp = time.Unix(int64(seconds), 0).UTC()
		}
		return pkgbuild.Build(cmd.Context(), args[0], args[1], options)
	}}
	pkg.Flags().StringVar(&options.Identifier, "identifier", "", "Package receipt identifier")
	pkg.Flags().StringVar(&options.Version, "version", "", "Package receipt version")
	pkg.Flags().StringVar(&options.Payload, "payload", "", "Payload directory relative to the source; omit for scripts-only")
	pkg.Flags().StringVar(&options.InstallLocation, "install-location", "/", "Absolute target installation location")
	pkg.Flags().StringVar(&options.Scripts, "scripts", "", "Directory of installer hooks and resources relative to the source")
	cmd.AddCommand(pkg)
	return cmd
}

func findConfig(root, path string) (string, error) {
	if path != "" {
		return filepath.Abs(path)
	}
	if root != "" {
		return filepath.Abs(filepath.Join(root, "stemma.yaml"))
	}
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	project, err := config.FindRoot(dir)
	if err != nil {
		return "", err
	}
	return filepath.Join(project, "stemma.yaml"), nil
}

func jsonFlag(cmd *cobra.Command) *bool {
	return cmd.Flags().Bool("json", false, "Print the report as JSON")
}

func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
