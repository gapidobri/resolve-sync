// The GUI. All the work is done by the core packages; this file and app.go
// only connect them to a window.
package main

import (
	"embed"
	"log"

	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
)

//go:embed all:frontend/dist
var assets embed.FS

func main() {
	svc := NewApp()
	app := application.New(application.Options{
		Name:     "Resolve Sync",
		Services: []application.Service{application.NewService(svc)},
		Assets:   application.AssetOptions{Handler: application.AssetFileServerFS(assets)},
		Mac:      application.MacOptions{ApplicationShouldTerminateAfterLastWindowClosed: true},
	})
	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:            "Resolve Sync",
		Width:            860,
		Height:           760,
		MinWidth:         640,
		MinHeight:        480,
		URL:              "/",
		BackgroundColour: application.NewRGB(22, 22, 24),
		EnableFileDrop:   true,
		Mac: application.MacWindow{
			TitleBar:                application.MacTitleBarHiddenInset,
			InvisibleTitleBarHeight: 38,
		},
	})
	svc.app, svc.win = app, win
	win.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		app.Event.Emit("opened", svc.Open(e.Context().DroppedFiles()))
	})
	if err := app.Run(); err != nil {
		log.Fatal(err)
	}
}
