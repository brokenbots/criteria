package main

import (
	adapterhost "github.com/brokenbots/criteria-go-adapter-sdk/adapterhost"
)

func main() {
	adapterhost.Serve(&MCPBridge{sessions: map[string]*sessionState{}})
}
