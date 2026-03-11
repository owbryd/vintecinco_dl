// Package main implements vintecinco_dl, a Wails v2 desktop application for
// downloading courses from multiple online platforms (Kiwify, Gumroad, Hotmart,
// Teachable, Kajabi). The Go backend handles authentication, course listing,
// and downloading (videos via yt-dlp, files via HTTP). The frontend is an
// embedded HTML/JS/CSS app served by the Wails asset server.
//
// Build tags:
//   - Windows: -tags "desktop production"
//   - Linux:   -tags "webkit2_41 desktop production"
package main

import (
	"embed"
	"fmt"
	"os"

	"github.com/wailsapp/wails/v2"
	"github.com/wailsapp/wails/v2/pkg/options"
	"github.com/wailsapp/wails/v2/pkg/options/assetserver"
)

// assets embeds the entire frontend directory (HTML/JS/CSS) into the binary
// so the app runs without external files.
//
//go:embed all:frontend
var assets embed.FS

func main() {
	app := NewApp()

	err := wails.Run(&options.App{
		Title:            "vintecinco_dl",
		Width:            900,
		Height:           650,
		MinWidth:         600,
		MinHeight:        400,
		BackgroundColour: &options.RGBA{R: 8, G: 8, B: 12, A: 255},
		AssetServer: &assetserver.Options{
			Assets: assets,
		},
		OnStartup: app.startup,
		Bind: []interface{}{
			app,
		},
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	}
}
