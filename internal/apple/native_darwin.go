package apple

import (
	"context"
	"os/exec"
	"strings"

	"github.com/woodleighschool/stemma/plugin"
)

// codesignValidity asks the system verifier for whole-bundle validity. The
// platform's own scanners exempt it, where a portable pass over every sealed
// file can cost minutes on a fresh copy. Signer identity remains Stemma's, and
// a bundle it rejects is judged by the portable verifier instead.
func codesignValidity(ctx context.Context, appPath string) bool {
	output, err := exec.CommandContext(ctx, "/usr/bin/codesign", "--verify", "--strict", "--deep", "--", appPath).CombinedOutput() //nolint:gosec // The bundle path is the verification target.
	if err == nil {
		return true
	}
	detail := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(string(output)), appPath+": "))
	if len(detail) > 512 {
		detail = detail[:512]
	}
	plugin.Logger(ctx).DebugContext(ctx, "System verifier did not establish bundle validity", "error", err, "detail", detail)
	return false
}
