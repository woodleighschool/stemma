package reconcile

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/woodleighschool/stemma/internal/fileio"
)

// marker records the last reviewed commit whose apply completed in full. Losing
// it only repeats an apply, which converges on what destinations already hold.
type marker struct {
	Version int    `json:"version"`
	Applied string `json:"applied"`
}

func markerPath(stateDir string) string { return filepath.Join(stateDir, "reconcile.json") }

func readMarker(stateDir string) (marker, error) {
	data, err := os.ReadFile(markerPath(stateDir))
	if errors.Is(err, os.ErrNotExist) {
		return marker{Version: 1}, nil
	}
	if err != nil {
		return marker{}, err
	}
	var m marker
	if err := json.Unmarshal(data, &m); err != nil || m.Version != 1 {
		return marker{}, errors.New("reconcile marker is corrupt; remove it to apply the reviewed branch again")
	}
	return m, nil
}

func writeMarker(stateDir, applied string) error {
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	data, err := json.Marshal(marker{Version: 1, Applied: applied})
	if err != nil {
		return err
	}
	return fileio.Write(markerPath(stateDir), append(data, '\n'), 0o600)
}
