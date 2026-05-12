# mode: standalone
# Example: demonstrates hash, encoding, and dynamic HCL functions.
workflow "hash_encoding_demo" {
  version       = "1"
  initial_state = "compute"
  target_state  = "done"
}

variable "input" {
  type    = "string"
  default = "hello world"
}

local "fingerprint" {
  description = "SHA-256 fingerprint of the input"
  value       = sha256(var.input)
}

local "envelope" {
  description = "Base64-encoded JSON envelope containing the payload and fingerprint"
  value       = base64encode(jsonencode({ payload = var.input, fingerprint = local.fingerprint }))
}

adapter "shell" "logger" {}

step "compute" {
  target = adapter.shell.logger
  input {
    command = "echo Envelope: ${local.envelope}"
  }
  outcome "success" { next = "done" }
}

state "done" {
  terminal = true
  success  = true
}
