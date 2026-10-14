package nativeassets

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"errors"
	"io"
	"os"
	"path/filepath"
)

func Ensure(dir string) error {
	compressed := Bytes()
	if len(compressed) == 0 {
		return nil
	}
	reader, e := gzip.NewReader(bytes.NewReader(compressed))
	if e != nil {
		return e
	}
	defer reader.Close()
	data, e := io.ReadAll(io.LimitReader(reader, 8*1024*1024+1))
	if e != nil {
		return e
	}
	if len(data) > 8*1024*1024 {
		return errors.New("Internal native library exceeds 8MiB")
	}
	destination := filepath.Join(dir, "libpebril_native.so")
	if info, e := os.Lstat(destination); e == nil && info.Mode().IsRegular() && info.Size() == int64(len(data)) {
		if current, e := os.ReadFile(destination); e == nil && sha256.Sum256(current) == sha256.Sum256(data) {
			return nil
		}
	}
	file, e := os.CreateTemp(dir, ".pebril-native-")
	if e != nil {
		return e
	}
	defer os.Remove(file.Name())
	_, e = file.Write(data)
	if e == nil {
		e = file.Sync()
	}
	closeError := file.Close()
	if e != nil {
		return e
	}
	if closeError != nil {
		return closeError
	}
	if e = os.Rename(file.Name(), destination); e != nil {
		return e
	}
	parent, e := os.Open(dir)
	if e != nil {
		return e
	}
	defer parent.Close()
	return parent.Sync()
}
