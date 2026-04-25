package main

import (
	pluginpkg "github.com/brokenbots/overlord/overseer/internal/plugin"
)

func main() {
	pluginpkg.Serve(&MCPBridge{sessions: map[string]*sessionState{}})
}
