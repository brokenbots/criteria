package main

import (
	pluginpkg "github.com/brokenbots/overseer/internal/plugin"
)

func main() {
	pluginpkg.Serve(&copilotPlugin{
		sessions: map[string]*sessionState{},
	})
}
