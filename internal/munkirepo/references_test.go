package munkirepo_test

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/woodleighschool/stemma/internal/munkirepo"
)

func TestPublicationRelationshipsRenderNativeNames(t *testing.T) {
	root, request := repositoryRequest(t, "App.pkg", `{
		"requires":["peer","Manual Item--2.0",{"resource":{"kind":"MacSoftware","name":"peer"}},{"resource":{"kind":"MacSoftware","name":"peer"},"version":"1.2.3"}],
		"update_for":[{"resource":{"kind":"MacSoftware","name":"default-name"}},"Other Item-1.0"]
	}`)
	request.Peers = map[string]json.RawMessage{
		"stemma/v1alpha1/MacSoftware/peer":         json.RawMessage(`{"pkginfo":{"name":"Native Name"}}`),
		"stemma/v1alpha1/MacSoftware/default-name": json.RawMessage(`{"pkginfo":{}}`),
	}
	filename := apply(t, root, &request)
	var got = readNative[struct {
		Requires  []string `plist:"requires"`
		UpdateFor []string `plist:"update_for"`
	}](t, filename)
	if !reflect.DeepEqual(got.Requires, []string{"peer", "Manual Item--2.0", "Native Name", "Native Name--1.2.3"}) || !reflect.DeepEqual(got.UpdateFor, []string{"default-name", "Other Item-1.0"}) {
		t.Fatalf("relationships changed meaning: %+v", got)
	}
	assertConverged(t, request)
	delete(request.Peers, "stemma/v1alpha1/MacSoftware/peer")
	request.Method = "plan"
	if _, err := munkirepo.Handle(t.Context(), request); err == nil {
		t.Fatal("resource reference fell back to a native name")
	}
}
