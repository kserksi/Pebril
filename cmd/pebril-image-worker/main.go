//go:build image_worker

package main

/*
#cgo pkg-config: vips
#cgo LDFLAGS: -ldl -lm
#include <stdlib.h>
#include "worker.h"
*/
import "C"

import (
	"encoding/json"
	"errors"
	"github.com/kserksi/Pebril/internal/config"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"unsafe"

	"github.com/davidbyttow/govips/v2/vips"
	"github.com/kserksi/Pebril/internal/imageproc"
)

func main() {
	result := imageproc.Result{}
	if err := vips.Startup(&vips.Config{ConcurrencyLevel: 1, MaxCacheFiles: 0, MaxCacheMem: 16 * 1024 * 1024, MaxCacheSize: 0}); err != nil {
		result.Error = "Cannot initialize conversion runtime"
	} else {
		defer vips.Shutdown()
		vips.LoggingSettings(func(string, vips.LogLevel, string) {}, vips.LogLevelError)
		result.Library = C.GoString(C.worker_library())
		if len(os.Args) == 2 && os.Args[1] == "check" {
			result.Error = check()
		} else if len(os.Args) == 1 {
			decoder := json.NewDecoder(io.LimitReader(os.Stdin, 1024*1024+1))
			decoder.DisallowUnknownFields()
			var request imageproc.Request
			if decoder.Decode(&request) != nil || decoder.Decode(new(any)) != io.EOF {
				result.Error = "Invalid conversion request"
			} else {
				var err error
				result, err = convert(request)
				if err != nil {
					result.Error = "Image conversion failed"
				}
			}
		} else {
			result.Error = "Invalid converter command"
		}
	}
	result.Version = vips.Version
	if json.NewEncoder(os.Stdout).Encode(result) != nil {
		os.Exit(1)
	}
}
func check() string {
	// The parent also verifies real supported-format output before enabling uploads.
	if C.worker_capabilities() == 0 {
		return "Required conversion operations are unavailable"
	}
	directory, err := os.Getwd()
	if err != nil {
		return "Cannot locate conversion self-test directory"
	}
	input := filepath.Join(directory, "probe.png")
	pixels := image.NewRGBA(image.Rect(0, 0, 2, 2))
	pixels.Set(0, 0, color.RGBA{R: 255, A: 255})
	file, err := os.Create(input)
	if err != nil {
		return "Cannot write conversion self-test"
	}
	err = png.Encode(file, pixels)
	closed := file.Close()
	if err != nil || closed != nil {
		return "Cannot encode conversion self-test"
	}
	defer os.Remove(input)
	defer os.Remove(filepath.Join(directory, "result"))
	result, err := convert(imageproc.Request{Input: input, Directory: directory, Frames: 1, Policy: config.Defaults()})
	if err != nil || result.Width != 2 || result.Height != 2 {
		return "Conversion self-test failed"
	}
	return ""
}
func transform(image *vips.ImageRef, request imageproc.Request) error {
	p := request.Policy
	if !p.PreserveOrientation {
		if err := image.RemoveOrientation(); err != nil {
			return err
		}
	}
	if p.PreserveOrientation {
		if err := image.AutoRotate(); err != nil {
			return err
		}
	}
	if p.OutputWidth > 0 {
		scale := math.Min(1, math.Min(float64(p.OutputWidth)/float64(image.Width()), float64(p.OutputHeight)/float64(image.PageHeight())))
		if scale < 1 {
			if err := image.Resize(scale, vips.KernelLanczos3); err != nil {
				return err
			}
		}
	}
	if err := image.RemoveMetadata(); err != nil {
		return err
	}
	if !p.PreserveColor {
		if err := image.RemoveICCProfile(); err != nil {
			return err
		}
	}
	return nil
}
func colorSource(request imageproc.Request, page int, name string) (string, error) {
	if !request.Policy.PreserveColor {
		return request.Input, nil
	}
	output := filepath.Join(request.Directory, name)
	input, target := C.CString(request.Input), C.CString(output)
	defer C.free(unsafe.Pointer(input))
	defer C.free(unsafe.Pointer(target))
	// Use reviewed libvips built-ins without linking govips's additional embedded profiles.
	status := C.worker_color(input, target, C.int(page))
	if status == 0 {
		return "", errors.New("Color normalization failed")
	}
	if status == 2 {
		return output, nil
	}
	return request.Input, nil
}
func convert(request imageproc.Request) (imageproc.Result, error) {
	result := imageproc.Result{}
	if request.Frames < 1 || request.Input == "" || request.Directory == "" {
		return result, errors.New("Invalid image request")
	}
	if request.Frames > 1 {
		format := request.Policy.AnimatedFormat
		if format != "webp" && format != "gif" {
			return result, errors.New("Invalid animated output")
		}
		args := []string{request.Input, filepath.Join(request.Directory, "result"), request.Rust, format}
		values := make([]*C.char, len(args))
		for i, s := range args {
			values[i] = C.CString(s)
			defer C.free(unsafe.Pointer(values[i]))
		}
		var w, h, n C.int
		if C.worker_animation(values[0], values[1], values[2], values[3], C.int(request.Policy.AnimatedQuality), C.int(request.Policy.OutputWidth), C.int(request.Policy.OutputHeight), C.int(boolInt(request.Policy.PreserveOrientation)), C.int(boolInt(request.Policy.PreserveColor)), &w, &h, &n) == 0 {
			return result, errors.New("Animated conversion failed")
		}
		result = imageproc.Result{Format: format, Width: int(w), Height: int(h), Frames: int(n)}
		params := vips.NewImportParams()
		params.Page.Set(request.Policy.CoverFrame)
		params.NumPages.Set(1)
		colorPath := filepath.Join(request.Directory, "cover-color.png")
		defer os.Remove(colorPath)
		colorInput, err := colorSource(request, request.Policy.CoverFrame, "cover-color.png")
		if err != nil {
			return result, err
		}
		if colorInput != request.Input {
			params.Page.Set(0)
		}
		coverSource, err := os.Open(colorInput)
		if err != nil {
			return result, err
		}
		defer coverSource.Close()
		cover, err := vips.LoadImageFromReader(coverSource, params)
		if err != nil {
			return result, err
		}
		defer cover.Close()
		if err = transform(cover, request); err != nil {
			return result, err
		}
		if err = save(cover, filepath.Join(request.Directory, "cover"), "webp", request.Policy.StaticQuality); err != nil {
			return result, err
		}
		result.Cover = true
		return result, nil
	}
	colorPath := filepath.Join(request.Directory, "source-color.png")
	defer os.Remove(colorPath)
	colorInput, err := colorSource(request, -1, "source-color.png")
	if err != nil {
		return result, err
	}
	source, err := os.Open(colorInput)
	if err != nil {
		return result, err
	}
	defer source.Close()
	image, err := vips.LoadImageFromReader(source, nil)
	if err != nil {
		return result, err
	}
	defer image.Close()
	if err = transform(image, request); err != nil {
		return result, err
	}
	format := request.Policy.StaticFormat
	if err = save(image, filepath.Join(request.Directory, "result"), format, request.Policy.StaticQuality); err != nil {
		return result, err
	}
	return imageproc.Result{Format: format, Width: image.Width(), Height: image.Height(), Frames: 1}, nil
}
func save(image *vips.ImageRef, path, format string, quality int) error {
	if format == "jpeg" {
		// Normalize 16-bit and grayscale alpha before using an RGB white background.
		if err := image.ToColorSpace(vips.InterpretationSRGB); err != nil {
			return err
		}
	}
	// govips treats zero as unspecified; preserve explicit minimum quality through libvips.
	if quality == 0 && (format == "webp" || format == "jpeg" || format == "avif") {
		intermediate := path + ".quality.png"
		defer os.Remove(intermediate)
		if err := save(image, intermediate, "png", 80); err != nil {
			return err
		}
		input, output, kind := C.CString(intermediate), C.CString(path), C.CString(format)
		defer C.free(unsafe.Pointer(input))
		defer C.free(unsafe.Pointer(output))
		defer C.free(unsafe.Pointer(kind))
		if C.worker_minimum_quality(input, output, kind) == 0 {
			return errors.New("Minimum-quality encoding failed")
		}
		file, err := os.OpenFile(path, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		err = file.Sync()
		closed := file.Close()
		if err == nil {
			err = closed
		}
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	switch format {
	case "webp":
		p := vips.NewWebpExportParams()
		p.Quality = quality
		if image.HasICCProfile() {
			p.IccProfile = "srgb"
		}
		err = image.SaveToWriterWebp(file, p)
	case "png":
		err = image.SaveToWriterPng(file, vips.NewPngExportParams())
	case "jpeg":
		if image.HasAlpha() {
			if err = image.Flatten(&vips.Color{R: 255, G: 255, B: 255}); err != nil {
				file.Close()
				return err
			}
		}
		p := vips.NewJpegExportParams()
		p.Quality = quality
		err = image.SaveToWriterJpeg(file, p)
	case "avif":
		p := vips.NewAvifExportParams()
		p.Quality = quality
		var data []byte
		data, _, err = image.ExportAvif(p)
		if err == nil {
			_, err = file.Write(data)
		}
	default:
		err = errors.New("Invalid static output")
	}
	if err == nil {
		err = file.Sync()
	}
	closed := file.Close()
	if err == nil {
		err = closed
	}
	return err
}
func boolInt(value bool) int {
	if value {
		return 1
	}
	return 0
}
