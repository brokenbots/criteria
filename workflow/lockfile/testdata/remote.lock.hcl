schema_version = 1

adapter "copilot" "default" {
  reference            = "ghcr.io/criteria-adapters/copilot:0.5.0"
  resolved_digest      = "sha256:789012"
  source_url           = "https://github.com/criteria-adapters/copilot"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64"]

  remote {
    listen_address          = "0.0.0.0:7778"
    server_cert_fingerprint = "sha256:certfp"
  }
}
