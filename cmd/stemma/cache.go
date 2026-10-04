package main

import (
	"context"
	"fmt"
	"math"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/cas"
)

type cacheSettings struct {
	Dir     string `env:"STEMMA_CACHE_DIR"`
	MaxSize string `env:"STEMMA_CACHE_MAX_SIZE" envDefault:"32GiB"`
}

func parseCachePolicy(value string) (cas.Policy, error) {
	size, err := humanize.ParseBytes(value)
	if err != nil || size > math.MaxInt64 {
		return cas.Policy{}, fmt.Errorf("invalid cache-max-size %q: use a size such as 32GiB, or 0 to disable eviction", value)
	}
	return cas.Policy{MaxSize: int64(size)}, nil
}

// Wrap at the command boundary so reconcile's internal engine calls share one
// lease and one maintenance cycle. MCP owns the same boundary per tool call.
func (c *cli) maintainCommands(root *cobra.Command) {
	for _, cmd := range root.Commands() {
		c.maintainCommands(cmd)
		if cmd.RunE == nil {
			continue
		}
		switch cmd.CommandPath() {
		case "stemma update", "stemma prepare", "stemma signature", "stemma icon", "stemma plan", "stemma apply", "stemma artifact", "stemma reconcile", "stemma validate", "stemma schema", "stemma plugins inspect", "stemma plugins update":
		default:
			continue
		}
		run := cmd.RunE
		cmd.RunE = func(cmd *cobra.Command, args []string) error {
			if builtins, _ := cmd.Flags().GetBool("builtins"); builtins {
				return run(cmd, args)
			}
			offline, _ := cmd.Flags().GetBool("offline")
			return cas.Run(cmd.Context(), c.cacheDir, c.cachePolicy, offline, func(ctx context.Context) error { cmd.SetContext(ctx); return run(cmd, args) })
		}
	}
}

func (c *cli) cacheCommand() *cobra.Command {
	cmd := &cobra.Command{Use: "cache", Short: "Inspect and maintain disposable cached content"}
	cmd.AddCommand(&cobra.Command{Use: "path", Short: "Print the cache location", Args: cobra.NoArgs, RunE: func(_ *cobra.Command, _ []string) error {
		store, err := cas.Open(c.cacheDir)
		if err != nil {
			return err
		}
		_, err = fmt.Fprintln(c.out, store.Dir)
		return err
	}})
	info := &cobra.Command{Use: "info", Short: "Show cache usage and effective retention policy", Args: cobra.NoArgs}
	asJSON := jsonFlag(info)
	info.RunE = func(cmd *cobra.Command, _ []string) error {
		store, err := cas.Open(c.cacheDir)
		if err != nil {
			return err
		}
		usage, err := store.Info(cmd.Context())
		if err != nil {
			return err
		}
		if *asJSON {
			return writeJSON(c.out, struct {
				Path    string    `json:"path"`
				MaxSize int64     `json:"max_size_bytes"`
				Grace   string    `json:"recent_use_grace"`
				Usage   cas.Usage `json:"usage"`
			}{store.Dir, c.cachePolicy.MaxSize, cas.Grace.String(), usage})
		}
		budget := humanize.IBytes(uint64(max(0, c.cachePolicy.MaxSize))) + " (soft target)"
		if c.cachePolicy.MaxSize == 0 {
			budget = "disabled"
		}
		_, err = fmt.Fprintf(c.out, "Cache: %s\nBudget: %s\nRecent-use protection: %g hours\nRetained: %s\n  Objects: %s\n  Materialized copies: %s\n  Metadata: %s\nTemporary work: %s\n", store.Dir, budget, cas.Grace.Hours(), humanize.IBytes(uint64(max(0, usage.Retained))), humanize.IBytes(uint64(max(0, usage.Objects))), humanize.IBytes(uint64(max(0, usage.Materialized))), humanize.IBytes(uint64(max(0, usage.Metadata))), humanize.IBytes(uint64(max(0, usage.Work))))
		return err
	}
	var all, dryRun bool
	prune := &cobra.Command{Use: "prune", Short: "Trim the cache after active runs finish; protect content used in the last day", Args: cobra.NoArgs}
	pruneJSON := jsonFlag(prune)
	prune.Flags().BoolVar(&all, "all", false, "Clear all disposable content, including recently used entries")
	prune.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be removed without deleting content")
	prune.RunE = func(cmd *cobra.Command, _ []string) error {
		store, err := cas.Open(c.cacheDir)
		if err != nil {
			return err
		}
		result, err := store.Prune(cmd.Context(), c.cachePolicy, cas.PruneOptions{All: all, DryRun: dryRun})
		if err != nil {
			return err
		}
		if *pruneJSON {
			return writeJSON(c.out, result)
		}
		verb := "Reclaimed"
		if dryRun {
			verb = "Would reclaim"
		}
		noun := "entries"
		if result.Removed == 1 {
			noun = "entry"
		}
		_, err = fmt.Fprintf(c.out, "%s %s (%d %s); %s retained.\n", verb, humanize.IBytes(uint64(max(0, result.Reclaimed))), result.Removed, noun, humanize.IBytes(uint64(max(0, result.After.Retained))))
		if err == nil && c.cachePolicy.MaxSize > 0 && result.After.Retained > c.cachePolicy.MaxSize {
			_, err = fmt.Fprintf(c.out, "Above %s target; recently used content remains protected.\n", humanize.IBytes(uint64(max(0, c.cachePolicy.MaxSize))))
		}
		return err
	}
	cmd.AddCommand(info, prune)
	return cmd
}
