package adapterhost

import (
	"testing"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestAdapterPluginWireNames verifies that the hard-coded wire-name constants
// match the names declared in the compiled proto descriptor. A mismatch causes
// host/adapter negotiation to fail at runtime.
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

// TestHandshakeConfigValues confirms that the magic cookie constants are
// consistent with the HandshakeConfig. An accidental edit to one without
// updating the other would break the host/adapter handshake.
func TestHandshakeConfigValues(t *testing.T) {
	if HandshakeConfig.MagicCookieKey != MagicCookieKey {
		t.Errorf("HandshakeConfig.MagicCookieKey = %q; want %q", HandshakeConfig.MagicCookieKey, MagicCookieKey)
	}
	if HandshakeConfig.MagicCookieValue != MagicCookieValue {
		t.Errorf("HandshakeConfig.MagicCookieValue = %q; want %q", HandshakeConfig.MagicCookieValue, MagicCookieValue)
	}
	if HandshakeConfig.ProtocolVersion != 1 {
		t.Errorf("HandshakeConfig.ProtocolVersion = %d; want 1", HandshakeConfig.ProtocolVersion)
	}
}

// TestGRPCServerNilImpl confirms that calling GRPCServer with a nil Impl
// returns an error rather than panicking. This guard prevents a subtle
// misconfigured-adapter failure mode.
func TestGRPCServerNilImpl(t *testing.T) {
	p := &grpcAdapter{Impl: nil}
	err := p.GRPCServer(nil, nil)
	if err == nil {
		t.Fatal("expected non-nil error from GRPCServer with nil Impl, got nil")
	}
}
