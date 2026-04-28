package main

import (
	pluginhost "github.com/brokenbots/criteria/sdk/pluginhost"
)

func main() {
	pluginhost.Serve(&copilotPlugin{
		sessions: map[string]*sessionState{},
	})
}
