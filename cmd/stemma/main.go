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
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/intunewin"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	pluginstore "github.com/woodleighschool/stemma/internal/plugins"
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
	var rootDir, configPath, cacheDir, stateDir, output string
	root := &cobra.Command{Use: "stemma", Short: "Resolve, prepare and publish reviewed software artifacts", SilenceErrors: true, SilenceUsage: true, Version: version}
	display := newCommandOutput(root, errOut)
	out = reportWriter{Writer: out, output: display}
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if err := display.start(cmd); err != nil {
			return err
		}
		if output != "text" && output != "json" {
			return errors.New("output must be text or json")
		}
		return nil
	}
	root.SetOut(out)
	root.SetErr(errOut)
	root.PersistentFlags().StringVar(&rootDir, "root", "", "Stemma project directory (discovered from the current directory)")
	root.PersistentFlags().StringVar(&configPath, "config", "", "Path to stemma.yaml")
	root.PersistentFlags().StringVar(&cacheDir, "cache-dir", os.Getenv("STEMMA_CACHE_DIR"), "Disposable content cache directory")
	root.PersistentFlags().StringVar(&stateDir, "state-dir", os.Getenv("STEMMA_STATE_DIR"), "Durable destination binding directory")
	root.PersistentFlags().StringVar(&output, "output", "text", "Report format: text or json")
	resolve := func() (string, error) { return findConfig(rootDir, configPath) }
	root.AddCommand(&cobra.Command{Use: "version", Short: "Print build information", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		return writeJSON(out, map[string]string{"version": version, "commit": commit, "date": date})
	}})
	var projectSchema, schemaOffline bool
	schema := &cobra.Command{Use: "schema", Short: "Print the generated JSON schema with editor descriptions", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		var data []byte
		var err error
		if projectSchema {
			path, resolveErr := resolve()
			if resolveErr != nil {
				return resolveErr
			}
			data, err = engine.ProjectSchema(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: cacheDir, Lock: lockfile.Options{Offline: schemaOffline}})
		} else {
			data, err = config.Schema()
		}
		if err != nil {
			return err
		}
		_, err = out.Write(data)
		return err
	}}
	schema.Flags().BoolVar(&projectSchema, "project", false, "Bind named connections to built-in and trusted plugin contracts")
	schema.Flags().BoolVar(&schemaOffline, "offline", false, "Require verified cached plugin bundles")
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
		if resolved || output == "json" {
			return writeJSON(out, p)
		}
		_, err = fmt.Fprintln(out, "Configuration is valid.")
		return err
	}}
	validate.Flags().BoolVar(&resolved, "resolved", false, "Show fully resolved software composition")
	validate.Flags().BoolVar(&validateOffline, "offline", false, "Require verified cached plugin bundles")
	root.AddCommand(validate)
	var operationsOffline bool
	operations := &cobra.Command{Use: "operations", Short: "Print built-in and trusted plugin operation contracts", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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
	for _, method := range []string{"update", "prepare", "signature", "plan", "apply"} {
		var offline, refreshIcons bool
		cmd := &cobra.Command{Use: method + " [Kind/name...]", Short: map[string]string{"update": "Resolve current sources and atomically update the lockfile", "prepare": "Lock and prepare inputs without publication", "signature": "Derive the verified signer of each published artifact", "plan": "Observe destinations and report changes without writing them", "apply": "Re-observe and reconcile destinations once"}[method], RunE: func(cmd *cobra.Command, args []string) error {
			path, err := resolve()
			if err != nil {
				return err
			}

			report, runErr := engine.Run(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: cacheDir, StateDir: stateDir, Method: method, Resources: args, RefreshIcons: refreshIcons, ResourceDone: func(resource engine.ResourceReport) error { return display.resourceDone(out, output, method, resource) }, Lock: lockfile.Options{Frozen: method == "plan" || method == "apply", Refresh: method == "update", Offline: offline}})
			if err := display.report(out, output, method, report, runErr); err != nil {
				return errors.Join(runErr, err)
			}
			return runErr
		}}

		cmd.Flags().BoolVar(&offline, "offline", false, "Use verified cached locked inputs without source network access")
		if method == "apply" || method == "plan" {
			cmd.Flags().BoolVar(&refreshIcons, "refresh-icons", false, "Refresh application icons without rebuilding installers")
		}
		root.AddCommand(cmd)
	}
	root.AddCommand(&cobra.Command{Use: "inspect FILE", Short: "Read artifact metadata without executing it", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		switch strings.ToLower(filepath.Ext(args[0])) {
		case ".intunewin":
			done := plugin.Stage(cmd.Context(), "Inspecting artifact")
			value, err := intunewin.Inspect(args[0])
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
	cache := &cobra.Command{Use: "cache", Short: "Manage disposable content; destination bindings are separate"}
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
	plugins := &cobra.Command{Use: "plugins", Short: "Install and update explicitly trusted executable plugins"}
	plugins.AddCommand(&cobra.Command{Use: "list", Short: "Show configured plugin images", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		path, err := resolve()
		if err != nil {
			return err
		}
		p, err := config.Load(path)
		if err != nil {
			return err
		}
		return writeJSON(out, p.Plugins)
	}})
	for _, method := range []string{"install", "update"} {
		plugins.AddCommand(&cobra.Command{Use: method, Short: "Resolve plugin images and lock their release indexes", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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
			return writeJSON(out, result.File.Plugins)
		}})
	}
	root.AddCommand(plugins)
	return root, display.finish
}

func packageCommand(out io.Writer) *cobra.Command {
	cmd := &cobra.Command{Use: "package", Short: "Build destination transport containers"}
	cmd.AddCommand(&cobra.Command{Use: "intunewin SOURCE_DIRECTORY SETUP_FILE OUTPUT", Short: "Build a randomized Intune Windows envelope", Args: cobra.ExactArgs(3), RunE: func(cmd *cobra.Command, args []string) error {
		result, err := intunewin.Write(cmd.Context(), args[0], args[1], args[2])
		if err != nil {
			return err
		}
		return writeJSON(out, result)
	}})
	var options pkgbuild.Options
	var preinstall, postinstall string
	pkg := &cobra.Command{Use: "pkg SOURCE_DIRECTORY OUTPUT", Short: "Build a portable payload or scripts-only Apple package", Long: "Build a portable Apple package, preserving source modification times.\nSet SOURCE_DATE_EPOCH to normalize timestamps for reproducible standalone builds.", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if value, ok := os.LookupEnv("SOURCE_DATE_EPOCH"); ok {
			seconds, err := strconv.ParseUint(value, 10, 32)
			if err != nil {
				return errors.New("SOURCE_DATE_EPOCH must be an integer from 0 to 4294967295")
			}
			options.Timestamp = time.Unix(int64(seconds), 0).UTC()
		}
		options.Scripts = map[string]string{}
		if preinstall != "" {
			options.Scripts["preinstall"] = preinstall
		}
		if postinstall != "" {
			options.Scripts["postinstall"] = postinstall
		}
		return pkgbuild.Build(cmd.Context(), args[0], args[1], options)
	}}
	pkg.Flags().StringVar(&options.Identifier, "identifier", "", "Package receipt identifier")
	pkg.Flags().StringVar(&options.Version, "version", "", "Package receipt version")
	pkg.Flags().StringVar(&options.Payload, "payload", "", "Payload directory relative to the source; omit for scripts-only")
	pkg.Flags().StringVar(&options.InstallLocation, "install-location", "/", "Absolute target installation location")
	pkg.Flags().StringVar(&preinstall, "preinstall", "", "Preinstall script relative to the source")
	pkg.Flags().StringVar(&postinstall, "postinstall", "", "Postinstall script relative to the source")
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
func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
