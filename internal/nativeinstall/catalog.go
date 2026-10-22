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
	ID:            "libvips-8.18.7-linux-x64",
	URL:           "https://github.com/kleisauke/libvips-packaging/releases/download/v8.18.7/libvips-8.18.7-linux-x64.tar.gz",
	SHA256:        "e10ffbd665b97fa76575cebe96af440d4d033d0295c4c00a0e27dd841996ef50",
	LibrarySHA256: "e42572c0021460ae6667ea5b67f1b1b8094575e8cccc7b317fa39dd6f828535f",
	Members:       map[string]string{"lib/libvips.so.42": "libvips.so.42", "versions.json": "versions.json", "THIRD-PARTY-NOTICES.md": "THIRD-PARTY-NOTICES.md"},
}

func Source(name string) (Artifact, bool) {
	switch name {
	case "upstream":
		return Vips, true
	default:
		return Artifact{}, false
	}
}

func Catalog() []Artifact { return []Artifact{Vips} }
