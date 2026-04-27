package pluginhost

import (
	"testing"

	pb "github.com/brokenbots/overseer/sdk/pb/overseer/v1"
)

// TestAdapterPluginWireNames verifies that the hard-coded wire-name constants
// match the names declared in the compiled proto descriptor. A mismatch causes
// host/plugin negotiation to fail at runtime.
func TestAdapterPluginWireNames(t *testing.T) {
	svc := pb.File_overseer_v1_adapter_plugin_proto.Services().ByName("AdapterPluginService")
	if svc == nil {
		t.Fatal("AdapterPluginService not found in proto descriptor")
	}

	wantService := string(svc.FullName())
	if adapterPluginServiceName != wantService {
		t.Errorf("adapterPluginServiceName = %q; want %q", adapterPluginServiceName, wantService)
	}

	for _, tc := range []struct {
		name   string
		got    string
		method string
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

// TestHandshakeConfigValues confirms that the magic cookie constants are
// consistent with the HandshakeConfig. An accidental edit to one without
// updating the other would break the host/plugin handshake.
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
// misconfigured-plugin failure mode.
func TestGRPCServerNilImpl(t *testing.T) {
	p := &grpcPlugin{Impl: nil}
	err := p.GRPCServer(nil, nil)
	if err == nil {
		t.Fatal("expected non-nil error from GRPCServer with nil Impl, got nil")
	}
}
