package main

import (
	"io"

	"github.com/spf13/cobra"
	"github.com/woodleighschool/stemma/internal/icon"
)

func iconCommand(out io.Writer) *cobra.Command {
	var output string
	var options icon.Options
	cmd := &cobra.Command{
		Use:   "icon SOURCE --out FILE.png",
		Short: "Retain an application icon or supplied PNG for portable reuse",
		Long:  "Render an app icon with macOS Quick Look or retain a supplied PNG. Existing output is reused unless --refresh is set. Writes PNG provenance to FILE.png.json. Disk images must be extracted first.",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			result, err := icon.Export(cmd.Context(), args[0], output, options)
			if err != nil {
				return err
			}
			return writeJSON(out, result)
		},
	}
	cmd.Flags().StringVar(&output, "out", "", "Durable PNG output path")
	cmd.Flags().IntVar(&options.Size, "size", 256, "Rendered icon width and height in pixels; supplied PNGs retain their dimensions")
	cmd.Flags().BoolVar(&options.Refresh, "refresh", false, "Replace an existing PNG by explicitly deriving it again")
	_ = cmd.MarkFlagRequired("out")
	return cmd
}
