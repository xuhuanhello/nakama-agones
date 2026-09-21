package console

import (
	"embed"
	"io/fs"
)

//go:embed web/*
var webAssets embed.FS

// WebFiles returns the administration console assets with index.html at the root.
func WebFiles() fs.FS {
	files, err := fs.Sub(webAssets, "web")
	if err != nil {
		panic(err) // The embedded web directory is fixed at compile time.
	}
	return files
}
