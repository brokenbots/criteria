schema_version = 1

adapter "noop" "default" {
  reference            = "ghcr.io/criteria-adapters/noop:latest"
  resolved_digest      = "sha256:000000"
  source_url           = "https://github.com/criteria-adapters/noop"
  sdk_protocol_version = 2
}
