package main

import (
	pluginhost "github.com/brokenbots/overseer/sdk/pluginhost"
)

func main() {
	pluginhost.Serve(&MCPBridge{sessions: map[string]*sessionState{}})
}
