// Package webui embeds the dashboard's static assets.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed index.html ios-screen.html grid.html runs.html builds.html setup.html terminal.html files.html logcat.html apps.html uidump.html tests.html vscode-setup.html agents.html app.css app.js vendor
var files embed.FS

// FS returns the embedded dashboard file system (rooted at the asset dir).
func FS() fs.FS { return files }

// File returns one embedded asset's bytes.
func File(name string) ([]byte, error) { return files.ReadFile(name) }
