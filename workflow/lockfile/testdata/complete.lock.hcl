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

  remote {
    listen_address          = "0.0.0.0:7778"
    server_cert_fingerprint = "sha256:certfp"
  }
}

workflow_ref "loop" {
  source       = "./loop"
  resolved_ref = "abc"
  kind         = "git"
}
