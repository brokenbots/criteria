package adapterhost

import (
	"testing"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestAdapterPluginWireNames verifies that the hard-coded wire-name constants
// match the names declared in the compiled proto descriptor. A mismatch causes
// host/plugin negotiation to fail at runtime.
func TestAdapterWireNames(t *testing.T) {
	svc := pb.File_criteria_v1_adapter_plugin_proto.Services().ByName("AdapterService")
	if svc == nil {
		t.Fatal("AdapterService not found in proto descriptor")
	}

	wantService := string(svc.FullName())
	if adapterServiceName != wantService {
		t.Errorf("adapterServiceName = %q; want %q", adapterServiceName, wantService)
	}

	for _, tc := range []struct {
		name   string
		got    string
		method string
	}{
		{"Info", adapterInfoMethod, "Info"},
		{"OpenSession", adapterOpenSessionMethod, "OpenSession"},
		{"Execute", adapterExecuteMethod, "Execute"},
		{"Permit", adapterPermitMethod, "Permit"},
		{"CloseSession", adapterCloseSessionMethod, "CloseSession"},
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
