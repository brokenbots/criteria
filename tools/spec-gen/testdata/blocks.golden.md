### `widget "name" { ... }`

- **Source:** [`testdata/schema_sample.go:15`](../testdata/schema_sample.go#L15)
- **Labels:** `name`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `title` | string | yes | Title is the display label shown in the UI. |
| `enabled` | bool | no | Enabled controls whether the widget is active. |


### `rule "id" { ... }`

- **Source:** [`testdata/schema_sample.go:26`](../testdata/schema_sample.go#L26)
- **Labels:** `id`
- **Attributes:**

| Attribute | Type | Required | Description |
|---|---|---|---|
| `priority` | number | yes | Priority sets the evaluation order; higher values run first. |