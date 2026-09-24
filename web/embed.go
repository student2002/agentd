// Package web embeds the static frontend assets under web/, served by
// agentd's local control API.
package web

import (
	"embed"
	"io/fs"
)

//go:embed control
var controlFiles embed.FS

// ControlIndexHTML returns the control console entry document.
func ControlIndexHTML() []byte {
	b, err := controlFiles.ReadFile("control/index.html")
	if err != nil {
		// The file is embedded at build time, so reading it cannot fail.
		panic(err)
	}
	return b
}

// ControlAssets returns the console's static assets (CSS/JS), rooted at the
// assets directory.
func ControlAssets() fs.FS {
	sub, err := fs.Sub(controlFiles, "control/assets")
	if err != nil {
		panic(err)
	}
	return sub
}
