package manifest

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	v2 "github.com/brokenbots/criteria-adapter-proto/criteria/v2"
)

// Verify compares the static manifest from adapter.yaml to the runtime
// Info() response. Divergence in any of the checked fields is fatal.
//
// Checked fields:
//   - name
//   - version
//   - sdk_protocol_version
//   - capabilities (set equality)
//   - platforms (set equality)
//   - config_schema, input_schema, output_schema (structural equality)
//   - declared secrets (set of names)
//   - compatible_environments (set equality; absent and ["*"] normalised to "any")
//
// Other fields (description, source_url, permissions) are allowed to differ
// at runtime: they're advisory or human-facing.
func Verify(static *Manifest, runtime *v2.InfoResponse) error {
	var errs []string

	errs = appendScalarDiffs(errs, static, runtime)
	errs = appendSetDiff(errs, "capabilities", static.Capabilities, runtime.GetCapabilities())
	errs = appendSetDiff(errs, "platforms", platformStrings(static.Platforms), runtime.GetPlatforms())

	for _, kind := range []string{"config_schema", "input_schema", "output_schema"} {
		err := schemaDiffFromKind(kind, static, runtime)
		if err != "" {
			errs = append(errs, err)
		}
	}

	errs = appendSetDiff(errs, "secrets", secretNames(static.Secrets), secretNamesFromMap(runtime.GetSecrets()))
	errs = appendSetDiff(errs, "compatible_environments",
		sortedStrings(normaliseCompatibleEnvironments(static.CompatibleEnvironments)),
		sortedStrings(normaliseCompatibleEnvironments(runtime.GetCompatibleEnvironments())))

	if len(errs) > 0 {
		return fmt.Errorf("adapter %q: manifest/runtime mismatch: %s", static.Name, strings.Join(errs, "; "))
	}
	return nil
}

func appendScalarDiffs(errs []string, static *Manifest, runtime *v2.InfoResponse) []string {
	if static.Name != runtime.GetName() {
		errs = append(errs, fmt.Sprintf("name: manifest=%q runtime=%q", static.Name, runtime.GetName()))
	}
	if static.Version != runtime.GetVersion() {
		errs = append(errs, fmt.Sprintf("version: manifest=%q runtime=%q", static.Version, runtime.GetVersion()))
	}
	if fmtInt(static.SDKProtocolVersion) != runtime.GetSdkProtocolVersion() {
		errs = append(errs, fmt.Sprintf("sdk_protocol_version: manifest=%d runtime=%q", static.SDKProtocolVersion, runtime.GetSdkProtocolVersion()))
	}
	return errs
}

func appendSetDiff(errs []string, label string, a, b []string) []string {
	if !slices.Equal(sortedStrings(a), sortedStrings(b)) {
		errs = append(errs, fmt.Sprintf("%s: manifest=%v runtime=%v", label, a, b))
	}
	return errs
}

func schemaDiffFromKind(kind string, static *Manifest, runtime *v2.InfoResponse) string {
	switch kind {
	case "config_schema":
		return schemaDiff(kind, static.ConfigSchema, runtime.GetConfigSchema())
	case "input_schema":
		return schemaDiff(kind, static.InputSchema, runtime.GetInputSchema())
	case "output_schema":
		return schemaDiff(kind, static.OutputSchema, runtime.GetOutputSchema())
	}
	return ""
}

func platformStrings(ps []Platform) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.OS + "/" + p.Arch
	}
	return out
}

func secretNames(ss []SecretDecl) []string {
	out := make([]string, len(ss))
	for i, s := range ss {
		out[i] = s.Name
	}
	return out
}

func secretNamesFromMap(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// schemaDiff compares a static Schema to a runtime AdapterSchemaProto.
// Two schemas are equal iff they have the same set of field names, and for
// every name the (type, required, sensitive) triple is equal.
// description and default are explicitly ignored.
// Returns the first divergence found with both sides quoted.
func schemaDiff(label string, static Schema, runtime *v2.AdapterSchemaProto) string {
	if runtime == nil {
		if len(static.Fields) == 0 {
			return ""
		}
		return fmt.Sprintf("%s: manifest has %d fields but runtime has none", label, len(static.Fields))
	}

	runtimeFields := runtime.GetFields()
	if len(static.Fields) != len(runtimeFields) {
		return fmt.Sprintf("%s: manifest has %d fields but runtime has %d", label, len(static.Fields), len(runtimeFields))
	}

	staticNames := make([]string, 0, len(static.Fields))
	for k := range static.Fields {
		staticNames = append(staticNames, k)
	}
	sort.Strings(staticNames)

	for _, name := range staticNames {
		rf, ok := runtimeFields[name]
		if !ok {
			return fmt.Sprintf("%s: manifest has field %q but runtime does not", label, name)
		}
		sf := static.Fields[name]

		if sf.Type != rf.GetType() {
			return fmt.Sprintf("%s.fields[%q].type: manifest=%q runtime=%q", label, name, sf.Type, rf.GetType())
		}
		if sf.Required != rf.GetRequired() {
			return fmt.Sprintf("%s.fields[%q].required: manifest=%v runtime=%v", label, name, sf.Required, rf.GetRequired())
		}
		if sf.Sensitive != rf.GetSensitive() {
			return fmt.Sprintf("%s.fields[%q].sensitive: manifest=%v runtime=%v", label, name, sf.Sensitive, rf.GetSensitive())
		}
	}

	return ""
}
