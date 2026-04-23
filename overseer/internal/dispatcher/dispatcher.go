// Package dispatcher resolves adapter names to Adapter implementations.
package dispatcher

import "github.com/brokenbots/overlord/overseer/internal/adapter"

type Map struct {
	m map[string]adapter.Adapter
}

func New() *Map { return &Map{m: map[string]adapter.Adapter{}} }

func (d *Map) Register(a adapter.Adapter) { d.m[a.Name()] = a }

func (d *Map) Adapter(name string) (adapter.Adapter, bool) {
	a, ok := d.m[name]
	return a, ok
}
