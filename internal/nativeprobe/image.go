package nativeprobe

/*
#include <stdlib.h>
#include "bridge.h"
*/
import "C"

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"encoding/xml"
	"errors"
	"github.com/kserksi/Pebril/internal/config"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"unsafe"
)

type ImageResult struct {
	Format string `json:"format"`
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Frames int    `json:"frames"`
	Error  string `json:"error,omitempty"`
}

func VerifyLocal(library, rust, input, output string, policy config.Runtime) ImageResult {
	result := ImageResult{}
	header := make([]byte, 512)
	f, err := os.Open(input)
	if err != nil {
		result.Error = "Cannot open uploaded input"
		return result
	}
	n, _ := f.Read(header)
	f.Close()
	header = header[:n]
	gif := bytes.HasPrefix(header, []byte("GIF87a")) || bytes.HasPrefix(header, []byte("GIF89a"))
	if bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n")) {
		animated, e := animatedPNG(input)
		if e != nil || animated {
			result.Error = "Animated or malformed PNG is not supported by this upload validator"
			return result
		}
	}
	raster := bytes.HasPrefix(header, []byte("\x89PNG\r\n\x1a\n")) || bytes.HasPrefix(header, []byte{0xff, 0xd8, 0xff}) || (len(header) >= 12 && string(header[:4]) == "RIFF" && string(header[8:12]) == "WEBP") || isAVIF(header)
	if !raster && !gif {
		result.Format = "svg"
	} else if gif {
		result.Format = "gif"
	} else if isAVIF(header) {
		result.Format = "avif"
	}
	if result.Format != "" && !policy.UploadFormats[result.Format] {
		result.Error = "Upload format is disabled"
		return result
	}
	clib := C.CString(library)
	crust := C.CString(rust)
	cin := C.CString(input)
	cout := C.CString(output)
	defer C.free(unsafe.Pointer(clib))
	defer C.free(unsafe.Pointer(crust))
	defer C.free(unsafe.Pointer(cin))
	defer C.free(unsafe.Pointer(cout))
	var w, h, frames C.int
	var message [2048]C.char
	var format [16]C.char
	limit := max(policy.StaticMaxPixels, policy.AnimatedMaxPixels)
	if result.Format == "svg" || gif {
		svg := C.int(0)
		if result.Format == "svg" {
			svg = 1
		}
		if C.verify_rust_file(crust, cin, cout, svg, C.ulonglong(limit), C.ulonglong(policy.StaticMaxBytes), &w, &h, &frames) == 0 {
			result.Error = "Image content is invalid, unsafe or exceeds the pixel limit"
			return result
		}
		if svg == 1 {
			if err = svgRoot(output); err != nil {
				result.Error = "Sanitized SVG is invalid"
				return result
			}
		}
	} else {
		if C.verify_raster(clib, cin, C.longlong(limit), &w, &h, &frames, &format[0], 16, &message[0], 2048) == 0 {
			result.Error = C.GoString(&message[0])
			return result
		}
		result.Format = C.GoString(&format[0])
	}
	result.Width = int(w)
	result.Height = int(h)
	result.Frames = int(frames)
	if !policy.UploadFormats[result.Format] {
		result.Error = "Upload format is disabled"
		return result
	}
	info, err := os.Stat(input)
	if err != nil {
		result.Error = "Cannot inspect uploaded input"
		return result
	}
	pixels := int64(w) * int64(h)
	pixelLimit, byteLimit := policy.StaticMaxPixels, policy.StaticMaxBytes
	if frames > 1 {
		pixelLimit, byteLimit = policy.AnimatedMaxPixels, policy.AnimatedMaxBytes
	}
	if pixels > pixelLimit/int64(frames) || info.Size() > byteLimit {
		result.Error = "Image exceeds configured pixel or byte limit"
	}
	return result
}
func isAVIF(header []byte) bool {
	if len(header) >= 12 && string(header[4:8]) == "ftyp" && (string(header[8:12]) == "avif" || string(header[8:12]) == "avis") {
		return true
	}
	if len(header) < 16 || string(header[4:8]) != "ftyp" {
		return false
	}
	size := int(binary.BigEndian.Uint32(header[:4]))
	if size < 16 || size > len(header) {
		return false
	}
	for i := 8; i+4 <= size; i += 4 {
		if i == 12 {
			continue
		}
		brand := string(header[i : i+4])
		if brand == "avif" || brand == "avis" {
			return true
		}
	}
	return false
}
func svgRoot(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	decoder := xml.NewDecoder(f)
	depth, roots := 0, 0
	for {
		token, e := decoder.Token()
		if e == io.EOF {
			break
		}
		if e != nil {
			return errors.New("Sanitized SVG is malformed")
		}
		switch t := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
				if t.Name.Local != "svg" || t.Name.Space != "http://www.w3.org/2000/svg" {
					return errors.New("Upload is not SVG")
				}
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) > 0 {
				return errors.New("Invalid SVG content")
			}
		}
	}
	if roots != 1 || depth != 0 {
		return errors.New("Upload is not a complete SVG")
	}
	return nil
}
func Verify(ctx context.Context, executable, library, rust, input, output, policyPath string) (ImageResult, error) {
	childContext, cancel := context.WithCancel(ctx)
	defer cancel()
	command := exec.CommandContext(childContext, executable, "_image-verify", library, rust, input, output, policyPath)
	command.Dir = filepath.Dir(input)
	command.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "VIPS_CONCURRENCY=1", "VIPS_MAX_MEM=16777216", "TMPDIR=" + filepath.Dir(input)}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	command.Cancel = func() error { return syscall.Kill(-command.Process.Pid, syscall.SIGKILL) }
	out := cancelingImageBuffer{cancel: cancel}
	stderr := cancelingImageBuffer{cancel: cancel}
	command.Stdout = &out
	command.Stderr = &stderr
	if err := command.Run(); err != nil {
		return ImageResult{}, errors.New("Isolated image validation failed")
	}
	var result ImageResult
	decoder := json.NewDecoder(&out)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return result, errors.New("Invalid image validation response")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return result, errors.New("Unexpected image validation response")
	}
	if result.Error != "" {
		return result, errors.New(result.Error)
	}
	if result.Frames < 1 || result.Width < 0 || result.Height < 0 {
		return result, errors.New("Invalid image dimensions")
	}
	return result, nil
}

// libvips PNG decoding covers static PNG only; never validate APNG by its first frame.
func animatedPNG(path string) (bool, error) {
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	if _, err = f.Seek(8, 0); err != nil {
		return false, err
	}
	info, err := f.Stat()
	if err != nil {
		return false, err
	}
	offset := int64(8)
	for offset < info.Size() {
		var header [8]byte
		if _, err = io.ReadFull(f, header[:]); err != nil {
			return false, err
		}
		length := int64(binary.BigEndian.Uint32(header[:4]))
		kind := string(header[4:])
		if kind == "acTL" {
			return true, nil
		}
		offset += 12 + length
		if offset > info.Size() {
			return false, io.ErrUnexpectedEOF
		}
		if _, err = f.Seek(length+4, io.SeekCurrent); err != nil {
			return false, err
		}
		if kind == "IEND" {
			if offset != info.Size() {
				return false, errors.New("Unexpected data after PNG end")
			}
			return false, nil
		}
	}
	return false, io.ErrUnexpectedEOF
}

// Reject and terminate a noisy validator instead of leaving its output pipe blocked.
type cancelingImageBuffer struct {
	boundedBuffer
	cancel context.CancelFunc
}

func (buffer *cancelingImageBuffer) Write(data []byte) (int, error) {
	count, err := buffer.boundedBuffer.Write(data)
	if err != nil {
		buffer.cancel()
	}
	return count, err
}
