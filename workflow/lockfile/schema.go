// Package lockfile implements the .criteria.lock.hcl schema, canonical read/write,
// diff, construction helpers, and validation against a compiled workflow.
//
// # Lockfile grammar
//
//	schema_version = 1
//
//	adapter "<type>" "<name>" {
//	  reference            = "ghcr.io/..."
//	  resolved_digest      = "sha256:..."
//	  source_url           = "https://..."
//	  sdk_protocol_version = 2
//	  platforms            = ["linux/amd64"]
//
//	  signature { ... }
//	  container_image { ... }
//	  remote { ... }
//	}
package lockfile

// Lockfile is the top-level structure of a .criteria.lock.hcl file.
type Lockfile struct {
	SchemaVersion int             `hcl:"schema_version"`
	Adapters      []LockedAdapter `hcl:"adapter,block"`
}
