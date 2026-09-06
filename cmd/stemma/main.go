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
	"github.com/woodleighschool/stemma/internal/msi"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	"github.com/woodleighschool/stemma/internal/source"
)

var version = "dev"
var commit = "unknown"
var date = "unknown"

func main() {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt)
	err := command(os.Stdout, os.Stderr).ExecuteContext(ctx)
	cancel()
	if err != nil {
		fmt.Fprintln(os.Stderr, "stemma:", err)
		os.Exit(1)
	}
}

func command(out, errOut io.Writer) *cobra.Command {
	var rootDir, configPath, cacheDir, stateDir, output string
	root := &cobra.Command{Use: "stemma", Short: "Resolve, prepare and publish reviewed software artifacts", SilenceErrors: true, SilenceUsage: true, Version: version}
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
	root.AddCommand(&cobra.Command{Use: "schema", Short: "Print the generated JSON schema with editor descriptions", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		data, err := config.Schema()
		if err != nil {
			return err
		}
		_, err = out.Write(data)
		return err
	}})
	var resolved bool
	validate := &cobra.Command{Use: "validate", Short: "Validate configuration without acquiring or publishing", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := resolve()
		if err != nil {
			return err
		}
		p, err := config.Load(path)
		if err != nil {
			return err
		}
		if err := engine.Validate(cmd.Context(), p); err != nil {
			return err
		}
		if resolved || output == "json" {
			return writeJSON(out, p)
		}
		_, err = fmt.Fprintln(out, "Configuration is valid.")
		return err
	}}
	validate.Flags().BoolVar(&resolved, "resolved", false, "Show fully resolved recipe composition")
	root.AddCommand(validate, iconCommand(out))
	for _, method := range []string{"update", "prepare", "plan", "apply"} {
		var frozen, noFrozen, refresh, ignore, offline bool
		cmd := &cobra.Command{Use: method + " [recipe...]", Short: map[string]string{"update": "Resolve current sources and atomically update the lockfile", "prepare": "Acquire and inspect locked inputs without publication", "plan": "Observe destinations and report changes without writing them", "apply": "Re-observe and reconcile destinations once"}[method], RunE: func(cmd *cobra.Command, args []string) error {
			if output != "text" && output != "json" {
				return errors.New("output must be text or json")
			}
			path, err := resolve()
			if err != nil {
				return err
			}
			if method == "update" && len(args) > 0 {
				return errors.New("update resolves the complete project; recipe filtering applies to prepare, plan and apply")
			}
			useFrozen := ci()
			if noFrozen || refresh || ignore || method == "update" {
				useFrozen = false
			}
			if cmd.Flags().Changed("frozen-lockfile") {
				useFrozen = frozen
			}
			report, runErr := engine.Run(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: cacheDir, StateDir: stateDir, Method: method, Recipes: args, Lock: lockfile.Options{Frozen: useFrozen, Refresh: refresh || method == "update", Ignore: ignore, Offline: offline}})
			if output == "json" {
				if err := writeJSON(out, report); err != nil {
					return errors.Join(runErr, err)
				}
			} else {
				if err := printReport(out, method, report); err != nil {
					return errors.Join(runErr, err)
				}
			}
			return runErr
		}}
		if method == "apply" {
			cmd.Aliases = []string{"run"}
		}
		cmd.Flags().BoolVar(&frozen, "frozen-lockfile", false, "Require an unchanged complete lockfile (default in CI)")
		cmd.Flags().BoolVar(&noFrozen, "no-frozen-lockfile", false, "Allow missing or changed source entries to update")
		cmd.Flags().BoolVar(&refresh, "refresh", false, "Resolve fresh sources while running and update the lockfile")
		cmd.Flags().BoolVar(&ignore, "no-lockfile", false, "Neither read nor write the lockfile")
		cmd.Flags().BoolVar(&offline, "offline", false, "Use verified cached locked inputs without source network access")
		cmd.MarkFlagsMutuallyExclusive("frozen-lockfile", "no-frozen-lockfile")
		cmd.MarkFlagsMutuallyExclusive("frozen-lockfile", "no-lockfile", "refresh")
		cmd.MarkFlagsMutuallyExclusive("offline", "no-lockfile", "refresh")
		root.AddCommand(cmd)
	}
	root.AddCommand(&cobra.Command{Use: "inspect FILE", Short: "Read artifact metadata without executing it", Args: cobra.ExactArgs(1), RunE: func(_ *cobra.Command, args []string) error {
		switch strings.ToLower(filepath.Ext(args[0])) {
		case ".intunewin":
			value, err := intunewin.Inspect(args[0])
			if err != nil {
				return err
			}
			return writeJSON(out, value)
		case ".msi":
			value, err := msi.Read(args[0])
			if err != nil {
				return err
			}
			return writeJSON(out, value)
		default:
			value, err := engine.Inspect(args[0])
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
	cache.AddCommand(&cobra.Command{Use: "prune", Short: "Remove cached objects after active runs finish", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		store, err := cas.Open(cacheDir)
		if err != nil {
			return err
		}
		return store.Prune(cmd.Context())
	}})
	root.AddCommand(cache)
	plugins := &cobra.Command{Use: "plugins", Short: "Install and update explicitly trusted executable plugins"}
	plugins.AddCommand(&cobra.Command{Use: "list", Short: "Show configured plugins and locked platform binaries", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
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
		plugins.AddCommand(&cobra.Command{Use: method, Short: "Resolve plugin binaries and write their immutable locks", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
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
			result, err := lockfile.Prepare(cmd.Context(), p, source.New(store, projectRoot, false), lockfile.Options{PluginsOnly: true, Refresh: method == "update"})
			if err != nil {
				return err
			}
			return writeJSON(out, result.File.Plugins)
		}})
	}
	root.AddCommand(plugins)
	return root
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
func ci() bool {
	value := strings.ToLower(os.Getenv("CI"))
	return value != "" && value != "false" && value != "0"
}
func writeJSON(out io.Writer, value any) error {
	encoder := json.NewEncoder(out)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}
func printReport(out io.Writer, method string, r engine.Report) error {
	var text strings.Builder
	if method == "update" {
		_, _ = fmt.Fprintf(&text, "Lockfile changed: %t\n", r.LockChanged)
		_, err := io.WriteString(out, text.String())
		return err
	}
	for _, recipe := range r.Recipes {
		if recipe.Error != "" {
			_, _ = fmt.Fprintf(&text, "%s: failed: %s\n", recipe.Name, recipe.Error)
			continue
		}
		_, _ = fmt.Fprintf(&text, "%s: %s %s (source cached: %t, preparation cached: %t)\n", recipe.Name, recipe.Prepared.Filename, recipe.Prepared.Version, recipe.SourceCached, recipe.Prepared.Cached)
		for name, artifact := range recipe.Artifacts {
			_, _ = fmt.Fprintf(&text, "  %s: %s %s (cached: %t)\n", name, artifact.Filename, artifact.Version, artifact.Cached)
		}
		for name, failure := range recipe.ArtifactErrors {
			_, _ = fmt.Fprintf(&text, "  %s: failed: %s\n", name, failure)
		}
		for _, destination := range recipe.Destinations {
			if destination.Error != "" {
				_, _ = fmt.Fprintf(&text, "  %s: failed: %s\n", destination.Name, destination.Error)
			} else {
				_, _ = fmt.Fprintf(&text, "  %s: %d changes, applied: %t\n", destination.Name, len(destination.Changes), destination.Applied)
			}
		}
	}
	_, err := io.WriteString(out, text.String())
	return err
}
