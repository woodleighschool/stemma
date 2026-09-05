package main

import (
	"context"
	"errors"
	"os"
	"time"

	"github.com/woodleighschool/stemma/plugin"
)

func main() {
	if plugin.Serve(context.Background(), os.Stdin, os.Stdout, func(ctx context.Context, request plugin.Request) (plugin.Response, error) {
		if request.Method == "wait" {
			time.Sleep(time.Minute)
		}
		if request.Method == "partial" {
			return plugin.Response{Binding: request.Binding}, errors.New("later upload failed")
		}
		return plugin.Response{Observation: request.Metadata}, nil
	}) != nil {
		os.Exit(1)
	}
}
