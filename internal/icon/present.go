package icon

import (
	"context"
	"fmt"
)

// Subject is what a presentation draws: the artwork extracted from software
// and, where the host can use it, the richer native source it came from.
type Subject struct {
	// Artwork is a PNG inside the asset bounds, or nil when only Path can draw it.
	Artwork []byte
	// Path names a native source such as an application bundle; optional.
	Path string
}

// Presentation is how stemma icon styles a subject into the asset.
type Presentation string

const (
	// Auto draws Glassy where the host can and Raw elsewhere.
	Auto Presentation = "auto"
	// Raw writes the extracted artwork unchanged on any host.
	Raw Presentation = "raw"
	// Glassy draws the subject with the macOS icon renderer.
	Glassy Presentation = "glassy"
)

// ParsePresentation accepts a presentation name from the command line.
func ParsePresentation(value string) (Presentation, error) {
	switch p := Presentation(value); p {
	case Auto, Raw, Glassy:
		return p, nil
	}
	return "", fmt.Errorf("presentation must be %s, %s or %s, not %q", Auto, Raw, Glassy, value)
}

// Resolve replaces Auto with the host's presentation and rejects one the host
// cannot draw, so a run fails before it acquires anything.
func (p Presentation) Resolve() (Presentation, error) {
	switch p {
	case Auto, "":
		return hostPresentation, nil
	case Raw:
		return Raw, nil
	case Glassy:
		if hostPresentation != Glassy {
			return "", ErrUnsupportedHost
		}
		return Glassy, nil
	}
	return "", fmt.Errorf("unknown presentation %q", p)
}

// Options configures one presentation.
type Options struct {
	Presentation Presentation
	// Size is the glassy edge in pixels; zero selects Size. Raw keeps the artwork's own.
	Size int
	// Workspace is a scratch directory the renderer may stage files in.
	Workspace string
}

// Present styles subject and reports the presentation it drew.
func Present(ctx context.Context, subject Subject, options Options) ([]byte, Presentation, error) {
	presentation, err := options.Presentation.Resolve()
	if err != nil {
		return nil, "", err
	}
	if presentation == Raw {
		if subject.Artwork == nil {
			return nil, Raw, ErrNoArtwork
		}
		return subject.Artwork, Raw, nil
	}
	if subject.Path == "" && subject.Artwork == nil {
		return nil, Glassy, ErrNoArtwork
	}
	size := options.Size
	if size == 0 {
		size = Size
	}
	data, err := glassy(ctx, subject, size, options.Workspace)
	return data, Glassy, err
}
