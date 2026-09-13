package pkgbuild

import (
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

func TestTimestampRange(t *testing.T) {
	for _, second := range []int64{-1, 0, math.MaxUint32, math.MaxUint32 + 1} {
		t.Run(strconv.FormatInt(second, 10), func(t *testing.T) {
			root, opts := fixture(t)
			opts.Timestamp = time.Unix(second, 0)
			output := filepath.Join(t.TempDir(), "out.pkg")
			err := Build(t.Context(), root, output, opts)
			if second < 0 || second > math.MaxUint32 {
				if err == nil {
					t.Fatal("unrepresentable timestamp accepted")
				}
				if _, err := os.Stat(output); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("failed build left output")
				}
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}
