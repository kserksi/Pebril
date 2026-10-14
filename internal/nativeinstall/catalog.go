package nativeinstall

// Only reviewed pinned packages enter the catalog; runtime settings cannot replace sources or checksums.
type Artifact struct {
	ID            string            `json:"id"`
	URL           string            `json:"url"`
	SHA256        string            `json:"sha256"`
	Members       map[string]string `json:"members"`
	LibrarySHA256 string            `json:"library_sha256"`
}

var Vips = Artifact{
	ID:            "libvips-8.18.5-linux-x64",
	URL:           "https://github.com/kleisauke/libvips-packaging/releases/download/v8.18.5/libvips-8.18.5-linux-x64.tar.gz",
	SHA256:        "2fc01c30f500db10e85510b77758d412b423ac5d7c9271d2d3539df7d8d074ee",
	LibrarySHA256: "2a3b36b1f3e742f204510dc366b183f56723580cb8b490911840c7b1d1f518a1",
	Members:       map[string]string{"lib/libvips.so.42": "libvips.so.42", "versions.json": "versions.json", "THIRD-PARTY-NOTICES.md": "THIRD-PARTY-NOTICES.md"},
}
