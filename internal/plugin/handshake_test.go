package plugin

import (
	"context"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	hplugin "github.com/hashicorp/go-plugin"
)

func TestHandshakeInfo(t *testing.T) {
	pluginBin := buildNoopPlugin(t)

	client := hplugin.NewClient(&hplugin.ClientConfig{
		HandshakeConfig:  HandshakeConfig,
		Plugins:          PluginMap(),
		Cmd:              exec.Command(pluginBin),
		AllowedProtocols: []hplugin.Protocol{hplugin.ProtocolGRPC},
		StartTimeout:     2 * time.Second,
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

func buildNoopPlugin(t *testing.T) string {
	t.Helper()

	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve caller path")
	}
	moduleRoot := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	pluginBin := filepath.Join(t.TempDir(), "criteria-adapter-noop")

	cmd := exec.Command("go", "build", "-o", pluginBin, "./cmd/criteria-adapter-noop")
	cmd.Dir = moduleRoot
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build noop plugin: %v\n%s", err, string(output))
	}

	return pluginBin
}
