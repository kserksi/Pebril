//go:build !bundled_native

package nativeassets

// Direct source builds omit generated assets; scripts/build.sh creates a deployment program with library recovery.
func Bytes() []byte { return nil }
