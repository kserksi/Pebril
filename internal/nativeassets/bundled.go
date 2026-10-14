//go:build bundled_native

package nativeassets

import _ "embed"

//go:embed runtime_native.gz
var compressed []byte

func Bytes() []byte { return compressed }
