package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/dustin/go-humanize"
	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/cas"
	"github.com/woodleighschool/stemma/internal/changes"
	"github.com/woodleighschool/stemma/internal/engine"
)

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
			c.result = nil
			err := cas.Run(cmd.Context(), c.cacheDir, c.cachePolicy, offline, func(ctx context.Context) error { cmd.SetContext(ctx); return run(cmd, args) })
			if c.result != nil {
				if resultErr := c.result(); resultErr != nil {
					if err != nil {
						err = fmt.Errorf("%s: %w", cmd.Name(), err)
					}
					err = errors.Join(err, resultErr)
				}
			}
			return err
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
		var text strings.Builder
		style := newTextStyle(c.out)
		writeFields(&text, style, "", "Cache", [][2]string{{"Path", store.Dir}, {"Budget", budget}, {"Recent-use protection", fmt.Sprintf("%g hours", cas.Grace.Hours())}})
		style.writeHeading(&text, "  ", changes.Line{{Role: changes.Plain, Text: "Retained"}}, changes.Line{{Role: changes.Plain, Text: humanize.IBytes(uint64(max(0, usage.Retained)))}})
		writeFields(&text, style, "    ", "", [][2]string{{"Objects", humanize.IBytes(uint64(max(0, usage.Objects)))}, {"Materialized copies", humanize.IBytes(uint64(max(0, usage.Materialized)))}, {"Metadata", humanize.IBytes(uint64(max(0, usage.Metadata)))}})
		writeFields(&text, style, "  ", "", [][2]string{{"Temporary work", humanize.IBytes(uint64(max(0, usage.Work)))}})
		_, err = io.WriteString(c.out, text.String())
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
		verb := "reclaimed"
		if dryRun {
			verb = "would reclaim"
		}
		noun := "entries"
		if result.Removed == 1 {
			noun = "entry"
		}
		row := changes.Row{
			Summary: changes.Line{{Role: changes.New, Text: fmt.Sprintf("%s %s (%d %s)", verb, humanize.IBytes(uint64(max(0, result.Reclaimed))), result.Removed, noun)}},
			Details: []changes.Line{{{Role: changes.Label, Text: "retained: "}, {Role: changes.Plain, Text: humanize.IBytes(uint64(max(0, result.After.Retained)))}}},
		}
		if c.cachePolicy.MaxSize > 0 && result.After.Retained > c.cachePolicy.MaxSize {
			row.Details = append(row.Details, changes.Line{{Role: changes.Notable, Text: fmt.Sprintf("Above %s target; recently used content remains protected.", humanize.IBytes(uint64(max(0, c.cachePolicy.MaxSize))))}})
		}
		_, err = io.WriteString(c.out, renderRows(newTextStyle(c.out), "Cache", engine.StatusChanged, []changes.Row{row}))
		return err
	}
	verify := &cobra.Command{Use: "verify", Short: "Check every cached object against its digest and discard any that changed", Args: cobra.NoArgs}
	verifyJSON := jsonFlag(verify)
	verify.RunE = func(cmd *cobra.Command, _ []string) error {
		store, err := cas.Open(c.cacheDir)
		if err != nil {
			return err
		}
		result, err := store.VerifyAll(cmd.Context())
		if err != nil {
			return err
		}
		objects := func(n int) string {
			if n == 1 {
				return "1 object"
			}
			return fmt.Sprintf("%d objects", n)
		}
		if *verifyJSON {
			err = writeJSON(c.out, result)
		} else {
			status := engine.StatusUnchanged
			row := changes.Row{Summary: changes.Line{{Role: changes.Plain, Text: fmt.Sprintf("checked %s (%s)", objects(result.Objects), humanize.IBytes(uint64(max(0, result.Bytes))))}}}
			if len(result.Discarded) > 0 {
				status = engine.StatusFailed
				row.Details = append(row.Details, changes.Line{{Role: changes.Label, Text: "discarded: "}, {Role: changes.Failure, Text: objects(len(result.Discarded))}})
			}
			_, err = io.WriteString(c.out, renderRows(newTextStyle(c.out), "Cache", status, []changes.Row{row}))
		}
		if err == nil && len(result.Discarded) > 0 {
			err = fmt.Errorf("discarded %s that no longer matched; the next run acquires or rebuilds them", objects(len(result.Discarded)))
		}
		return err
	}
	cmd.AddCommand(info, prune, verify)
	return cmd
}
