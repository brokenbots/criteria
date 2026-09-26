schema_version = 1
adapter "noop" "gate" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-noop:0.5.2"
  version              = "0.5.2"
  resolved_digest      = "sha256:00ab4151baacba3b14e89cdd5c99e0c1924e23198d0ff91387f995021d75ce4f"
  source_url           = "https://github.com/brokenbots/criteria-adapter-noop"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-noop/.github/workflows/publish.yml@refs/tags/v0.5.2"
    }
  }
}
adapter "shell" "main" {
  reference            = "ghcr.io/brokenbots/criteria-adapter-shell:0.5.3"
  version              = "0.5.3"
  resolved_digest      = "sha256:d9f306c29f4145da8bcc44187c9e4ae0f69ed30db3b3edac6e9b6350469bc635"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]
  signature {
    keyless {
      issuer  = "https://token.actions.githubusercontent.com"
      subject = "https://github.com/brokenbots/criteria-adapter-shell/.github/workflows/publish.yml@refs/tags/v0.5.3"
    }
  }
}
