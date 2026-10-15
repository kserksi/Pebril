package nativeinstall

// Only reviewed pinned packages enter the catalog; runtime settings cannot replace sources or checksums.
type Artifact struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	SHA256        string            `json:"sha256"`
	Members       map[string]string `json:"members"`
	LibrarySHA256 string            `json:"library_sha256"`
	NoticesPrefix string            `json:"notices_prefix,omitempty"`
}

var Vips = Artifact{
	ID:            "libvips-8.18.5-linux-x64",
	URL:           "https://github.com/kleisauke/libvips-packaging/releases/download/v8.18.5/libvips-8.18.5-linux-x64.tar.gz",
	SHA256:        "2fc01c30f500db10e85510b77758d412b423ac5d7c9271d2d3539df7d8d074ee",
	LibrarySHA256: "2a3b36b1f3e742f204510dc366b183f56723580cb8b490911840c7b1d1f518a1",
	Members:       map[string]string{"lib/libvips.so.42": "libvips.so.42", "versions.json": "versions.json", "THIRD-PARTY-NOTICES.md": "THIRD-PARTY-NOTICES.md"},
}

// PebrilVips is the owner-approved runtime repack with its published source companion.
var PebrilVips = Artifact{
	NoticesPrefix: "libvips-8.16.1-linux-x64-glibc-r2/licenses/",
	ID:            "libvips-8.16.1-linux-x64-glibc-r2",
	URL:           "https://github.com/kserksi/Pebril/releases/download/native-libvips-8.16.1-r2/libvips-8.16.1-linux-x64-glibc-r2-runtime.tar.gz",
	SHA256:        "d28707dcdc1630e9d24d8d969fc558d465d34f0b22c1610ce733f146060b41fc",
	LibrarySHA256: "65e2e1b0eeec6fd120eb9a3fcdf71c7cbe45742c3fb2de050fc73f864ee84e81",
	Members: map[string]string{
		"libvips-8.16.1-linux-x64-glibc-r2/DISTRIBUTION-NOTICE.txt": "DISTRIBUTION-NOTICE.txt",
		"libvips-8.16.1-linux-x64-glibc-r2/PACKAGING-LICENSE.txt":   "PACKAGING-LICENSE.txt",
		"libvips-8.16.1-linux-x64-glibc-r2/README.txt":              "README.txt",
		"libvips-8.16.1-linux-x64-glibc-r2/THIRD-PARTY-NOTICES.md":  "THIRD-PARTY-NOTICES.md",
		"libvips-8.16.1-linux-x64-glibc-r2/files.sha256":            "files.sha256",
		"libvips-8.16.1-linux-x64-glibc-r2/lib/libvips.so.42":       "libvips.so.42",
		"libvips-8.16.1-linux-x64-glibc-r2/licenses/MPL-2.0.txt":    "licenses/MPL-2.0.txt",
		"libvips-8.16.1-linux-x64-glibc-r2/runtime-manifest.json":   "runtime-manifest.json",
		"libvips-8.16.1-linux-x64-glibc-r2/versions.json":           "versions.json",
	},
}

func Source(name string) (Artifact, bool) {
	switch name {
	case "upstream":
		return Vips, true
	case "pebril":
		return PebrilVips, true
	default:
		return Artifact{}, false
	}
}

func Catalog() []Artifact { return []Artifact{Vips, PebrilVips} }
