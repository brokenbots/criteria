package main

import (
	adapterhost "github.com/brokenbots/criteria/sdk/adapterhost"
)

func main() {
	adapterhost.Serve(&MCPBridge{sessions: map[string]*sessionState{}})
}
