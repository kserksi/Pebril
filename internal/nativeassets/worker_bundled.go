//go:build bundled_native

package nativeassets

import _ "embed"

//go:embed runtime_worker.gz
var compressedWorker []byte

func WorkerBytes() []byte { return compressedWorker }
