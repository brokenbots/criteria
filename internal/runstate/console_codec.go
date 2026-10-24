package runstate

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The console surface decodes castle.v1 ResolveResumeRequest and encodes
// ResolveResumeResponse in the two media types Connect unary protocol
// clients use. The local loopback control contract defines these shapes by
// field number in castle.v1; JSON stays protojson-compatible (camelCase and
// snake_case field names are both accepted) so the same front-end works
// against either castle or a criteria-local viewer.

type consoleCodec int

const (
	consoleCodecJSON consoleCodec = iota
	consoleCodecProto
)

// contentType returns the media type the codec's responses are labeled with.
func (c consoleCodec) contentType() string {
	if c == consoleCodecProto {
		return "application/proto"
	}
	return "application/json"
}

// consoleCodecName maps a request Content-Type to the codec that decodes
// it, tolerating parameters such as "; charset=utf-8". ok is false for a
// media type the surface does not speak; the caller answers 415 with
// Accept-Post advertising what it does.
func consoleCodecName(contentType string) (consoleCodec, bool) {
	name := contentType
	if i := strings.IndexByte(name, ';'); i >= 0 {
		name = name[:i]
	}
	name = strings.TrimSpace(strings.ToLower(name))
	switch name {
	case "application/json":
		return consoleCodecJSON, true
	case "application/proto":
		return consoleCodecProto, true
	}
	return 0, false
}

type consoleResolveRequest struct {
	runID   string
	signal  string
	payload map[string]string
}

// decodeConsoleResolve decodes a ResolveResumeRequest body. The return
// value derr is a non-empty, end-user-safe problem description when the
// body does not parse; the runID emptiness itself is left to the resolver
// (the local control vocabulary answers it).
func decodeConsoleResolve(body []byte, codec consoleCodec) (req consoleResolveRequest, derr string) {
	switch codec {
	case consoleCodecJSON:
		return decodeConsoleResolveJSON(body)
	case consoleCodecProto:
		return decodeConsoleResolveProto(body)
	}
	return consoleResolveRequest{}, "unknown console codec"
}

func decodeConsoleResolveJSON(body []byte) (req consoleResolveRequest, derr string) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return consoleResolveRequest{}, "invalid JSON request body: " + err.Error()
	}
	req, derr = consoleResolveFields(fields)
	if derr != "" {
		return consoleResolveRequest{}, derr
	}
	if raw, ok := fields["payload"]; ok && string(raw) != "null" {
		var payload map[string]string
		if err := json.Unmarshal(raw, &payload); err != nil {
			return consoleResolveRequest{}, "invalid payload field: want an object of string values"
		}
		req.payload = payload
	}
	return req, ""
}

// consoleResolveFields extracts the runId and signal fields, accepting both
// protojson's camelCase and the snake_case spellings.
func consoleResolveFields(fields map[string]json.RawMessage) (req consoleResolveRequest, derr string) {
	raw, ok := fields["runId"]
	if !ok {
		raw, ok = fields["run_id"]
	}
	if !ok {
		return req, "missing runId field"
	}
	if err := json.Unmarshal(raw, &req.runID); err != nil {
		return req, "invalid runId field: want a string"
	}
	raw, ok = fields["signal"]
	if ok {
		if err := json.Unmarshal(raw, &req.signal); err != nil {
			return req, "invalid signal field: want a string"
		}
	}
	return req, ""
}

// decodeConsoleResolveProto decodes a binary ResolveResumeRequest: field 1
// run_id, field 2 signal, field 3 a map<string,string> payload entry pair.
// Unknown fields are skipped so the surface tolerates forward-compatible
// callers; everything else a real proto codec rejects — a known field with
// an unexpected wire type, a truncated value, a malformed map entry — is
// rejected loudly as well.
func decodeConsoleResolveProto(body []byte) (req consoleResolveRequest, derr string) {
	b := body
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return consoleResolveRequest{}, "invalid proto request body: malformed field tag"
		}
		b = b[n:]
		field, wire := int(tag>>3), int(tag&0x7)
		var ferr string
		b, ferr = decodeConsoleProtoStep(&req, field, wire, b)
		if ferr != "" {
			return consoleResolveRequest{}, ferr
		}
	}
	return req, ""
}

// decodeConsoleProtoField decodes one ResolveResumeRequest field into req.
// recognized reports whether the field number belongs to the schema
// (fields 1, 2, 3), independently of wire type: a known field is fully
// owned here, so a schema-impossible wire type is a hard reject ("want 2")
// exactly as a real proto codec would reject it, never an unknown-field
// skip. derr is non-empty only for hard errors; an unknown field number
// returns (nil, false, "") and the caller skips it generically.
func decodeConsoleProtoField(req *consoleResolveRequest, field, wire int, b []byte) (tail []byte, recognized bool, derr string) {
	var what string
	recognized = true
	switch field {
	case 1:
		what = "run_id"
	case 2:
		what = "signal"
	case 3:
		what = "payload"
	default:
		return nil, false, ""
	}
	if wire != 2 {
		return nil, recognized, fmt.Sprintf("invalid proto request body: field %d (%s) has wire type %d, want 2", field, what, wire)
	}
	v, tail, ok := protoConsumeBytes(b)
	if !ok {
		name := what
		if field == 3 {
			name = "payload entry"
		}
		return nil, recognized, "invalid proto request body: truncated " + name
	}
	if field != 3 {
		if field == 1 {
			req.runID = string(v)
		} else {
			req.signal = string(v)
		}
		return tail, recognized, ""
	}
	k, val, ok := protoConsumeMapEntry(v)
	if !ok {
		return nil, recognized, "invalid proto request body: malformed payload entry"
	}
	req.payload = mergeProtoPayloadEntry(req.payload, k, val)
	return tail, recognized, ""
}

// decodeConsoleProtoStep owns one decode-loop iteration: a schema field
// decodes or hard-rejects; only an unrecognized field number is skipped
// generically.
func decodeConsoleProtoStep(req *consoleResolveRequest, field, wire int, b []byte) (rest []byte, derr string) {
	tail, recognized, ferr := decodeConsoleProtoField(req, field, wire, b)
	if recognized {
		return tail, ferr
	}
	return protoSkipUnknownField(b, wire)
}

// mergeProtoPayloadEntry merges one map<string,string> entry into a
// payload under protobuf map merge semantics: the last value for a repeated
// key wins.
func mergeProtoPayloadEntry(payload map[string]string, k, v string) map[string]string {
	if payload == nil {
		payload = map[string]string{}
	}
	payload[k] = v
	return payload
}

// protoSkipUnknownField consumes an unrecognized field's data for the given
// wire type so a decode loop can step past forward-compatible extensions.
func protoSkipUnknownField(b []byte, wire int) (rest []byte, derr string) {
	switch wire {
	case 0:
		_, tail, ok := protoConsumeVarint(b)
		if !ok {
			return nil, "invalid proto request body: truncated varint field"
		}
		rest = tail
	case 1:
		if len(b) < 8 {
			return nil, "invalid proto request body: truncated fixed64 field"
		}
		rest = b[8:]
	case 2:
		_, tail, ok := protoConsumeBytes(b)
		if !ok {
			return nil, "invalid proto request body: truncated length-delimited field"
		}
		rest = tail
	case 5:
		if len(b) < 4 {
			return nil, "invalid proto request body: truncated fixed32 field"
		}
		rest = b[4:]
	default:
		return nil, fmt.Sprintf("invalid proto request body: unsupported wire type %d", wire)
	}
	return rest, ""
}

func protoConsumeBytes(b []byte) (val, rest []byte, ok bool) {
	n, used := binary.Uvarint(b)
	if used <= 0 || uint64(len(b)-used) < n {
		return nil, nil, false
	}
	return b[used : used+int(n)], b[used+int(n):], true
}

func protoConsumeVarint(b []byte) (v uint64, rest []byte, ok bool) {
	v, n := binary.Uvarint(b)
	if n <= 0 {
		return 0, nil, false
	}
	return v, b[n:], true
}

// protoConsumeMapEntry decodes one map<string,string> entry sub-message:
// field 1 is the key, field 2 the value.
func protoConsumeMapEntry(b []byte) (k, v string, ok bool) {
	for len(b) > 0 {
		tag, n := binary.Uvarint(b)
		if n <= 0 {
			return "", "", false
		}
		b = b[n:]
		field, wire := int(tag>>3), int(tag&0x7)
		if wire != 2 || (field != 1 && field != 2) {
			return "", "", false
		}
		val, rest, ok := protoConsumeBytes(b)
		if !ok {
			return "", "", false
		}
		if field == 1 {
			k, b = string(val), rest
		} else {
			v, b = string(val), rest
		}
	}
	// An entry with empty key and value strings is a legal map entry; an
	// empty-string k is a valid result too.
	return k, v, true
}

// encodeConsoleResolve encodes a ResolveResumeResponse carrying only
// issued_at (field 1).
func encodeConsoleResolve(issuedAt time.Time, codec consoleCodec) (resp []byte, derr string) {
	switch codec {
	case consoleCodecJSON:
		body, err := json.Marshal(struct {
			IssuedAt string `json:"issuedAt"`
		}{IssuedAt: issuedAt.Format(time.RFC3339)})
		if err != nil {
			return nil, "encode console resolve response: " + err.Error()
		}
		return body, ""
	case consoleCodecProto:
		// google.protobuf.Timestamp: field 1 seconds, field 2 nanos.
		sec := issuedAt.Unix()
		ts := appendProtoVarintField(nil, 1, uint64(sec))
		ts = appendProtoVarintField(ts, 2, uint64(issuedAt.Nanosecond()))
		return appendProtoBytesField(nil, 1, ts), ""
	}
	return nil, "unknown console codec"
}

// protoTag builds the field tag for the given wire type (0 = varint,
// 2 = length-delimited).
func protoTag(field int, wireType byte) uint64 {
	return uint64(field)<<3 | uint64(wireType)
}

func appendProtoVarintField(dst []byte, field int, v uint64) []byte {
	dst = binary.AppendUvarint(dst, protoTag(field, 0))
	return binary.AppendUvarint(dst, v)
}

func appendProtoBytesField(dst []byte, field int, val []byte) []byte {
	dst = binary.AppendUvarint(dst, protoTag(field, 2))
	dst = binary.AppendUvarint(dst, uint64(len(val)))
	return append(dst, val...)
}
