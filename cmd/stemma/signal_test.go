package main

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
)

func TestInterruptCancelsThenExits(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("process interrupts are not supported on Windows")
	}
	if os.Getenv("STEMMA_SIGNAL_TEST") == "1" {
		ctx, stop := interruptContext()
		defer stop()
		fmt.Println("ready")
		<-ctx.Done()
		fmt.Println("canceled")
		time.Sleep(time.Minute)
		return
	}
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.CommandContext(ctx, executable, "-test.run=^TestInterruptCancelsThenExits$")
	child.Env = append(os.Environ(), "STEMMA_SIGNAL_TEST=1")
	stdout, err := child.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = child.Process.Kill() }()
	lines := bufio.NewScanner(stdout)
	for _, want := range []string{"ready", "canceled"} {
		if !lines.Scan() || lines.Text() != want {
			t.Fatalf("child: got %q, want %q", lines.Text(), want)
		}
		if err := child.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
	}
	if err := child.Wait(); err == nil || ctx.Err() != nil {
		t.Fatalf("second interrupt did not exit promptly: %v, %v", err, ctx.Err())
	}
}
