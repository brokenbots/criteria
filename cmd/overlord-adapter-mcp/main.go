package main

import (
	pluginpkg "github.com/brokenbots/overseer/internal/plugin"
)

func main() {
	pluginpkg.Serve(&MCPBridge{sessions: map[string]*sessionState{}})
}
