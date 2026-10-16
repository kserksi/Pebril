package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/joho/godotenv"
)

type Deployment struct {
	Root       string `json:"-"`
	Listen     string `json:"listen"`
	DataDir    string `json:"-"`
	PublicDir  string `json:"-"`
	PrivateDir string `json:"-"`
	DBDir      string `json:"-"`
	NativeDir  string `json:"-"`
	AVIF       bool   `json:"avif"`
	Restricted bool   `json:"-"`
}

// Expose only confirmed phase-A fields; phase-B defaults are enforced without configuration fields.
type Runtime struct {
	KeepOriginal          bool            `json:"keep_original"`
	AllowOriginalDownload bool            `json:"allow_original_download"`
	StaticFormat          string          `json:"static_format"`
	OutputWidth           int             `json:"output_width"`
	OutputHeight          int             `json:"output_height"`
	StaticQuality         int             `json:"static_quality"`
	AnimatedFormat        string          `json:"animated_format"`
	PreserveAnimation     bool            `json:"preserve_animation"`
	AnimatedQuality       int             `json:"animated_quality"`
	CoverFrame            int             `json:"cover_frame"`
	PreserveOrientation   bool            `json:"preserve_orientation"`
	PreserveColor         bool            `json:"preserve_color"`
	StaticMaxPixels       int64           `json:"static_max_pixels"`
	StaticMaxBytes        int64           `json:"static_max_bytes"`
	AnimatedMaxPixels     int64           `json:"animated_max_pixels"`
	AnimatedMaxBytes      int64           `json:"animated_max_bytes"`
	UploadFormats         map[string]bool `json:"upload_formats"`
	ConversionConcurrency int             `json:"conversion_concurrency"`
	Scheduling            string          `json:"scheduling"`
	Priority              string          `json:"priority"`
	ResumeTasks           bool            `json:"resume_tasks"`
	OriginalFallback      bool            `json:"original_fallback"`
	AutomaticRetry        bool            `json:"automatic_retry"`
	PublicCDNSeconds      int64           `json:"public_cdn_seconds"`
	PublicBrowserSeconds  int64           `json:"public_browser_seconds"`
	FallbackCDNSeconds    int64           `json:"fallback_cdn_seconds"`
	PrivateKeyDefaultMS   int64           `json:"private_key_default_ms"`
}
type Snapshot struct {
	Revision uint64  `json:"revision"`
	Policy   Runtime `json:"policy"`
}
type Manager struct {
	path       string
	deployment Deployment
	current    atomic.Pointer[Snapshot]
	watcher    *fsnotify.Watcher
	mu         sync.Mutex
}

func Defaults() Runtime {
	return Runtime{KeepOriginal: true, AllowOriginalDownload: true, StaticFormat: "webp", StaticQuality: 80,
		AnimatedFormat: "webp", PreserveAnimation: true, AnimatedQuality: 80, PreserveOrientation: true, PreserveColor: true,
		StaticMaxPixels: 50_000_000, StaticMaxBytes: 30 * 1024 * 1024, AnimatedMaxPixels: 50_000_000, AnimatedMaxBytes: 15 * 1024 * 1024,
		UploadFormats:         map[string]bool{"png": true, "jpeg": true, "gif": true, "webp": true, "svg": true, "avif": false},
		ConversionConcurrency: 10, Scheduling: "round_robin", Priority: "new_first", ResumeTasks: true, OriginalFallback: true, AutomaticRetry: true,
		PublicCDNSeconds: 15 * 24 * 3600, PublicBrowserSeconds: 3 * 24 * 3600, FallbackCDNSeconds: 3600, PrivateKeyDefaultMS: 1_800_000}
}
func LoadDeployment(root string) (Deployment, error) {
	abs, err := filepath.Abs(root)
	if err != nil {
		return Deployment{}, err
	}
	env := map[string]string{}
	path := filepath.Join(abs, ".env")
	if _, err = os.Stat(path); err == nil {
		env, err = godotenv.Read(path)
		if err != nil {
			return Deployment{}, fmt.Errorf(".env: %w", err)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return Deployment{}, err
	}
	value := func(key, fallback string) string {
		if v, ok := os.LookupEnv(key); ok {
			return v
		}
		if v, ok := env[key]; ok {
			return v
		}
		return fallback
	}
	resolve := func(p string) string {
		if filepath.IsAbs(p) {
			return filepath.Clean(p)
		}
		return filepath.Join(abs, p)
	}
	d := Deployment{Root: abs, Listen: value("PEBRIL_LISTEN", "127.0.0.1:8080"), DataDir: resolve(value("PEBRIL_DATA_DIR", "data")), NativeDir: resolve(value("PEBRIL_NATIVE_DIR", "bin"))}
	_, port, err := net.SplitHostPort(d.Listen)
	if err != nil {
		return d, fmt.Errorf("PEBRIL_LISTEN: %w", err)
	}
	number, e := strconv.Atoi(port)
	if e != nil || number < 1 || number > 65535 {
		return d, errors.New("Listen port must be in 1..65535")
	}
	d.PublicDir = resolve(value("PEBRIL_PUBLIC_DIR", filepath.Join(d.DataDir, "image/public")))
	d.PrivateDir = resolve(value("PEBRIL_PRIVATE_DIR", filepath.Join(d.DataDir, "image/private")))
	d.DBDir = resolve(value("PEBRIL_DB_DIR", filepath.Join(d.DataDir, "db")))
	switch value("PEBRIL_AVIF", "false") {
	case "true":
		d.AVIF = true
	case "false":
	default:
		return d, errors.New("PEBRIL_AVIF must be true or false")
	}
	dirs := []string{d.PublicDir, d.PrivateDir, d.DBDir, d.NativeDir}
	for i, a := range dirs {
		for j, b := range dirs {
			if i == j {
				continue
			}
			rel, e := filepath.Rel(a, b)
			if e == nil && (rel == "." || (!filepath.IsAbs(rel) && rel != ".." && len(rel) > 0 && !bytes.HasPrefix([]byte(rel), []byte(".."+string(os.PathSeparator))))) {
				return d, errors.New("Public, private, database, and native directories must not overlap")
			}
		}
	}
	return d, nil
}
func (r Runtime) Validate(d Deployment) error {
	for _, n := range []int64{r.StaticMaxPixels, r.StaticMaxBytes, r.AnimatedMaxPixels, r.AnimatedMaxBytes} {
		if n <= 0 {
			return errors.New("Pixel and byte limits must be positive")
		}
	}
	if r.StaticQuality < 0 || r.StaticQuality > 100 || r.AnimatedQuality < 0 || r.AnimatedQuality > 100 {
		return errors.New("WebP quality must be in the encoder-supported range 0..100")
	}
	if r.OutputWidth < 0 || r.OutputHeight < 0 || (r.OutputWidth == 0) != (r.OutputHeight == 0) {
		return errors.New("Output width and height must both be zero (original dimensions) or positive")
	}
	if r.OutputWidth > 0 && (int64(r.OutputWidth) > r.StaticMaxPixels/int64(r.OutputHeight)) {
		return errors.New("Output pixels exceed the static limit")
	}
	if r.CoverFrame < 0 || r.ConversionConcurrency <= 0 || r.PublicCDNSeconds < 0 || r.PublicBrowserSeconds < 0 || r.FallbackCDNSeconds < 0 {
		return errors.New("Invalid frame, concurrency, or cache parameters")
	}
	if r.PrivateKeyDefaultMS <= 0 {
		return errors.New("Default private-key lifetime must be positive; the global range remains unconfirmed")
	}
	if r.Scheduling != "round_robin" || r.Priority != "new_first" {
		return errors.New("Only the confirmed round_robin/new_first policies are currently implemented")
	}
	if !r.PreserveAnimation {
		return errors.New("Disabling animation preservation is unconfirmed and not accepted in this version")
	}
	if r.StaticFormat != "webp" && r.StaticFormat != "png" && r.StaticFormat != "jpeg" && r.StaticFormat != "avif" {
		return errors.New("Invalid static output format")
	}
	if r.AnimatedFormat != "webp" && r.AnimatedFormat != "gif" {
		return errors.New("Invalid animated output format")
	}
	for k := range r.UploadFormats {
		switch k {
		case "png", "jpeg", "gif", "webp", "svg", "avif":
		default:
			return fmt.Errorf("Unknown upload format %s", k)
		}
	}
	if !d.AVIF && (r.UploadFormats["avif"] || r.StaticFormat == "avif") {
		return errors.New("AVIF is disabled in deployment; new AVIF input or output cannot be enabled")
	}
	return nil
}
func duplicateKeys(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	var walk func() error
	walk = func() error {
		token, err := dec.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			if token == nil {
				return errors.New("Configuration does not accept null")
			}
			return nil
		}
		switch delim {
		case '{':
			seen := map[string]bool{}
			for dec.More() {
				k, e := dec.Token()
				if e != nil {
					return e
				}
				s, ok := k.(string)
				if !ok {
					return errors.New("Invalid JSON key")
				}
				if seen[s] {
					return fmt.Errorf("Duplicate JSON key %s", s)
				}
				seen[s] = true
				if e = walk(); e != nil {
					return e
				}
			}
		case '[':
			for dec.More() {
				if e := walk(); e != nil {
					return e
				}
			}
		default:
			return errors.New("Invalid JSON structure")
		}
		_, err = dec.Token()
		return err
	}
	if err := walk(); err != nil {
		return err
	}
	if _, err := dec.Token(); err != io.EOF {
		return errors.New("Unexpected content after JSON")
	}
	return nil
}
func Read(path string, d Deployment) (Runtime, error) {
	f, err := os.Open(path)
	if err != nil {
		return Runtime{}, err
	}
	defer f.Close()
	data, err := io.ReadAll(io.LimitReader(f, 1024*1024+1))
	if err != nil {
		return Runtime{}, err
	}
	if len(data) > 1024*1024 {
		return Runtime{}, errors.New("Configuration file exceeds 1MiB")
	}
	if err = duplicateKeys(data); err != nil {
		return Runtime{}, err
	}
	if len(bytes.TrimSpace(data)) == 0 || bytes.TrimSpace(data)[0] != '{' {
		return Runtime{}, errors.New("Configuration root must be a JSON object")
	}
	r := Defaults()
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(&r); err != nil {
		return r, err
	}
	if err = r.Validate(d); err != nil {
		return r, err
	}
	return r, nil
}
func New(d Deployment, create bool) (*Manager, error) {
	path := filepath.Join(d.Root, "config.json")
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) && create {
		defaults := Defaults()
		defaults.UploadFormats["avif"] = d.AVIF
		data, e := json.MarshalIndent(defaults, "", "  ")
		if e != nil {
			return nil, e
		}
		f, e := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if e == nil {
			_, e = f.Write(append(data, '\n'))
			if e == nil {
				e = f.Sync()
			}
			closeErr := f.Close()
			if e == nil {
				e = closeErr
			}
			if e != nil {
				return nil, e
			}
		} else if !errors.Is(e, os.ErrExist) {
			return nil, e
		}
	}
	r, err := Read(path, d)
	if err != nil {
		return nil, fmt.Errorf("config.json: %w", err)
	}
	m := &Manager{path: path, deployment: d}
	m.current.Store(&Snapshot{Revision: 1, Policy: r})
	return m, nil
}

// Return an independent copy so tasks can persist the policy captured at acceptance.
func (m *Manager) Snapshot() Snapshot {
	current := m.current.Load()
	s := *current
	s.Policy.UploadFormats = map[string]bool{}
	for k, v := range current.Policy.UploadFormats {
		s.Policy.UploadFormats[k] = v
	}
	if m.deployment.Restricted {
		s.Policy.ConversionConcurrency = 1
		s.Policy.UploadFormats["avif"] = false
	}
	return s
}
func (m *Manager) Reload() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, e := Read(m.path, m.deployment)
	if e != nil {
		return e
	}
	old := m.current.Load()
	m.current.Store(&Snapshot{Revision: old.Revision + 1, Policy: r})
	return nil
}
func (m *Manager) Watch(onResult func(error)) error {
	w, e := fsnotify.NewWatcher()
	if e != nil {
		return e
	}
	if e = w.Add(filepath.Dir(m.path)); e != nil {
		w.Close()
		return e
	}
	m.watcher = w
	go func() {
		var timer *time.Timer
		defer func() {
			if timer != nil {
				timer.Stop()
			}
		}()
		var tick <-chan time.Time
		for {
			select {
			case event, ok := <-w.Events:
				if !ok {
					if timer != nil {
						timer.Stop()
					}
					return
				}
				if filepath.Clean(event.Name) == m.path {
					if timer != nil {
						timer.Stop()
					}
					timer = time.NewTimer(100 * time.Millisecond)
					tick = timer.C
				}
			case err, ok := <-w.Errors:
				if !ok {
					return
				}
				onResult(err)
			case <-tick:
				tick = nil
				onResult(m.Reload())
			}
		}
	}()
	return nil
}
func (m *Manager) Close() error {
	if m.watcher != nil {
		return m.watcher.Close()
	}
	return nil
}
