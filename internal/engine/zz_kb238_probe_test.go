package engine

// Temporary probe (not part of the deliverable): mirrors the KB-238 live shape
// (root with env-only remote develop env + handler subworkflow re-declaring its
// own worktree/primary envs and binding a copilot-style adapter), and dumps the
// compiled env-key space and declared-adapter walk results.

import (
	"fmt"
	"testing"

	"github.com/brokenbots/criteria/workflow/lockfile"
)

func kb238ProbeRootHCL() string {
	return `workflow {
  name          = "kb238-probe"
  version       = "0.1"
  initial_state = "start"
  target_state  = "done"
}

environment "remote" "develop" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

environment "shell" "local" {}

adapter "noop" "default" {
  environment = shell.local
}

subworkflow "handler" {
  source = "./handler"
}

step "start" {
  target = adapter.noop.default
  outcome "success" { next = step.engage }
}

step "engage" {
  target = subworkflow.handler
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
`
}

func kb238ProbeHandlerHCL() string {
	return `workflow {
  name          = "workstream_handler_v1"
  version       = "0.1"
  initial_state = "setup"
  target_state  = "done"
}

environment "remote" "worktree" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

environment "remote" "primary" {
  listen_address     = "127.0.0.1:0"
  per_scope_sessions = true
}

adapter "copilot" "coordinator" {
  environment = remote.worktree
}

step "setup" {
  target = adapter.copilot.coordinator
  outcome "success" { next = state.done }
  outcome "failure" { next = state.failed }
}

state "done" {
  terminal = true
  success  = true
}

state "failed" {
  terminal = true
  success  = false
}
`
}

func TestProbe_KB238ShapeEnvKeys(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root+"/main.chcl", kb238ProbeRootHCL())
	writeFile(t, root+"/handler/main.chcl", kb238ProbeHandlerHCL())
	writeLockfile(t, root, &lockfile.Lockfile{
		SchemaVersion: 1,
		Adapters: []lockfile.LockedAdapter{
			{Type: "copilot", Name: "coordinator", ResolvedDigest: "sha256:abcd1234"},
		},
	})
	g := compileWorkflowDir(t, root)

	fmt.Printf("=== ROOT Environments keys:\n")
	for k := range g.Environments {
		fmt.Printf("  %q type=%s\n", k, g.Environments[k].Type)
	}
	fmt.Printf("=== ROOT DefaultEnvironment: %q\n", g.DefaultEnvironment)
	for _, swName := range g.SubworkflowOrder {
		sw := g.Subworkflows[swName]
		fmt.Printf("=== subworkflow %q env=%q body envs:\n", swName, sw.Environment)
		for k := range sw.Body.Environments {
			fmt.Printf("  %q type=%s\n", k, sw.Body.Environments[k].Type)
		}
		fmt.Printf("  body DefaultEnvironment: %q\n", sw.Body.DefaultEnvironment)
		for _, id := range sw.Body.AdapterOrder {
			ad := sw.Body.Adapters[id]
			fmt.Printf("  body adapter %q type=%s env=%q\n", id, ad.Type, ad.Environment)
		}
	}
	fmt.Printf("=== declared walk results:\n")
	fmt.Printf("  remote.develop: %v\n", declaredAdaptersForEnv("remote.develop", g))
	fmt.Printf("  remote.worktree: %v\n", declaredAdaptersForEnv("remote.worktree", g))
	fmt.Printf("  remote.primary: %v\n", declaredAdaptersForEnv("remote.primary", g))
	fmt.Printf("  shell.local: %v\n", declaredAdaptersForEnv("shell.local", g))
}
