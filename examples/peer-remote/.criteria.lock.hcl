# Demo lockfile for the peer-remote example.
#
# The remote shim's digest gate is fail-closed: every peer dial must present
# the resolved_digest pinned here for its adapter type, and the compose file
# injects the same placeholder digest as CRITERIA_REMOTE_DIGEST. This demo
# therefore pins a placeholder digest — it is an identity agreement between
# the host's lockfile and the operator-injected peer env, not a signature
# verification. In a real deployment run `criteria adapter lock` in this
# directory to pin real digests (and signer identities) from the OCI
# artifacts, then feed the pinned digest to your peers via the
# provision_wanted lifecycle event.
schema_version = 1

adapter "shell" "main" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.3"
  resolved_digest      = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
}

adapter "noop" "gate" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-noop:0.5.2"
  resolved_digest      = "sha256:0000000000000000000000000000000000000000000000000000000000000000"
  source_url           = "https://github.com/brokenbots/criteria-adapter-noop"
  sdk_protocol_version = 2
}