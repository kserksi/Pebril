package nativeinstall

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const maxArchive = 32 << 20
const maxExpanded = 128 << 20
const currentName = "vips-current"

type receipt struct {
	Artifact      string `json:"artifact"`
	ArchiveSHA256 string `json:"archive_sha256"`
	LibrarySHA256 string `json:"library_sha256"`
}

func digest(filename string) (string, error) {
	f, e := os.Open(filename)
	if e != nil {
		return "", e
	}
	defer f.Close()
	h := sha256.New()
	if _, e = io.Copy(h, f); e != nil {
		return "", e
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
func Active(dir string, artifact Artifact) (string, error) {
	target, e := os.Readlink(filepath.Join(dir, currentName))
	if e != nil {
		return "", e
	}
	if filepath.Base(target) != target || !strings.HasPrefix(target, "pebril-vips-") {
		return "", errors.New("Native runtime pointer escapes the installation directory")
	}
	installed := filepath.Join(dir, target)
	if info, e := os.Lstat(installed); e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("Invalid native runtime directory")
	}
	data, e := os.ReadFile(filepath.Join(installed, "receipt.json"))
	if e != nil {
		return "", e
	}
	var r receipt
	if e = json.Unmarshal(data, &r); e != nil {
		return "", e
	}
	if r.Artifact != artifact.ID || r.ArchiveSHA256 != artifact.SHA256 || r.LibrarySHA256 != artifact.LibrarySHA256 {
		return "", errors.New("Native runtime receipt does not match the pinned catalog")
	}
	library := filepath.Join(installed, "libvips.so.42")
	info, e := os.Lstat(library)
	if e != nil || !info.Mode().IsRegular() {
		return "", errors.New("Native runtime is not a regular file")
	}
	sum, e := digest(library)
	if e != nil || sum != r.LibrarySHA256 {
		return "", errors.New("Native runtime integrity check failed")
	}
	return library, nil
}
func client() *http.Client {
	return &http.Client{Timeout: 2 * time.Minute, CheckRedirect: func(request *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("Download redirect limit exceeded")
		}
		if request.URL.Scheme != "https" {
			return errors.New("Non-HTTPS redirects are rejected")
		}
		switch request.URL.Hostname() {
		case "github.com", "release-assets.githubusercontent.com", "objects.githubusercontent.com":
			return nil
		default:
			return errors.New("Unapproved download host is rejected")
		}
	}}
}
func Ensure(ctx context.Context, dir string, verify func(string) error) (string, error) {
	return install(ctx, dir, Vips, client(), verify)
}

func install(ctx context.Context, dir string, artifact Artifact, httpClient *http.Client, verify func(string) error) (string, error) {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", err
	}
	lock, e := os.OpenFile(filepath.Join(dir, ".pebril-install.lock"), os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0600)
	if e != nil {
		return "", e
	}
	defer lock.Close()
	for {
		e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if e == nil {
			break
		}
		if e != syscall.EWOULDBLOCK && e != syscall.EAGAIN {
			return "", e
		}
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	// Collect only incomplete private staging directories while holding the project installation lock.
	entries, e := os.ReadDir(dir)
	if e != nil {
		return "", e
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".pebril-install-") && entry.IsDir() {
			if e = os.RemoveAll(filepath.Join(dir, entry.Name())); e != nil {
				return "", e
			}
		}
	}
	if active, e := Active(dir, artifact); e == nil {
		if e = verify(active); e == nil {
			return active, nil
		}
	}
	stage, e := os.MkdirTemp(dir, ".pebril-install-")
	if e != nil {
		return "", e
	}
	defer os.RemoveAll(stage)
	archive := filepath.Join(stage, "download.tar.gz")
	if e = download(ctx, httpClient, artifact, archive); e != nil {
		return "", e
	}
	if e = extract(archive, stage, artifact.Members); e != nil {
		return "", e
	}
	if e = os.Remove(archive); e != nil {
		return "", e
	}
	sum, e := digest(filepath.Join(stage, "libvips.so.42"))
	if e != nil {
		return "", e
	}
	if sum != artifact.LibrarySHA256 {
		return "", errors.New("Extracted runtime SHA256 mismatch")
	}
	if e = verify(filepath.Join(stage, "libvips.so.42")); e != nil {
		return "", fmt.Errorf("Downloaded runtime failed capability verification: %w", e)
	}
	data, e := json.Marshal(receipt{artifact.ID, artifact.SHA256, sum})
	if e != nil {
		return "", e
	}
	if e = os.WriteFile(filepath.Join(stage, "receipt.json"), data, 0600); e != nil {
		return "", e
	}
	files, e := os.ReadDir(stage)
	if e != nil {
		return "", e
	}
	for _, file := range files {
		f, e := os.Open(filepath.Join(stage, file.Name()))
		if e != nil {
			return "", e
		}
		e = f.Sync()
		f.Close()
		if e != nil {
			return "", e
		}
	}
	current := filepath.Join(dir, currentName)
	if info, e := os.Lstat(current); e == nil && info.Mode()&os.ModeSymlink == 0 {
		return "", errors.New("Refusing to overwrite a non-project vips-current file")
	} else if e != nil && !errors.Is(e, os.ErrNotExist) {
		return "", e
	}
	oldTarget, _ := os.Readlink(current)
	oldOwned := false
	if filepath.Base(oldTarget) == oldTarget && strings.HasPrefix(oldTarget, "pebril-vips-") {
		data, err := os.ReadFile(filepath.Join(dir, oldTarget, "receipt.json"))
		var old receipt
		oldOwned = err == nil && json.Unmarshal(data, &old) == nil && old.Artifact == artifact.ID && old.ArchiveSHA256 == artifact.SHA256 && old.LibrarySHA256 == artifact.LibrarySHA256
	}
	if e = ctx.Err(); e != nil {
		return "", e
	}
	generation := "pebril-vips-" + strings.TrimPrefix(filepath.Base(stage), ".pebril-install-")
	destination := filepath.Join(dir, generation)
	if e = os.Rename(stage, destination); e != nil {
		return "", e
	}
	published := false
	defer func() {
		if !published {
			os.RemoveAll(destination)
		}
	}()
	temporary := filepath.Join(dir, ".pebril-current-"+generation)
	if e = os.Symlink(generation, temporary); e != nil {
		return "", e
	}
	defer os.Remove(temporary)
	if e = os.Rename(temporary, current); e != nil {
		return "", e
	}
	published = true
	parent, e := os.Open(dir)
	if e != nil {
		return "", e
	}
	e = parent.Sync()
	parent.Close()
	if e != nil {
		return "", e
	}
	if oldOwned && oldTarget != generation {
		_ = os.RemoveAll(filepath.Join(dir, oldTarget))
	}
	return filepath.Join(destination, "libvips.so.42"), nil
}
func download(ctx context.Context, client *http.Client, artifact Artifact, destination string) error {
	parsed, e := url.Parse(artifact.URL)
	if e != nil || parsed.Scheme != "https" {
		return errors.New("Runtime source must use HTTPS")
	}
	request, e := http.NewRequestWithContext(ctx, http.MethodGet, artifact.URL, nil)
	if e != nil {
		return e
	}
	response, e := client.Do(request)
	if e != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return errors.New("Runtime download failed; check network and proxy settings")
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return fmt.Errorf("Download HTTP status %d", response.StatusCode)
	}
	if response.ContentLength > maxArchive {
		return errors.New("Archive exceeds 32MiB")
	}
	file, e := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return e
	}
	defer file.Close()
	hash := sha256.New()
	n, e := io.Copy(io.MultiWriter(file, hash), io.LimitReader(response.Body, maxArchive+1))
	if e != nil {
		return e
	}
	if n > maxArchive {
		return errors.New("Archive exceeds 32MiB")
	}
	if hex.EncodeToString(hash.Sum(nil)) != artifact.SHA256 {
		return errors.New("Downloaded archive SHA256 mismatch")
	}
	return file.Sync()
}
func extract(archive, directory string, members map[string]string) error {
	file, e := os.Open(archive)
	if e != nil {
		return e
	}
	defer file.Close()
	gzipReader, e := gzip.NewReader(file)
	if e != nil {
		return e
	}
	defer gzipReader.Close()
	limited := &io.LimitedReader{R: gzipReader, N: maxExpanded + 1}
	reader := tar.NewReader(limited)
	seen := map[string]bool{}
	var size int64
	count := 0
	for {
		header, e := reader.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		count++
		if count > 2000 {
			return errors.New("Too many archive entries")
		}
		clean := path.Clean(header.Name)
		if path.IsAbs(header.Name) || clean == ".." || strings.HasPrefix(clean, "../") || strings.Contains(header.Name, "\\") {
			return errors.New("Archive path escapes the installation directory")
		}
		if header.Size < 0 || header.Size > maxExpanded-size {
			return errors.New("Expanded archive exceeds 128MiB")
		}
		size += header.Size
		target, selected := members[clean]
		if !selected {
			continue
		}
		if seen[clean] || header.Typeflag != tar.TypeReg {
			return errors.New("Runtime entry is duplicated or is not a regular file")
		}
		seen[clean] = true
		if filepath.Base(target) != target || target == "." || target == ".." {
			return errors.New("Invalid installation manifest path")
		}
		output, e := os.OpenFile(filepath.Join(directory, target), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e != nil {
			return e
		}
		_, e = io.Copy(output, reader)
		closeError := output.Close()
		if e != nil {
			return e
		}
		if closeError != nil {
			return closeError
		}
	}
	for name := range members {
		if !seen[name] {
			return fmt.Errorf("Archive is missing %s", name)
		}
	}
	// Consume the gzip trailer to verify CRC and reject hidden oversized content.
	_, e = io.Copy(io.Discard, limited)
	if e != nil {
		return e
	}
	if limited.N <= 0 {
		return errors.New("Archive trailing content exceeds the limit")
	}
	return nil
}
