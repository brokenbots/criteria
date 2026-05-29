schema_version = 1

adapter "claude" "default" {
  reference            = "ghcr.io/criteria-adapters/claude:1.2.3"
  resolved_digest      = "sha256:abc123def456"
  source_url           = "https://github.com/criteria-adapters/claude"
  sdk_protocol_version = 2

  container_image {
    ref    = "ghcr.io/criteria-adapters/claude:1.2.3-image"
    digest = "sha256:def456abc789"
  }
}
