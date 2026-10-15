package imageproc

import "github.com/kserksi/Pebril/internal/config"

// The parent controls every path; workers receive no database or account credentials.
type Request struct {
	Input        string         `json:"input"`
	Directory    string         `json:"directory"`
	Rust         string         `json:"rust"`
	SourceFormat string         `json:"source_format"`
	Frames       int            `json:"frames"`
	Policy       config.Runtime `json:"policy"`
}
type Result struct {
	Format  string `json:"format"`
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	Frames  int    `json:"frames"`
	Cover   bool   `json:"cover"`
	Error   string `json:"error,omitempty"`
	Version string `json:"version,omitempty"`
	Library string `json:"library,omitempty"`
}
