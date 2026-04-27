package main

import (
	pluginpkg "github.com/brokenbots/overlord/overseer/internal/plugin"
)

func main() {
	pluginpkg.Serve(&copilotPlugin{
		sessions: map[string]*sessionState{},
	})
}
