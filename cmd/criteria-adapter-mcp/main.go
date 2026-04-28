package main

import (
	pluginhost "github.com/brokenbots/criteria/sdk/pluginhost"
)

func main() {
	pluginhost.Serve(&MCPBridge{sessions: map[string]*sessionState{}})
}
