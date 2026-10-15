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
	return restore(dir, "libpebril_native.so", Bytes(), 8*1024*1024, 0600)
}

func EnsureWorker(dir string) error {
	return restore(dir, "pebril-image-worker", WorkerBytes(), 32*1024*1024, 0700)
}

func restore(dir, name string, compressed []byte, maximum int64, mode os.FileMode) error {
	if len(compressed) == 0 {
		return nil
	}
	reader, e := gzip.NewReader(bytes.NewReader(compressed))
	if e != nil {
		return e
	}
	defer reader.Close()
	data, e := io.ReadAll(io.LimitReader(reader, maximum+1))
	if e != nil {
		return e
	}
	if int64(len(data)) > maximum {
		return errors.New("Embedded component exceeds its size limit")
	}
	destination := filepath.Join(dir, name)
	if info, e := os.Lstat(destination); e == nil && info.Mode().IsRegular() && info.Size() == int64(len(data)) && info.Mode().Perm() == mode {
		if current, e := os.ReadFile(destination); e == nil && sha256.Sum256(current) == sha256.Sum256(data) {
			return nil
		}
	}
	file, e := os.CreateTemp(dir, ".pebril-native-")
	if e != nil {
		return e
	}
	defer os.Remove(file.Name())
	if e = file.Chmod(mode); e != nil {
		file.Close()
		return e
	}
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
