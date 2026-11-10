# KB-394 canary lockfile — pins the criteria-adapter-shell artifact published (4-platform tree, republished deliberately)
# to the LAN registry by the KB-398 gate (PR #597 close-out, 2026-11-10) and
# the container image its manifest declares. The container lane resolves the
# container_image block at session open: the adapter child launches in-image
# from the pinned image ref + digest.
#
# Signature blocks intentionally omitted: this tree pulls with signature
# verification relaxed (--allow-unsigned); the artifact digests below pin
# exactly what was published and consumed.
schema_version = 1

adapter "shell" "crasher" {
  reference            = "localhost:5000/criteria-adapter-shell:v0.5.6"
  version              = "v0.5.6"
  resolved_digest      = "sha256:9acf32f98e7b896b515c37a2f0e83c5352f3ca86d8b84322148781dbc77fe1a5"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]

  container_image {
    ref    = "localhost:5000/criteria-adapter-shell:0.5.5-image"
    digest = "sha256:b6c44272fcc911db6225df77bdd22a85e0228914934d7a2f200fca5762087bc0"
  }
}

adapter "shell" "freshp" {
  reference            = "localhost:5000/criteria-adapter-shell:v0.5.6"
  version              = "v0.5.6"
  resolved_digest      = "sha256:9acf32f98e7b896b515c37a2f0e83c5352f3ca86d8b84322148781dbc77fe1a5"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]

  container_image {
    ref    = "localhost:5000/criteria-adapter-shell:0.5.5-image"
    digest = "sha256:b6c44272fcc911db6225df77bdd22a85e0228914934d7a2f200fca5762087bc0"
  }
}

adapter "shell" "reusep" {
  reference            = "localhost:5000/criteria-adapter-shell:v0.5.6"
  version              = "v0.5.6"
  resolved_digest      = "sha256:9acf32f98e7b896b515c37a2f0e83c5352f3ca86d8b84322148781dbc77fe1a5"
  source_url           = "https://github.com/brokenbots/criteria-adapter-shell"
  sdk_protocol_version = 2
  platforms            = ["linux/amd64", "linux/arm64", "darwin/amd64", "darwin/arm64"]

  container_image {
    ref    = "localhost:5000/criteria-adapter-shell:0.5.5-image"
    digest = "sha256:b6c44272fcc911db6225df77bdd22a85e0228914934d7a2f200fca5762087bc0"
  }
}
