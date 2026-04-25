package plugin

import (
	"context"
	"testing"
)

func TestLoaderResolveNoopPlugin(t *testing.T) {
	pluginBin := buildNoopPlugin(t)
	loader := NewLoaderWithDiscovery(func(string) (string, error) {
		return pluginBin, nil
	})
	t.Cleanup(func() {
		_ = loader.Shutdown(context.Background())
	})

	p, err := loader.Resolve(context.Background(), "noop")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	info, err := p.Info(context.Background())
	if err != nil {
		t.Fatalf("info: %v", err)
	}
	if info.Name != "noop" {
		t.Fatalf("plugin name=%q want noop", info.Name)
	}
	if info.Version == "" {
		t.Fatal("expected non-empty plugin version")
	}
}
