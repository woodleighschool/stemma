package macsoftware

import (
	"testing"

	"github.com/woodleighschool/stemma/internal/signature"
	"github.com/woodleighschool/stemma/plugin"
)

func TestSignatureRecommendation(t *testing.T) {
	source := &plugin.Input{Resolver: "http", Config: map[string]any{"url": "https://example.test/vendor.pkg"}}
	for _, test := range []struct {
		name   string
		spec   Spec
		derive string
		want   bool
	}{
		{name: "vendor", spec: Spec{Source: source}, want: true},
		{name: "local vendor", spec: Spec{Source: &plugin.Input{Resolver: "file"}}, want: true},
		{name: "policy", spec: Spec{}},
		{name: "built", spec: Spec{Source: &plugin.Input{Resource: &plugin.ResourceOutputReference{Kind: "BuildMacPkg", Name: "built"}}}},
		{name: "signer", spec: Spec{Source: source, Signatures: []signature.Expectation{{Signer: "apple:developer-id:ABCDEFGHIJ"}}}},
		{name: "unsigned", spec: Spec{Source: source, Signatures: []signature.Expectation{{Unsigned: true}}}},
		{name: "deriving", spec: Spec{Source: source}, derive: "signature"},
	} {
		t.Run(test.name, func(t *testing.T) {
			notices := test.spec.Notices(plugin.ResourceReference{Kind: "MacSoftware", Name: "foo"}, test.derive)
			if (len(notices) > 0) != test.want {
				t.Fatalf("notices=%+v", notices)
			}
			if test.want && (len(notices) != 1 || notices[0].Hint != "Run `stemma signature MacSoftware/foo` to derive one.") {
				t.Fatalf("notices=%+v", notices)
			}
		})
	}
}
