package adapterhost

import (
"testing"

v2 "github.com/brokenbots/criteria/proto/criteria/v2"
)

// TestAdapterWireNames verifies that the v2 AdapterService descriptor has the
// expected methods. A mismatch causes host/adapter negotiation to fail at runtime.
func TestAdapterWireNames(t *testing.T) {
svc := v2.File_criteria_v2_adapter_proto.Services().ByName("AdapterService")
if svc == nil {
t.Fatal("AdapterService not found in v2 proto descriptor")
}

const wantService = "criteria.v2.AdapterService"
if string(svc.FullName()) != wantService {
t.Errorf("service full name = %q; want %q", string(svc.FullName()), wantService)
}

for _, method := range []string{
"Info", "OpenSession", "Execute", "Log", "Permissions",
"Pause", "Resume", "Snapshot", "Restore", "Inspect", "CloseSession",
} {
found := false
for i := 0; i < svc.Methods().Len(); i++ {
if string(svc.Methods().Get(i).Name()) == method {
found = true
break
}
}
if !found {
t.Errorf("method %q not found in v2 proto descriptor", method)
}
}
}
