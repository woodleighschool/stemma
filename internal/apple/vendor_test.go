package apple

import (
	"os"
	"testing"
)

// TestVendorPackageSignature is opt-in because vendor installers are neither
// downloaded nor redistributed by the ordinary test suite.
func TestVendorPackageSignature(t *testing.T) {
	file := os.Getenv("STEMMA_APPLE_VENDOR_PKG")
	if file == "" {
		t.Skip("set STEMMA_APPLE_VENDOR_PKG to a retained signed vendor PKG")
	}
	pin := os.Getenv("STEMMA_APPLE_VENDOR_CERTIFICATE_SHA256")
	evidence, err := VerifyPackage(file, Policy{RequireIntegrity: true, RequireSignature: true, CertificateSHA256: pin})
	if err != nil {
		t.Fatalf("vendor PKG verification: %+v: %v", evidence, err)
	}
	facts, err := InspectPackage(file)
	if err != nil {
		t.Fatal(err)
	}
	if !facts.HasSignature || len(facts.Entries) == 0 {
		t.Fatalf("missing vendor package facts: %+v", facts)
	}
	t.Logf("verified vendor PKG SHA-256 %s (%d XAR entries, %d component receipts)", evidence.SubjectSHA256, len(facts.Entries), len(facts.Packages))
}
