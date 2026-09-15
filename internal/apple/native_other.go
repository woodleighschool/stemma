//go:build !darwin

package apple

import "context"

// codesignValidity leaves whole-bundle validity to Stemma where the platform
// has no verifier of its own.
func codesignValidity(context.Context, string) bool {
	return false
}
