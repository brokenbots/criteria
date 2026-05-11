# Archived Examples

Workflows in this directory are preserved as historical reference. They are no longer the sanctioned path and are **not** included in `make validate`.

## workstream_review_loop

The original single-file workstream automation. Bundles executor, reviewer, and pr_manager agents plus the full GitHub PR lifecycle into one monolithic HCL workflow. Superseded by the modular subworkflow layout in [`.criteria/workflows/`](../../.criteria/workflows/).

Use `make self` to run the modern flow.
