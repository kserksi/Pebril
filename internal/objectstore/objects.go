package objectstore

import (
	"context"
	"crypto/md5"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/kserksi/Pebril/internal/config"
)

var ErrUnavailable = errors.New("Selected image storage is unavailable")
var ErrIntegrity = errors.New("Stored image failed integrity verification")
var ErrNamespace = errors.New("Object namespace is not exclusively owned by this database")

type File interface {
	io.ReadSeekCloser
	Stat() (os.FileInfo, error)
}
type Objects struct {
	directory string
	remote    *remote
}

func New(ctx context.Context, directory string, configuration config.Storage, namespace string) (*Objects, error) {
	objects := &Objects{directory: directory}
	if configuration.Backend == "s3" {
		if err := configuration.Validate(); err != nil {
			return nil, err
		}
		identity, err := hex.DecodeString(namespace)
		if err != nil || len(identity) != 16 || hex.EncodeToString(identity) != namespace {
			return nil, ErrNamespace
		}
		objects.remote = newRemote(*configuration.S3, namespace, nil)
		if err := objects.remote.claim(ctx); err != nil {
			return nil, err
		}
	} else if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	return objects, nil
}
func ValidHash(hash string) bool {
	raw, err := hex.DecodeString(hash)
	return err == nil && len(raw) == 32 && hex.EncodeToString(raw) == hash
}
func (o *Objects) Remote() bool { return o.remote != nil }

// Publish verifies existing bytes as well as new writes before a DB reference can be committed.
func (o *Objects) Publish(ctx context.Context, path string) (string, int64, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 {
		return "", 0, ErrIntegrity
	}
	sha, md := sha256.New(), md5.New()
	count, err := io.Copy(io.MultiWriter(sha, md), file)
	if err != nil || count != info.Size() {
		return "", 0, ErrIntegrity
	}
	hash := hex.EncodeToString(sha.Sum(nil))
	if _, err = file.Seek(0, 0); err != nil {
		return "", 0, err
	}
	if o.remote != nil {
		err = o.remote.publish(ctx, hash, count, base64.StdEncoding.EncodeToString(md.Sum(nil)), file)
		return hash, count, err
	}
	stage, err := os.CreateTemp(o.directory, ".object-")
	if err != nil {
		return "", 0, err
	}
	defer os.Remove(stage.Name())
	_, err = io.Copy(stage, file)
	if err == nil {
		err = stage.Sync()
	}
	closed := stage.Close()
	if err == nil {
		err = closed
	}
	if err != nil {
		return "", 0, err
	}
	target := filepath.Join(o.directory, hash)
	if err = os.Link(stage.Name(), target); err != nil && !errors.Is(err, os.ErrExist) {
		return "", 0, err
	}
	existing, err := os.Open(target)
	if err != nil {
		return "", 0, err
	}
	check := sha256.New()
	n, err := io.Copy(check, existing)
	existing.Close()
	if err != nil || n != count || hex.EncodeToString(check.Sum(nil)) != hash {
		return "", 0, ErrIntegrity
	}
	directory, err := os.Open(o.directory)
	if err != nil {
		return "", 0, err
	}
	err = directory.Sync()
	directory.Close()
	return hash, count, err
}
func (o *Objects) Open(ctx context.Context, hash string, size int64) (File, error) {
	if !ValidHash(hash) {
		return nil, os.ErrNotExist
	}
	if o.remote != nil {
		return o.remote.open(ctx, hash, size)
	}
	file, err := os.Open(filepath.Join(o.directory, hash))
	if err != nil {
		return nil, err
	}
	return file, nil
}
func (o *Objects) Stage(ctx context.Context, hash string, size int64, path string) error {
	input, err := o.Open(ctx, hash, size)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	complete := false
	defer func() {
		output.Close()
		if !complete {
			os.Remove(path)
		}
	}()
	digest := sha256.New()
	n, err := io.Copy(io.MultiWriter(output, digest), io.LimitReader(input, size+1))
	if err != nil {
		return ErrUnavailable
	}
	if n != size || hex.EncodeToString(digest.Sum(nil)) != hash {
		return ErrIntegrity
	}
	if err = output.Sync(); err != nil {
		return err
	}
	if err = output.Close(); err != nil {
		return err
	}
	complete = true
	return nil
}
func (o *Objects) Remove(ctx context.Context, hash string) error {
	if !ValidHash(hash) {
		return ErrIntegrity
	}
	if o.remote != nil {
		return o.remote.remove(ctx, hash)
	}
	err := os.Remove(filepath.Join(o.directory, hash))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}
func (o *Objects) Reconcile(ctx context.Context, referenced func(string) bool) error {
	if o.remote != nil {
		return o.remote.reconcile(ctx, referenced)
	}
	entries, err := os.ReadDir(o.directory)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if ValidHash(name) && !referenced(name) || strings.HasPrefix(name, ".object-") {
			if err = os.Remove(filepath.Join(o.directory, name)); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		}
	}
	return nil
}
func (o *Objects) Check(ctx context.Context) error {
	if o.remote != nil {
		return o.remote.check(ctx)
	}
	info, err := os.Stat(o.directory)
	if err != nil || !info.IsDir() {
		return ErrUnavailable
	}
	return nil
}

// CheckConnection is read-only and does not claim a namespace or initialize business data.
func CheckConnection(ctx context.Context, c config.Storage) error {
	if c.Backend != "s3" {
		return nil
	}
	if err := c.Validate(); err != nil {
		return err
	}
	r := newRemote(*c.S3, "", nil)
	return r.connection(ctx)
}
