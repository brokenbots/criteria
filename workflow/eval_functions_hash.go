package workflow

// eval_functions_hash.go — SHA-256, SHA-1, SHA-512, and MD5 HCL functions.

import (
	"crypto/md5"  //nolint:gosec // exposed by deliberate design for caching/identity use; documented as insecure
	"crypto/sha1" //nolint:gosec // same
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"hash"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

func registerHashFunctions() map[string]function.Function {
	return map[string]function.Function{
		"sha256": hashFunction(sha256.New),
		"sha1":   hashFunction(sha1.New), //nolint:gosec // intentional: weak hash exposed by design for caching/identity
		"sha512": hashFunction(sha512.New),
		"md5":    hashFunction(md5.New), //nolint:gosec // intentional: weak hash exposed by design for caching/identity
	}
}

// hashFunction constructs a cty function that hex-encodes the digest produced
// by the given hash.Hash factory over the input string.
func hashFunction(newHash func() hash.Hash) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			h := newHash()
			h.Write([]byte(args[0].AsString()))
			return cty.StringVal(hex.EncodeToString(h.Sum(nil))), nil
		},
	})
}
