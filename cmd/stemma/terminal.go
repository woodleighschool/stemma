package main

import (
	"context"
	"errors"
	"io"
	"os"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type terminalProgress struct {
	program     *tea.Program
	done        chan struct{}
	err         error
	interactive bool
}

func newTerminalProgress(ctx context.Context, out io.Writer, interactive bool, interrupt func()) *terminalProgress {
	m := &progressModel{ctx: ctx, out: out, interactive: interactive, interrupt: interrupt, width: 100, height: 28}
	m.state.style = newTextStyle(out)
	options := []tea.ProgramOption{tea.WithOutput(out), tea.WithoutSignalHandler(), tea.WithoutBracketedPaste()}
	if !interactive {
		options = append(options, tea.WithInput(nil), tea.WithoutRenderer())
	}
	p := &terminalProgress{program: tea.NewProgram(m, options...), done: make(chan struct{}), interactive: interactive}
	go func() {
		_, err := p.program.Run()
		p.err = errors.Join(err, m.err)
		close(p.done)
		if m.forceExit {
			// A second interrupt exits even if the operation cannot cancel.
			// Run has restored the terminal before we leave.
			os.Exit(130)
		}
	}()
	return p
}

func (p *terminalProgress) update(a activity) { p.program.Send(a) }

func (p *terminalProgress) complete(scope string) { p.program.Send(progressComplete(scope)) }

func (p *terminalProgress) write(out io.Writer, text string) error {
	if text == "" {
		return nil
	}
	if p.interactive && terminalOutput(out) {
		// Send the print message directly so reports retain their arrival order.
		p.program.Send(tea.Println(strings.TrimSuffix(text, "\n"))())
		return nil
	}
	done := make(chan error, 1)
	p.program.Send(progressOutput{out: out, text: text, done: done})
	select {
	case err := <-done:
		return err
	case <-p.done:
		return p.err
	}
}

func (p *terminalProgress) stop() error {
	p.program.Send(progressStop{})
	<-p.done
	return p.err
}

type progressTick time.Time
type progressComplete string
type progressStop struct{}
type progressOutput struct {
	out  io.Writer
	text string
	done chan error
}

type progressModel struct {
	state         progressState
	ctx           context.Context
	out           io.Writer
	interactive   bool
	interrupt     func()
	width, height int
	err           error
	forceExit     bool
}

func (m *progressModel) Init() tea.Cmd {
	interval := time.Second
	if m.interactive {
		interval = 100 * time.Millisecond
	}
	return tea.Tick(interval, func(now time.Time) tea.Msg { return progressTick(now) })
}

func (m *progressModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case activity:
		m.state.update(msg)
	case progressComplete:
		m.state.complete(string(msg))
	case progressOutput:
		_, err := io.WriteString(msg.out, msg.text)
		msg.done <- err
	case progressStop:
		m.state.groups = nil
		return m, tea.Quit
	case progressTick:
		if !m.interactive {
			if _, m.err = io.WriteString(m.out, m.state.plain(time.Time(msg))); m.err != nil {
				return m, tea.Quit
			}
		}
		return m, m.Init()
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
	case tea.KeyMsg:
		switch msg.String() {
		case "ctrl+c":
			if m.ctx.Err() != nil {
				m.forceExit = true
				m.state.groups = nil
				return m, tea.Quit
			}
			if m.interrupt != nil {
				m.interrupt()
			}
		case "ctrl+z":
			return m, tea.Suspend
		}
	}
	return m, nil
}

func (m *progressModel) View() string {
	if !m.interactive {
		return ""
	}
	return m.state.view(time.Now(), m.width, m.height)
}
