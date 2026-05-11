### `widget "name" { ... }`

- **Source:** [`testdata/schema_sample.go:15`](../testdata/schema_sample.go#L15)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `title` | string | yes | Title is the display label shown in the UI. |
| `enabled` | bool | no | Enabled controls whether the widget is active. |

- **Additional attributes:** Captures: style (optional, one of "compact" or "expanded"); icon (optional, string).

### `rule "id" { ... }`

- **Source:** [`testdata/schema_sample.go:27`](../testdata/schema_sample.go#L27)
- **Labels:** `id`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `priority` | number | yes | Priority sets the evaluation order; higher values run first. |