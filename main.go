package main

import (
	"log"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"starbound-modmanager/internal/app"
	"starbound-modmanager/internal/i18n"
)

func main() {
	manager, err := app.NewManager()
	if err != nil {
		log.Fatal(err)
	}
	view := newManagerWindow(manager)
	appName := i18n.Translate(view.locale(), i18n.KeyAppTitle)
	mygo.App.SetName(appName)

	mygo.App.WhenReady(func() {
		view.window = mygo.NewWindow(mygo.WindowOptions{
			Title:     appName,
			Width:     1135,
			Height:    685,
			MinWidth:  850,
			MinHeight: 500,
			Content:   ui.View(view.view),
		})
		if err := view.window.SetIcon(applicationIcon); err != nil {
			log.Print(i18n.Translate(view.locale(), i18n.KeyLogIconFailure, err))
		}
		root := view.state.Root
		if root == "" {
			root = view.state.GameDirectory
		}
		if root != "" {
			view.scan(root)
		}
	})

	if err := mygo.App.Run(); err != nil {
		log.Fatal(err)
	}
}
