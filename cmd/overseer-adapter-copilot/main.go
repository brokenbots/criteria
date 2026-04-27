package main

import (
	pluginhost "github.com/brokenbots/overseer/sdk/pluginhost"
)

func main() {
	pluginhost.Serve(&copilotPlugin{
		sessions: map[string]*sessionState{},
	})
}
