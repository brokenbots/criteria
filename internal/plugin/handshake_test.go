package plugin

import (
	"context"
	"os/exec"
	"testing"
	"time"

	hplugin "github.com/hashicorp/go-plugin"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func TestHandshakeInfo(t *testing.T) {
	pluginBin := buildNoopPlugin(t)

	client := hplugin.NewClient(&hplugin.ClientConfig{
		HandshakeConfig:  HandshakeConfig,
		Plugins:          PluginMap(),
		Cmd:              exec.Command(pluginBin),
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		// 30 s matches production loader.go and handles CPU-loaded CI hosts
		// where the plugin process advertisement is slow.
		StartTimeout: 30 * time.Second,
	})
	t.Cleanup(client.Kill)

	rpcClient, err := client.Client()
	if err != nil {
		t.Fatalf("create plugin rpc client: %v", err)
	}

	raw, err := rpcClient.Dispense(PluginName)
	if err != nil {
		t.Fatalf("dispense plugin client: %v", err)
	}

	adapterClient, ok := raw.(Client)
	if !ok {
		t.Fatalf("unexpected plugin type: %T", raw)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	resp, err := adapterClient.Info(ctx, &pb.InfoRequest{})
	if err != nil {
		t.Fatalf("info rpc: %v", err)
	}
	if resp.GetName() != "noop" {
		t.Fatalf("unexpected plugin name: %q", resp.GetName())
	}
	if resp.GetVersion() == "" {
		t.Fatal("expected non-empty version")
	}
}

// buildNoopPlugin returns the noop adapter binary compiled once for the test
// binary lifetime. The actual build happens in TestMain (see main_test.go).
func buildNoopPlugin(t *testing.T) string {
	t.Helper()
	if testNoopPluginBin == "" {
		t.Fatal("testNoopPluginBin not set; ensure TestMain ran")
	}
	return testNoopPluginBin
}
