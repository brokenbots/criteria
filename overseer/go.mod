module github.com/brokenbots/overlord/overseer

go 1.26

require (
	github.com/brokenbots/overlord/shared v0.0.0
	github.com/brokenbots/overlord/workflow v0.0.0
	github.com/coder/websocket v1.8.12
	github.com/github/copilot-sdk/go v0.2.2
	github.com/google/uuid v1.6.0
	github.com/spf13/cobra v1.8.1
)

require (
	github.com/agext/levenshtein v1.2.1 // indirect
	github.com/apparentlymart/go-textseg/v15 v15.0.0 // indirect
	github.com/go-logr/logr v1.4.3 // indirect
	github.com/go-logr/stdr v1.2.2 // indirect
	github.com/google/go-cmp v0.7.0 // indirect
	github.com/google/jsonschema-go v0.4.2 // indirect
	github.com/hashicorp/hcl/v2 v2.20.1 // indirect
	github.com/inconshreveable/mousetrap v1.1.0 // indirect
	github.com/mitchellh/go-wordwrap v0.0.0-20150314170334-ad45545899c7 // indirect
	github.com/spf13/pflag v1.0.5 // indirect
	github.com/zclconf/go-cty v1.14.4 // indirect
	go.opentelemetry.io/auto/sdk v1.1.0 // indirect
	go.opentelemetry.io/otel v1.35.0 // indirect
	go.opentelemetry.io/otel/metric v1.35.0 // indirect
	go.opentelemetry.io/otel/trace v1.35.0 // indirect
	golang.org/x/mod v0.8.0 // indirect
	golang.org/x/sys v0.5.0 // indirect
	golang.org/x/text v0.11.0 // indirect
	golang.org/x/tools v0.6.0 // indirect
)

replace (
	github.com/brokenbots/overlord/shared => ../shared
	github.com/brokenbots/overlord/workflow => ../workflow
)
