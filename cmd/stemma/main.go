// Stemma is a finite, reproducible software artifact pipeline.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"os"
	"os/signal"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/caarlos0/env/v11"
	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/config"
	"github.com/woodleighschool/stemma/internal/engine"
	"github.com/woodleighschool/stemma/internal/fileio"
	"github.com/woodleighschool/stemma/internal/icon"
	"github.com/woodleighschool/stemma/internal/intunewin"
	"github.com/woodleighschool/stemma/internal/lockfile"
	"github.com/woodleighschool/stemma/internal/mcpserver"
	"github.com/woodleighschool/stemma/internal/pkgbuild"
	pluginstore "github.com/woodleighschool/stemma/internal/plugins"
	"github.com/woodleighschool/stemma/internal/reconcile"
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

// cli is what every command shares: the project and cache flags, and the
// output that renders progress and reports.
type cli struct {
	out                           io.Writer
	display                       *commandOutput
	rootDir, configPath, cacheDir string
	cacheSize                     string
	cachePolicy                   cas.Policy
}

func command(out, errOut io.Writer) (*cobra.Command, func(error)) {
	settings, settingsErr := env.ParseAs[cacheSettings]()
	display := newCommandOutput(out, errOut)
	c := &cli{out: finalWriter{Writer: out, output: display}, display: display}
	root := &cobra.Command{Use: "stemma", Short: "Resolve, prepare and publish reviewed software artifacts", SilenceErrors: true, SilenceUsage: true, Version: version}
	root.PersistentPreRunE = func(cmd *cobra.Command, _ []string) error {
		if settingsErr != nil {
			return settingsErr
		}
		policy, err := parseCachePolicy(c.cacheSize)
		if err != nil {
			return err
		}
		c.cachePolicy = policy
		return display.start(cmd)
	}
	root.SetOut(c.out)
	root.SetErr(errOut)
	root.PersistentFlags().StringVar(&c.rootDir, "root", "", "Stemma project directory (discovered from the current directory)")
	root.PersistentFlags().StringVar(&c.configPath, "config", "", "Path to stemma.yaml")
	root.PersistentFlags().StringVar(&c.cacheDir, "cache-dir", settings.Dir, "Disposable content cache directory")
	root.PersistentFlags().StringVar(&c.cacheSize, "cache-max-size", settings.MaxSize, "Soft retained-cache budget; 0 disables automatic content eviction")
	root.AddCommand(c.versionCommand(), c.schemaCommand(), c.validateCommand(), c.mcpCommand())
	for _, method := range []string{"update", "prepare", "signature", "icon", "plan", "apply"} {
		root.AddCommand(c.runCommand(method))
	}
	root.AddCommand(c.artifactCommand(), c.reconcileCommand(), c.inspectCommand(), packageCommand(c.out), c.cacheCommand(), c.pluginsCommand())
	c.maintainCommands(root)
	return root, display.finish
}

func (c *cli) project() (string, error) { return findConfig(c.rootDir, c.configPath) }

func (c *cli) versionCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "version", Short: "Print build information", Args: cobra.NoArgs}
	asJSON := jsonFlag(cmd)
	cmd.RunE = func(_ *cobra.Command, _ []string) error {
		if *asJSON {
			return writeJSON(c.out, map[string]string{"version": version, "commit": commit, "date": date})
		}
		_, err := fmt.Fprintf(c.out, "stemma %s (commit %s, built %s)\n", version, commit, date)
		return err
	}
	return cmd
}

func (c *cli) schemaCommand() *cobra.Command {
	var output string
	var offline, builtins bool
	cmd := &cobra.Command{Use: "schema --output-file PATH", Short: "Write the catalog JSON Schema for the loaded plugin registry", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		if output == "" {
			return errors.New("output path is required; use --output-file - for stdout")
		}
		var data []byte
		var err error
		if builtins {
			data, err = engine.BuiltinSchema()
		} else {
			path, resolveErr := c.project()
			if resolveErr != nil {
				return resolveErr
			}
			data, err = engine.ProjectSchema(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: c.cacheDir, Lock: lockfile.Options{Offline: offline}})
		}
		if err != nil {
			return err
		}
		if output == "-" {
			_, err = c.out.Write(data)
			return err
		}
		return fileio.Write(output, data, 0o644)
	}}
	cmd.Flags().StringVar(&output, "output-file", "", "Required output path; - writes to stdout")
	_ = cmd.MarkFlagRequired("output-file")
	cmd.Flags().BoolVar(&builtins, "builtins", false, "Describe built-in operations without loading a project")
	cmd.Flags().BoolVar(&offline, "offline", false, "Require verified cached plugin bundles")
	cmd.MarkFlagsMutuallyExclusive("builtins", "offline")
	return cmd
}

func (c *cli) validateCommand() *cobra.Command {
	var resolved, offline bool
	cmd := &cobra.Command{Use: "validate", Short: "Validate the catalog as written, without environment values or software acquisition", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := c.project()
		if err != nil {
			return err
		}
		p, err := engine.ValidateProject(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: c.cacheDir, Lock: lockfile.Options{Offline: offline}}, resolved)
		if err != nil {
			return err
		}
		if resolved {
			return writeJSON(c.out, p)
		}
		_, err = fmt.Fprintln(c.out, "Configuration is valid.")
		return err
	}}
	cmd.Flags().BoolVar(&resolved, "resolved", false, "Evaluate environment values as runs do and print the resolved composition as JSON")
	cmd.Flags().BoolVar(&offline, "offline", false, "Require verified cached plugin bundles")
	return cmd
}

func (c *cli) mcpCommand() *cobra.Command {
	return &cobra.Command{Use: "mcp", Short: "Serve this project's tools to agents over MCP on standard input and output", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		path, err := c.project()
		if err != nil {
			return err
		}
		return mcpserver.Run(cmd.Context(), mcpserver.Options{ConfigPath: path, CacheDir: c.cacheDir, CachePolicy: c.cachePolicy, Version: version})
	}}
}

var runShort = map[string]string{
	"update":    "Resolve current sources and atomically update the lockfile",
	"prepare":   "Prepare resources from the lockfile without publication",
	"signature": "Derive signed or unsigned expectations for each signing subject",
	"icon":      "Create declared icon assets from the artwork prepared software carries",
	"plan":      "Observe destinations and report changes without writing them",
	"apply":     "Re-observe and reconcile destinations once",
}

func (c *cli) runCommand(method string) *cobra.Command {
	var offline bool
	var icons engine.IconOptions
	var input engine.InputSelection
	var presentation, changedSince string
	cmd := &cobra.Command{Use: method + " [Kind/name...]", Short: runShort[method]}
	jsonFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		path, err := c.project()
		if err != nil {
			return err
		}
		if method == "icon" {
			if icons.Presentation, err = icon.ParsePresentation(presentation); err != nil {
				return err
			}
		}
		report, runErr := engine.Run(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: c.cacheDir, Method: method, Resources: args, ChangedSince: changedSince, Icons: icons, Input: input, ResourceDone: func(resource engine.ResourceReport) error {
			return c.display.resourceDone(method, resource)
		}, Lock: lockfile.Options{Offline: offline}})
		if err := c.display.report(c.out, method, report, runErr); err != nil {
			if runErr != nil {
				runErr = fmt.Errorf("%s: %w", method, runErr)
			}
			return errors.Join(runErr, fmt.Errorf("write report: %w", err))
		}
		return runErr
	}
	cmd.Flags().Bool("all", false, "Include unchanged resources in the report")
	cmd.Flags().BoolVar(&offline, "offline", false, "Use verified cached locked inputs without source network access")
	if method == "prepare" {
		cmd.Flags().StringVar(&changedSince, "changed-since", "", "Check the whole lockfile, then prepare only resources whose preparation changed since the Git revision `REV`")
	}
	if method == "icon" {
		cmd.Flags().StringVar(&input.Name, "input", "", "Extract from one resource's locked input without building its output")
		cmd.Flags().StringVar(&input.Path, "path", "", "Application or artwork path within the selected input")
		cmd.Flags().BoolVar(&icons.Force, "force", false, "Replace icon assets that already exist")
		cmd.Flags().StringVar(&presentation, "presentation", string(icon.Auto), "Icon presentation: auto (glassy on macOS, raw elsewhere), raw or glassy")
		cmd.Flags().IntVar(&icons.Size, "size", icon.Size, "Glassy icon width and height in pixels")
	}
	return cmd
}

func (c *cli) artifactCommand() *cobra.Command {
	var output, outputFile string
	var offline, noInputLock bool
	cmd := &cobra.Command{Use: "artifact Kind/name", Short: "Prepare one resource from the lockfile and print the path of its artifact", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		path, err := c.project()
		if err != nil {
			return err
		}
		report, err := engine.Run(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: c.cacheDir, Method: "artifact", Resources: args, Output: output, OutputFile: outputFile, ResourceDone: func(resource engine.ResourceReport) error {
			return c.display.resourceDone("artifact", resource)
		}, Lock: lockfile.Options{Offline: offline, IgnoreInputs: noInputLock}})
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(c.out, report.Artifact)
		return err
	}}
	cmd.Flags().StringVar(&output, "output", "installer", "Resource output to materialize")
	cmd.Flags().StringVar(&outputFile, "output-file", "", "Export to a new file or directory outside the disposable cache")
	cmd.Flags().BoolVar(&offline, "offline", false, "Use verified cached locked inputs without source network access")
	cmd.Flags().BoolVar(&noInputLock, "no-input-lock", false, "Resolve inputs from their sources now instead of their lock entries; plugins stay locked")
	cmd.MarkFlagsMutuallyExclusive("offline", "no-input-lock")
	return cmd
}

func (c *cli) reconcileCommand() *cobra.Command {
	var stateDir string
	cmd := &cobra.Command{Use: "reconcile", Short: "Apply the reviewed branch of this checkout and propose lock updates as pull requests", Args: cobra.NoArgs}
	jsonFlag(cmd)
	cmd.Flags().Bool("all", false, "Include unchanged resources and proposals in the report")
	cmd.Flags().StringVar(&stateDir, "state-dir", os.Getenv("STEMMA_STATE_DIR"), "Directory recording the last reviewed commit applied in full")
	cmd.RunE = func(cmd *cobra.Command, _ []string) error {
		path, err := c.project()
		if err != nil {
			return err
		}
		report, runErr := reconcile.Run(cmd.Context(), reconcile.Options{ConfigPath: path, CacheDir: c.cacheDir, StateDir: stateDir, ResourceDone: c.display.resourceDone, ApplyDone: c.display.applyDone, ProposalDone: c.display.proposalDone})
		if err := c.display.reconciled(c.out, report, runErr); err != nil {
			return errors.Join(runErr, err)
		}
		return runErr
	}
	return cmd
}

func (c *cli) inspectCommand() *cobra.Command {
	var input, selection string
	var offline, noInputLock bool
	cmd := &cobra.Command{Use: "inspect PATH | Kind/name --input NAME", Short: "Describe a local artifact or resource input without executing it", Args: cobra.ExactArgs(1)}
	cmd.Flags().StringVar(&input, "input", "", "Inspect this resource input without building the resource")
	cmd.Flags().StringVar(&selection, "path", "", "Subject path within the artifact or resource input")
	cmd.Flags().BoolVar(&offline, "offline", false, "Use verified cached locked inputs without source network access")
	cmd.Flags().BoolVar(&noInputLock, "no-input-lock", false, "Resolve current inputs without changing their lock entries")
	cmd.MarkFlagsMutuallyExclusive("offline", "no-input-lock")
	asJSON := jsonFlag(cmd)
	cmd.RunE = func(cmd *cobra.Command, args []string) error {
		if input != "" {
			path, err := c.project()
			if err != nil {
				return err
			}
			report, err := engine.Run(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: c.cacheDir, Method: "inspect", Resources: args, Input: engine.InputSelection{Name: input, Path: selection}, Lock: lockfile.Options{Offline: offline, IgnoreInputs: noInputLock}})
			if err != nil {
				return err
			}
			if *asJSON {
				return writeJSON(c.out, report.Inspection)
			}
			_, err = io.WriteString(c.out, renderInspection(c.display.outStyle, *report.Inspection))
			return err
		}
		if offline || noInputLock {
			return errors.New("input options require --input NAME")
		}
		if strings.EqualFold(filepath.Ext(args[0]), ".intunewin") {
			if selection != "" {
				return errors.New("intunewin inspection does not accept --path")
			}
			done := plugin.Stage(cmd.Context(), "Inspecting artifact", plugin.Detail(filepath.Base(args[0])))
			envelope, err := intunewin.Inspect(cmd.Context(), args[0])
			done(err)
			if err != nil {
				return err
			}
			if *asJSON {
				return writeJSON(c.out, envelope)
			}
			_, err = io.WriteString(c.out, renderEnvelope(c.display.outStyle, filepath.Base(args[0]), envelope))
			return err
		}
		inspection, err := engine.Inspect(cmd.Context(), args[0], selection)
		if err != nil {
			return err
		}
		if *asJSON {
			return writeJSON(c.out, inspection)
		}
		_, err = io.WriteString(c.out, renderInspection(c.display.outStyle, inspection))
		return err
	}
	return cmd
}

func (c *cli) pluginsCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "plugins", Short: "Inspect, update and publish executable plugins"}
	var offline bool
	list := &cobra.Command{Use: "list", Short: "Load each plugin from its lock entry and describe what it offers", Args: cobra.NoArgs}
	listJSON := jsonFlag(list)
	list.Flags().BoolVar(&offline, "offline", false, "Require verified cached plugin bundles")
	list.RunE = func(cmd *cobra.Command, _ []string) error {
		path, err := c.project()
		if err != nil {
			return err
		}
		reports, listErr := engine.ListPlugins(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: c.cacheDir, Lock: lockfile.Options{Offline: offline}})
		if listErr != nil && !errors.Is(listErr, engine.ErrPluginsFailed) {
			return listErr
		}
		if *listJSON {
			err = writeJSON(c.out, reports)
		} else {
			err = printPlugins(c.out, reports)
		}
		if err != nil {
			return errors.Join(listErr, fmt.Errorf("write report: %w", err))
		}
		return listErr
	}
	update := &cobra.Command{Use: "update [NAME...]", Short: "Lock plugins to the code their declarations select now", Args: cobra.ArbitraryArgs}
	updateJSON := jsonFlag(update)
	update.RunE = func(cmd *cobra.Command, args []string) error {
		path, err := c.project()
		if err != nil {
			return err
		}
		result, updateErr := engine.UpdatePlugins(cmd.Context(), engine.Options{ConfigPath: path, CacheDir: c.cacheDir}, args)
		if updateErr != nil && !errors.Is(updateErr, engine.ErrPluginsFailed) {
			return updateErr
		}
		if *updateJSON {
			err = writeJSON(c.out, result)
		} else {
			err = printPluginUpdate(c.out, result)
		}
		if err != nil {
			return errors.Join(updateErr, fmt.Errorf("write report: %w", err))
		}
		return updateErr
	}
	cmd.AddCommand(list, update, publishCommand(c.out))
	return cmd
}

func publishCommand(out io.Writer) *cobra.Command {
	var dist, archiveID string
	var annotations []string
	cmd := &cobra.Command{Use: "publish IMAGE --goreleaser DIST", Short: "Publish GoReleaser plugin bundles as an OCI platform index", Args: cobra.ExactArgs(1)}
	publishJSON := jsonFlag(cmd)
	cmd.Flags().StringVar(&dist, "goreleaser", "", "GoReleaser dist directory whose artifacts.json lists the bundles; run in the directory GoReleaser ran in")
	cmd.Flags().StringVar(&archiveID, "goreleaser-id", "", "GoReleaser archive id of the bundles, when more than one archive id builds tar.zst")
	cmd.Flags().StringArrayVar(&annotations, "annotation", nil, "Index annotation as KEY=VALUE, beside the version and revision GoReleaser recorded; repeat for more")
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
		done := plugin.Stage(cmd.Context(), "Publishing plugin", plugin.Detail(args[0]))
		bundles, err := pluginstore.GoReleaserBundles(dist, archiveID)
		var labels map[string]string
		if err == nil {
			labels, err = pluginstore.GoReleaserAnnotations(dist)
		}
		var digest string
		if err == nil {
			maps.Copy(labels, values)
			digest, err = pluginstore.Publish(cmd.Context(), args[0], bundles, labels)
		}
		done(err)
		if err != nil {
			return err
		}
		published := publishedPlugin{Image: args[0], Digest: digest}
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
	pkg.Flags().StringVar((*string)(&options.Compression), "compression", string(pkgbuild.Gzip), "Payload compression: gzip or xz")
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
