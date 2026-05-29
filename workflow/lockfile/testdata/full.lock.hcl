schema_version = 1

adapter "claude" "default" {
  reference            = "ghcr.io/criteria-adapters/claude:1.2.3"
  resolved_digest      = "sha256:abc123def456"
  source_url           = "https://github.com/criteria-adapters/claude"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/arm64"]

  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/criteria-adapters/claude/.github/workflows/publish.yml@refs/tags/v1.2.3"
    }
  }

  container_image {
    ref    = "ghcr.io/criteria-adapters/claude:1.2.3-image"
    digest = "sha256:def456abc789"
  }
}

adapter "copilot" "default" {
  reference            = "ghcr.io/criteria-adapters/copilot:0.5.0"
  resolved_digest      = "sha256:789012"
  source_url           = "https://github.com/criteria-adapters/copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64"]

  signature {
    key {
      algorithm   = "ed25519"
      fingerprint = "sha256:pubkeyfp"
    }
  }

  remote {
    listen_address          = "0.0.0.0:7778"
    server_cert_fingerprint = "sha256:certfp"
  }

  compatible_environments_override = ["shell"]
  overridden_by                    = "workflow.hcl:42"
}
