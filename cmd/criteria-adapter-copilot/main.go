package main

import (
	adapterhost "github.com/brokenbots/criteria/sdk/adapterhost"
)

func main() {
	adapterhost.Serve(&copilotAdapter{
		sessions: map[string]*sessionState{},
	})
}
