package plugin

import (
	"testing"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestAdapterPluginWireNames verifies that the hard-coded wire-name constants
// match the names declared in the compiled proto descriptor. A mismatch causes
// host/plugin negotiation to fail at runtime.
func TestAdapterPluginWireNames(t *testing.T) {
	svc := pb.File_criteria_v1_adapter_plugin_proto.Services().ByName("AdapterPluginService")
	if svc == nil {
		t.Fatal("AdapterPluginService not found in proto descriptor")
	}

	wantService := string(svc.FullName())
	if adapterPluginServiceName != wantService {
		t.Errorf("adapterPluginServiceName = %q; want %q", adapterPluginServiceName, wantService)
	}

	for _, tc := range []struct {
		name    string
		got     string
		method  string
	}{
		{"Info", adapterPluginInfoMethod, "Info"},
		{"OpenSession", adapterPluginOpenSessionMethod, "OpenSession"},
		{"Execute", adapterPluginExecuteMethod, "Execute"},
		{"Permit", adapterPluginPermitMethod, "Permit"},
		{"CloseSession", adapterPluginCloseSessionMethod, "CloseSession"},
	} {
		var found bool
		for i := 0; i < svc.Methods().Len(); i++ {
			if string(svc.Methods().Get(i).Name()) == tc.method {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("method %q not found in proto descriptor", tc.method)
			continue
		}
		want := "/" + wantService + "/" + tc.method
		if tc.got != want {
			t.Errorf("%s method constant = %q; want %q", tc.name, tc.got, want)
		}
	}
}
