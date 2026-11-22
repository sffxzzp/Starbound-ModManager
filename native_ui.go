package main

import (
	"context"
	_ "embed"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/egoist/mygo"
	"github.com/egoist/mygo/ui"

	"starbound-modmanager/internal/app"
	"starbound-modmanager/internal/i18n"
	"starbound-modmanager/internal/mods"
)

//go:embed resources/icon.png
var applicationIcon []byte

type managerWindow struct {
	manager            *app.Manager
	window             *mygo.Window
	state              app.AppState
	appliedOrder       []string
	orderDirty         bool
	priorityNeedsApply bool
	modsList           ui.ListState
	conflictsList      ui.ListState
	mergeMetadataOpen  bool
	mergeMetadata      app.MergedModMetadata
	metadataProblem    error
	busy               bool
	status             i18n.Message
	problem            error
	progress           float64
	progressStatus     i18n.Message
	progressPath       string
}

func newManagerWindow(manager *app.Manager) *managerWindow {
	return &managerWindow{
		manager: manager,
		state:   manager.GetState(),
		status:  i18n.Msg(i18n.KeyStatusReady),
	}
}

func (w *managerWindow) locale() i18n.Locale {
	return i18n.Normalize(w.state.Language)
}

func (w *managerWindow) tr(key i18n.Key, args ...any) string {
	return i18n.Translate(w.locale(), key, args...)
}

func (w *managerWindow) message(message i18n.Message) string {
	return i18n.Text(w.locale(), message)
}

func (w *managerWindow) errorText(err error) string {
	return i18n.ErrorText(w.locale(), err)
}

func (w *managerWindow) toggleLanguage() {
	next := i18n.English
	if w.locale() == i18n.English {
		next = i18n.Chinese
	}
	if err := w.manager.SetLanguage(next); err != nil {
		w.problem = err
		return
	}
	w.state = w.manager.GetState()
	appName := w.tr(i18n.KeyAppTitle)
	if w.window != nil {
		w.window.SetTitle(appName)
	}
	w.problem = nil
	w.status = i18n.Msg(i18n.KeyStatusLanguageChanged)
}

func (w *managerWindow) view(c *ui.Context) {
	t := c.Theme()
	w.modsList.Key = func(i int) any { return w.state.Mods[i].ID }
	w.modsList.Label = func(i int) string { return w.state.Mods[i].Name }
	w.conflictsList.Key = func(i int) any { return w.state.Conflicts[i].Path }
	w.conflictsList.Label = func(i int) string { return w.state.Conflicts[i].Path }

	ui.Column(c).Fill().Padding(10).Gap(6).Children(func() {
		w.directorySection(c, t)
		w.stats(c, t)

		ui.Row(c).FillWidth().Grow(1).Gap(10).AlignItems(ui.Stretch).Children(func() {
			w.modSection(c, t)
			if w.state.ConflictCount > 0 {
				w.conflictSection(c, t)
			}
		})

		if len(w.state.Warnings) > 0 {
			w.warningSection(c, t)
		}
		w.footer(c, t)
	})
	w.mergeMetadataDialog(c, t)
}

func (w *managerWindow) directorySection(c *ui.Context, t *ui.Theme) {
	gameDirectory := w.state.GameDirectory
	if gameDirectory == "" {
		gameDirectory = w.tr(i18n.KeyDirectoryNotSet)
	}
	ui.Column(c).FillWidth().Gap(5).Padding(7, 10).Radius(9).
		Background(t.Surface).Border(1, t.Border).Children(func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
			ui.Text(c, w.tr(i18n.KeyDirectoryGame)).Width(54).FontSize(12).Bold()
			ui.Text(c, gameDirectory).Grow(1).SingleLine().FontSize(12).TextColor(t.TextMuted)
			if ui.Button(c, w.tr(i18n.KeyActionChange)).Disabled(w.busy).Clicked() {
				w.chooseGameDirectory()
			}
			if ui.Button(c, w.tr(i18n.KeyLanguageSwitch)).Clicked() {
				w.toggleLanguage()
			}
		})
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
			ui.Text(c, w.tr(i18n.KeyDirectorySource)).Width(54).FontSize(12).Bold()
			ui.Text(c, w.modSourceLabel()).Grow(1).SingleLine().FontSize(12).TextColor(t.TextMuted)
			if ui.Button(c, w.tr(i18n.KeyActionScan)).Disabled(w.busy || w.state.Root == "").Clicked() {
				w.scan(w.state.Root)
			}
			if ui.Button(c, w.tr(i18n.KeyActionChange)).Disabled(w.busy).Clicked() {
				w.chooseRoot()
			}
		})
	})
}

func (w *managerWindow) modSourceLabel() string {
	root := w.state.Root
	if root == "" {
		return w.tr(i18n.KeyDirectoryNotSelected)
	}
	if sameDirectoryPath(root, w.state.GameDirectory) {
		return w.tr(i18n.KeyDirectoryGameSource)
	}
	if w.state.GameDirectory != "" && sameDirectoryPath(root, filepath.Join(w.state.GameDirectory, "mods")) {
		return w.tr(i18n.KeyDirectoryModsSource)
	}
	return root
}

func sameDirectoryPath(left, right string) bool {
	return left != "" && right != "" && strings.EqualFold(filepath.Clean(left), filepath.Clean(right))
}

func (w *managerWindow) stats(c *ui.Context, t *ui.Theme) {
	ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(14).Padding(6, 12).Radius(8).
		Background(t.Surface).Border(1, t.Border).Children(func() {
		selectableCount := 0
		for _, mod := range w.state.Mods {
			if mod.Available {
				selectableCount++
			}
		}
		w.metric(c, t, w.tr(i18n.KeyStatsSelected), fmt.Sprintf("%d / %d", w.state.EnabledCount, selectableCount))
		w.metric(c, t, w.tr(i18n.KeyStatsAssets), fmt.Sprintf("%d", w.state.AssetCount))
		w.metric(c, t, w.tr(i18n.KeyStatsConflicts), fmt.Sprintf("%d", w.state.ConflictCount))
		w.metric(c, t, w.tr(i18n.KeyStatsPatchTargetsLabel), w.tr(i18n.KeyStatsPatchTargets, w.state.PatchTargetCount, w.state.PatchFileCount))
	})
}

func (w *managerWindow) metric(c *ui.Context, t *ui.Theme, label, value string) {
	ui.Row(c).Grow(1).AlignItems(ui.Center).Gap(6).Children(func() {
		ui.Text(c, label).FontSize(12).TextColor(t.TextMuted)
		if value != "" {
			ui.Text(c, value).FontSize(15).Bold()
		}
	})
}

func (w *managerWindow) modSection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Grow(1).MinWidth(520).Padding(10).Gap(6).Radius(9).
		Background(t.Surface).Border(1, t.Border).Children(func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(8).Children(func() {
			ui.Text(c, w.tr(i18n.KeyModsHeading)).Grow(1).FontSize(17).Bold()
			ui.Text(c, w.tr(i18n.KeyModsCount, len(w.state.Mods))).FontSize(12).TextColor(t.TextMuted)
			availableCount := 0
			for _, mod := range w.state.Mods {
				if mod.Available {
					availableCount++
				}
			}
			if ui.Button(c, w.tr(i18n.KeyActionSelectAll)).Disabled(w.busy || availableCount == 0).Clicked() {
				w.setAllIncluded(true)
			}
			if ui.Button(c, w.tr(i18n.KeyActionClearSelection)).Disabled(w.busy || w.state.EnabledCount == 0).Clicked() {
				w.setAllIncluded(false)
			}
		})
		ui.List(c, &w.modsList, len(w.state.Mods), func(i int) {
			w.modRow(c, t, i, i+1)
		}).Grow(1).Children(func() {
			if len(w.state.Mods) == 0 {
				message := w.tr(i18n.KeyModsEmptySource)
				if w.state.Root == "" {
					message = w.tr(i18n.KeyModsEmptyGame)
				}
				ui.Text(c, message).Padding(18).TextColor(t.TextMuted)
			}
		})
	})
}

func (w *managerWindow) modRow(c *ui.Context, t *ui.Theme, i, displayNumber int) {
	mod := w.state.Mods[i]
	ui.Row(c.Key(mod.ID)).FillWidth().AlignItems(ui.Center).Gap(9).Padding(7, 4).Children(func() {
		ui.Textf(c, "%02d", displayNumber).Width(30).FontSize(14).TextColor(t.TextMuted)

		included := mod.Enabled
		if ui.Checkbox(c, &included, w.tr(i18n.KeyModInclude)).Disabled(w.busy || !mod.Available).Changed() {
			w.setIncluded(mod.ID, included)
		}

		ui.Column(c).Grow(1).Gap(3).Children(func() {
			ui.Text(c, mod.Name).SingleLine().FontSize(14).Bold()
			version := w.tr(i18n.KeyModVersionUnknown)
			if mod.Version != "" {
				version = mod.Version
			}
			source := w.tr(i18n.KeyModSourceDirectory)
			if mod.SourceType == "pak" {
				source = w.tr(i18n.KeyModSourcePak)
			}
			origin := w.tr(i18n.KeyModSourceSelected)
			switch mod.SourceLocation {
			case mods.SourceLocationWorkshop:
				origin = w.tr(i18n.KeyModSourceWorkshop)
			case mods.SourceLocationLocal:
				origin = w.tr(i18n.KeyModSourceLocal)
			}
			priority := w.tr(i18n.KeyModPriorityDefault)
			if mod.PriorityKnown {
				priority = fmt.Sprintf("%.6g", mod.Priority)
			}
			ui.Text(c, w.tr(i18n.KeyModDetail, origin, source, version, priority, mod.AssetCount)).
				SingleLine().FontSize(12).TextColor(t.TextMuted)
			if !mod.Available {
				ui.Text(c, w.errorText(mod.Error)).FontSize(12).TextColor(t.Danger)
			}
		})

		if mod.ConflictCount > 0 {
			ui.Text(c, w.tr(i18n.KeyModConflictCount, mod.ConflictCount)).FontSize(12).TextColor(t.Warning)
		} else if mod.PatchCount > 0 {
			ui.Text(c, w.tr(i18n.KeyModPatchCount, mod.PatchCount)).FontSize(12).TextColor(t.Accent)
		}
		if mod.OrderLocked {
			ui.Text(c, w.tr(i18n.KeyModLocked)).FontSize(11).TextColor(t.Warning)
		}
		gameAction := w.tr(i18n.KeyActionDisable)
		if !mod.GameEnabled {
			gameAction = w.tr(i18n.KeyActionEnable)
		}
		if ui.Button(c, gameAction).Disabled(w.busy).Clicked() {
			w.setGameEnabled(mod.ID, !mod.GameEnabled)
		}

		if ui.Button(c, "↑").Width(32).Label(w.tr(i18n.KeyActionMoveEarlier)).Disabled(w.busy || mod.OrderLocked || i == 0 || w.state.Mods[i-1].OrderLocked).Clicked() {
			w.moveMod(i, -1)
		}
		if ui.Button(c, "↓").Width(32).Label(w.tr(i18n.KeyActionMoveLater)).Disabled(w.busy || mod.OrderLocked || i == len(w.state.Mods)-1 || w.state.Mods[i+1].OrderLocked).Clicked() {
			w.moveMod(i, 1)
		}
	})
}

func (w *managerWindow) conflictSection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).Width(240).Padding(10).Gap(8).Radius(9).
		Background(t.Surface).Border(1, t.Border).Children(func() {
		ui.Row(c).FillWidth().AlignItems(ui.Center).Children(func() {
			ui.Text(c, w.tr(i18n.KeyConflictHeading)).FontSize(17).Bold().Grow(1)
			ui.Textf(c, "%d", w.state.ConflictCount).FontSize(14).TextColor(t.Warning)
		})
		ui.Text(c, w.tr(i18n.KeyConflictDescription)).FontSize(12).TextColor(t.TextMuted)

		if w.state.EnabledCount == 0 {
			ui.Text(c, w.tr(i18n.KeyConflictEmpty)).Padding(10).TextColor(t.TextMuted)
		} else if w.state.ConflictCount == 0 {
			ui.Text(c, w.tr(i18n.KeyConflictNone)).Padding(10).TextColor(t.TextMuted)
		} else {
			ui.List(c, &w.conflictsList, len(w.state.Conflicts), func(i int) {
				conflict := w.state.Conflicts[i]
				ui.Column(c.Key(conflict.Path)).FillWidth().Gap(4).Padding(7, 4).Children(func() {
					ui.Text(c, conflict.Path).SingleLine().FontSize(12)
					ui.Text(c, w.tr(i18n.KeyConflictWinner, conflict.Winner)).SingleLine().FontSize(12).TextColor(t.Accent)
					ui.Text(c, strings.Join(conflict.Mods, w.tr(i18n.KeyListSeparator))).SingleLine().FontSize(12).TextColor(t.TextMuted)
				})
			}).Grow(1)
			if w.state.ConflictCount > len(w.state.Conflicts) {
				ui.Text(c, w.tr(i18n.KeyConflictMore, w.state.ConflictCount-len(w.state.Conflicts))).
					FontSize(12).TextColor(t.TextMuted)
			}
		}
	})
}

func (w *managerWindow) warningSection(c *ui.Context, t *ui.Theme) {
	ui.Column(c).FillWidth().Padding(10).Gap(5).Radius(8).
		Background(t.Surface).Border(1, t.Border).Children(func() {
		ui.Text(c, w.tr(i18n.KeyWarningsHeading, len(w.state.Warnings))).FontSize(13).Bold()
		ui.Scroll(c).Height(48).Gap(4).Children(func() {
			for _, warning := range w.state.Warnings {
				ui.Text(c, "• "+w.errorText(warning)).FontSize(12).TextColor(t.TextMuted)
			}
		})
	})
}

func (w *managerWindow) footer(c *ui.Context, t *ui.Theme) {
	ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(12).Padding(8, 12).Radius(9).
		Background(t.Surface).Border(1, t.Border).Children(func() {
		ui.Column(c).Grow(1).Gap(4).Children(func() {
			statusColor := t.TextMuted
			if w.problem != nil {
				statusColor = t.Danger
			}
			ui.Text(c, w.message(w.status)).FontSize(13).TextColor(statusColor)
			notice := w.tr(i18n.KeySortHint)
			noticeColor := t.TextMuted
			if w.orderDirty {
				notice = w.tr(i18n.KeySortPending)
				noticeColor = t.Warning
			} else if w.priorityNeedsApply {
				notice = w.tr(i18n.KeySortNeedsApply)
				noticeColor = t.Warning
			}
			ui.Text(c, notice).FontSize(12).TextColor(noticeColor)
			if w.busy {
				progressLabel := w.message(w.progressStatus)
				if w.progressPath != "" {
					progressLabel = w.progressPath
				}
				ui.Progress(c, w.progress).Label(progressLabel)
			}
			if w.problem != nil {
				ui.Text(c, w.errorText(w.problem)).FontSize(12).TextColor(t.Danger)
			}
		})
		ui.Column(c).Width(520).Gap(5).Children(func() {
			ui.Row(c).FillWidth().AlignItems(ui.Center).Gap(10).Children(func() {
				if ui.Button(c, w.tr(i18n.KeyActionApplyOrder)).Disabled(w.busy || len(w.state.Mods) == 0).Clicked() {
					w.applyOrder()
				}
				ui.Divider(c)
				if ui.PrimaryButton(c, w.tr(i18n.KeyActionMergeInstall)).Disabled(w.busy || !w.state.CanMerge || w.orderDirty || w.priorityNeedsApply).Clicked() {
					w.openMergeMetadata()
				}
				ui.Divider(c)
				if ui.Button(c, w.tr(i18n.KeyActionLaunchDirect)).Disabled(w.busy || w.state.GameDirectory == "").Clicked() {
					w.launchGame(false)
				}
				if ui.Button(c, w.tr(i18n.KeyActionLaunchSteam)).Disabled(w.busy).Clicked() {
					w.launchGame(true)
				}
			})
			ui.Text(c, w.tr(i18n.KeyLaunchHint)).FillWidth().FontSize(12).TextColor(t.Warning)
		})
	})
}

func (w *managerWindow) mergeMetadataDialog(c *ui.Context, t *ui.Theme) {
	_, windowHeight := c.Size()
	dialogHeight := windowHeight - 96
	if dialogHeight > 640 {
		dialogHeight = 640
	}
	if dialogHeight < 180 {
		dialogHeight = 180
	}
	formHeight := dialogHeight - 112
	if formHeight < 80 {
		formHeight = 80
	}
	ui.Modal(c, &w.mergeMetadataOpen, func() {
		ui.Column(c).Width(640).Height(dialogHeight).Padding(14).Gap(8).Radius(10).
			Background(t.Surface).Border(1, t.Border).Children(func() {
			ui.Column(c).FillWidth().Gap(3).Shrink(0).Children(func() {
				ui.Text(c, w.tr(i18n.KeyMetadataTitle)).FontSize(20).Bold()
				ui.Text(c, w.tr(i18n.KeyMetadataIntro)).
					FontSize(13).TextColor(t.TextMuted)
			})

			ui.Scroll(c).FillWidth().Height(formHeight).MinHeight(0).Gap(9).Children(func() {
				ui.Column(c).FillWidth().Gap(4).Children(func() {
					ui.Text(c, w.tr(i18n.KeyMetadataOutputFile)).FontSize(13).Bold()
					ui.TextInput(c, &w.mergeMetadata.OutputFileName).
						Placeholder("Starbound-ModManager-Merged.pak").FillWidth()
					ui.Text(c, w.tr(i18n.KeyMetadataExtension)).FontSize(12).TextColor(t.TextMuted)
				})

				metadataField(c, w.tr(i18n.KeyMetadataName), w.tr(i18n.KeyMetadataNameHint), &w.mergeMetadata.Name, true)
				metadataField(c, w.tr(i18n.KeyMetadataFriendlyName), w.tr(i18n.KeyMetadataFriendlyHint), &w.mergeMetadata.FriendlyName, false)
				ui.Row(c).FillWidth().Gap(10).Children(func() {
					ui.Column(c).Grow(1).Gap(4).Children(func() {
						ui.Text(c, w.tr(i18n.KeyMetadataVersion)).FontSize(13).Bold()
						ui.TextInput(c, &w.mergeMetadata.Version).Placeholder("1.0.0").FillWidth()
					})
					ui.Column(c).Grow(1).Gap(4).Children(func() {
						ui.Text(c, w.tr(i18n.KeyMetadataAuthor)).FontSize(13).Bold()
						ui.TextInput(c, &w.mergeMetadata.Author).Placeholder(w.tr(i18n.KeyMetadataAuthorHint)).FillWidth()
					})
				})

				ui.Column(c).FillWidth().Gap(4).Children(func() {
					ui.Text(c, w.tr(i18n.KeyMetadataDescription)).FontSize(13).Bold()
					ui.TextArea(c, &w.mergeMetadata.Description).Lines(2, 4).FillWidth()
				})

				ui.Column(c).FillWidth().Gap(4).Children(func() {
					ui.Text(c, w.tr(i18n.KeyMetadataExtra)).FontSize(13).Bold()
					ui.Text(c, w.tr(i18n.KeyMetadataExample)).
						FontSize(12).TextColor(t.TextMuted)
					ui.TextArea(c, &w.mergeMetadata.AdditionalJSON).Lines(2, 5).FillWidth()
				})

				if w.metadataProblem != nil {
					ui.Text(c, w.errorText(w.metadataProblem)).FontSize(13).TextColor(t.Danger)
				}
			})

			ui.Row(c).FillWidth().Shrink(0).Justify(ui.End).Gap(8).Children(func() {
				if ui.Button(c, w.tr(i18n.KeyActionCancel)).Clicked() {
					w.mergeMetadataOpen = false
					w.metadataProblem = nil
				}
				if ui.PrimaryButton(c, w.tr(i18n.KeyActionGenerate)).Disabled(w.busy).Clicked() {
					w.confirmMergedMod()
				}
			})
		})
	})
}

func metadataField(c *ui.Context, label, placeholder string, value *string, autoFocus bool) {
	ui.Column(c).FillWidth().Gap(4).Children(func() {
		ui.Text(c, label).FontSize(13).Bold()
		field := ui.TextInput(c, value).Placeholder(placeholder).FillWidth()
		if autoFocus {
			field.AutoFocus()
		}
	})
}

func (w *managerWindow) openMergeMetadata() {
	if w.busy || !w.state.CanMerge || w.orderDirty || w.priorityNeedsApply || w.window == nil {
		return
	}
	w.mergeMetadata = app.DefaultMergedModMetadata(w.state.EnabledCount)
	w.mergeMetadata.FriendlyName = w.tr(i18n.KeyMetadataDefaultFriendly)
	w.mergeMetadata.Description = w.tr(i18n.KeyMetadataDefaultDescription, w.state.EnabledCount)
	if w.state.MergedPackFileName != "" {
		w.mergeMetadata.OutputFileName = w.state.MergedPackFileName
	}
	w.metadataProblem = nil
	w.mergeMetadataOpen = true
}

func (w *managerWindow) confirmMergedMod() {
	if w.busy || !w.mergeMetadataOpen {
		return
	}
	if err := w.mergeMetadata.Validate(); err != nil {
		w.metadataProblem = err
		return
	}
	w.mergeMetadataOpen = false
	w.metadataProblem = nil
	w.createMergedMod()
}

func (w *managerWindow) chooseGameDirectory() {
	if w.busy || w.window == nil {
		return
	}
	w.busy = true
	w.problem = nil
	w.status = i18n.Msg(i18n.KeyStatusWaitGame)
	win := w.window
	defaultPath := w.state.GameDirectory
	if defaultPath == "" {
		defaultPath = w.state.Root
	}
	dialogTitle := w.tr(i18n.KeyDialogGameDirectory)
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:      win,
			Title:       dialogTitle,
			DefaultPath: defaultPath,
			Directory:   true,
		})
		if win.IsDestroyed() {
			return
		}
		win.Update(func() {
			w.busy = false
			if err != nil {
				w.problem = err
				w.status = i18n.Msg(i18n.KeyStatusDialogError)
				return
			}
			if len(paths) == 0 {
				w.status = i18n.Msg(i18n.KeyStatusCancelled)
				return
			}
			next, setErr := w.manager.SetGameDirectory(paths[0])
			if setErr != nil {
				w.problem = setErr
				w.status = i18n.Msg(i18n.KeyStatusSetGameError)
				return
			}
			w.state = next
			w.scan(paths[0])
		})
	}()
}

func (w *managerWindow) chooseRoot() {
	if w.busy || w.window == nil {
		return
	}
	w.busy = true
	w.problem = nil
	w.status = i18n.Msg(i18n.KeyStatusWaitSource)
	w.progress = -1
	w.progressStatus = i18n.Msg(i18n.KeyProgressSelecting)
	w.progressPath = ""
	win := w.window
	defaultPath := w.state.Root
	dialogTitle := w.tr(i18n.KeyDialogModSource)
	go func() {
		paths, err := mygo.Dialog.Open(mygo.OpenDialogOptions{
			Parent:      win,
			Title:       dialogTitle,
			DefaultPath: defaultPath,
			Directory:   true,
		})
		if win.IsDestroyed() {
			return
		}
		win.Update(func() {
			w.busy = false
			if err != nil {
				w.problem = err
				w.status = i18n.Msg(i18n.KeyStatusDialogError)
				return
			}
			if len(paths) == 0 {
				w.status = i18n.Msg(i18n.KeyStatusCancelled)
				return
			}
			w.scan(paths[0])
		})
	}()
}

func (w *managerWindow) scan(root string) {
	if w.busy || root == "" || w.window == nil {
		return
	}
	w.busy = true
	w.problem = nil
	w.status = i18n.Msg(i18n.KeyStatusScanning)
	w.progress = -1
	w.progressStatus = i18n.Msg(i18n.KeyProgressScanning)
	w.progressPath = ""
	win := w.window
	go func() {
		next, err := w.manager.Scan(context.Background(), root)
		if win.IsDestroyed() {
			return
		}
		win.Update(func() {
			w.busy = false
			if err != nil {
				w.problem = err
				w.status = i18n.Msg(i18n.KeyStatusScanFailed)
				return
			}
			w.state = next
			w.appliedOrder = modOrderIDs(next.Mods)
			w.orderDirty = false
			w.priorityNeedsApply = next.PriorityNeedsApply
			w.status = i18n.Msg(i18n.KeyStatusScanComplete, len(next.Mods))
		})
	}()
}

func (w *managerWindow) setIncluded(id string, included bool) {
	if w.busy {
		return
	}
	next, err := w.manager.SetIncluded(id, included)
	if err != nil {
		w.problem = err
		w.status = i18n.Msg(i18n.KeyStatusSelectionError)
		return
	}
	w.problem = nil
	w.state = next
	w.orderDirty = !sameModOrder(w.appliedOrder, modOrderIDs(next.Mods))
	w.priorityNeedsApply = next.PriorityNeedsApply
	w.status = i18n.Msg(i18n.KeyStatusSelectionSet)
}

func (w *managerWindow) setAllIncluded(included bool) {
	if w.busy {
		return
	}
	next, skippedGroups, err := w.manager.SetAllIncluded(included)
	if err != nil {
		w.problem = err
		w.status = i18n.Msg(i18n.KeyStatusSelectionError)
		return
	}
	w.problem = nil
	w.state = next
	w.orderDirty = !sameModOrder(w.appliedOrder, modOrderIDs(next.Mods))
	w.priorityNeedsApply = next.PriorityNeedsApply
	if !included {
		w.status = i18n.Msg(i18n.KeyStatusSelectionClear)
	} else if skippedGroups > 0 {
		w.status = i18n.Msg(i18n.KeyStatusAllSkipped, skippedGroups)
	} else {
		w.status = i18n.Msg(i18n.KeyStatusAllSelected)
	}
}

func (w *managerWindow) setGameEnabled(id string, enabled bool) {
	if w.busy || w.window == nil {
		return
	}
	w.busy = true
	w.problem = nil
	w.status = i18n.Msg(i18n.KeyStatusModUpdating)
	win := w.window
	go func() {
		next, err := w.manager.SetGameEnabled(id, enabled)
		if win.IsDestroyed() {
			return
		}
		win.Update(func() {
			w.busy = false
			if err != nil {
				w.problem = err
				w.status = i18n.Msg(i18n.KeyStatusModChangeError)
				return
			}
			w.state = next
			w.orderDirty = !sameModOrder(w.appliedOrder, modOrderIDs(next.Mods))
			w.priorityNeedsApply = next.PriorityNeedsApply
			w.problem = nil
			if enabled {
				w.status = i18n.Msg(i18n.KeyStatusModEnabled)
			} else {
				w.status = i18n.Msg(i18n.KeyStatusModDisabled)
			}
		})
	}()
}

func (w *managerWindow) moveMod(index, delta int) {
	if w.busy || w.window == nil {
		return
	}
	ids := make([]string, len(w.state.Mods))
	for i, mod := range w.state.Mods {
		ids[i] = mod.ID
	}
	to := index + delta
	if index < 0 || index >= len(ids) || to < 0 || to >= len(ids) {
		return
	}
	if w.state.Mods[index].OrderLocked || w.state.Mods[to].OrderLocked {
		return
	}
	ids[index], ids[to] = ids[to], ids[index]
	w.busy = true
	w.problem = nil
	w.status = i18n.Msg(i18n.KeyStatusOrderPreview)
	win := w.window
	go func() {
		next, err := w.manager.PreviewOrder(ids)
		if win.IsDestroyed() {
			return
		}
		win.Update(func() {
			w.busy = false
			if err != nil {
				w.state = next
				w.problem = err
				w.status = i18n.Msg(i18n.KeyStatusOrderError)
				return
			}
			w.problem = nil
			w.state = next
			w.orderDirty = !sameModOrder(w.appliedOrder, modOrderIDs(next.Mods))
			w.priorityNeedsApply = next.PriorityNeedsApply
			if w.orderDirty {
				w.status = i18n.Msg(i18n.KeyStatusOrderStaged)
			} else if w.priorityNeedsApply {
				w.status = i18n.Msg(i18n.KeyStatusOrderRestored)
			} else {
				w.status = i18n.Msg(i18n.KeyStatusOrderMatched)
			}
		})
	}()
}

func (w *managerWindow) applyOrder() {
	if w.busy || len(w.state.Mods) == 0 || w.window == nil {
		return
	}
	ids := modOrderIDs(w.state.Mods)
	w.busy = true
	w.problem = nil
	w.status = i18n.Msg(i18n.KeyStatusApplyRunning)
	win := w.window
	go func() {
		next, err := w.manager.SetOrder(context.Background(), ids)
		if win.IsDestroyed() {
			return
		}
		win.Update(func() {
			w.busy = false
			w.state = next
			if err != nil {
				w.orderDirty = !sameModOrder(w.appliedOrder, modOrderIDs(next.Mods))
				w.priorityNeedsApply = next.PriorityNeedsApply
				w.problem = err
				w.status = i18n.Msg(i18n.KeyStatusApplyFailed)
				return
			}
			w.appliedOrder = modOrderIDs(next.Mods)
			w.orderDirty = false
			w.priorityNeedsApply = next.PriorityNeedsApply
			w.problem = nil
			w.status = i18n.Msg(i18n.KeyStatusOrderApplied)
		})
	}()
}

func modOrderIDs(mods []app.ModInfo) []string {
	ids := make([]string, len(mods))
	for i, mod := range mods {
		ids[i] = mod.ID
	}
	return ids
}

func sameModOrder(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for i := range left {
		if left[i] != right[i] {
			return false
		}
	}
	return true
}

func (w *managerWindow) createMergedMod() {
	if w.busy || !w.state.CanMerge || w.orderDirty || w.priorityNeedsApply || w.window == nil {
		return
	}
	w.busy = true
	w.problem = nil
	w.status = i18n.Msg(i18n.KeyStatusMergePreparing)
	w.progress = -1
	w.progressStatus = i18n.Msg(i18n.KeyProgressInstalling)
	w.progressPath = ""
	win := w.window
	metadata := w.mergeMetadata
	go func() {
		report, err := w.manager.CreateMergedMod(context.Background(), metadata, func(progress app.MergeProgress) {
			if win.IsDestroyed() {
				return
			}
			win.Update(func() {
				w.progressPath = progress.Path
				w.progressStatus = i18n.Msg(i18n.KeyProgressPacking)
				if progress.Total > 0 {
					w.progress = float64(progress.Current) / float64(progress.Total)
				}
				if progress.Phase == "installing" {
					w.status = i18n.Msg(i18n.KeyStatusMergeInstalling)
				} else if progress.Phase == "complete" {
					w.status = i18n.Msg(i18n.KeyStatusMergeInstalled)
				} else {
					w.status = i18n.Msg(i18n.KeyStatusMergeProgress, progress.Current, progress.Total)
				}
			})
		})
		if win.IsDestroyed() {
			return
		}
		win.Update(func() {
			w.busy = false
			if err != nil {
				w.problem = err
				w.status = i18n.Msg(i18n.KeyStatusMergeFailed)
				return
			}
			w.problem = nil
			w.state = w.manager.GetState()
			w.appliedOrder = modOrderIDs(w.state.Mods)
			w.orderDirty = false
			w.priorityNeedsApply = w.state.PriorityNeedsApply
			w.status = i18n.Msg(i18n.KeyStatusMergeDone, report.OutputPath)
			w.progressStatus = i18n.Msg(i18n.KeyStatusMergeAssets, report.AssetCount, formatNativeBytes(report.SizeBytes))
			w.progressPath = ""
			w.progress = 1
		})
	}()
}

func (w *managerWindow) launchGame(viaSteam bool) {
	if w.busy || w.window == nil {
		return
	}
	w.busy = true
	w.problem = nil
	if viaSteam {
		w.status = i18n.Msg(i18n.KeyStatusLaunchSteam)
	} else {
		w.status = i18n.Msg(i18n.KeyStatusLaunchDirect)
	}
	win := w.window
	go func() {
		var err error
		if viaSteam {
			err = w.manager.LaunchViaSteam()
		} else {
			err = w.manager.LaunchDirect()
		}
		if win.IsDestroyed() {
			return
		}
		win.Update(func() {
			w.busy = false
			if err != nil {
				w.problem = err
				w.status = i18n.Msg(i18n.KeyStatusLaunchFailed)
				return
			}
			w.problem = nil
			if viaSteam {
				w.status = i18n.Msg(i18n.KeyStatusLaunchedSteam)
			} else {
				w.status = i18n.Msg(i18n.KeyStatusLaunchedDirect)
			}
		})
	}()
}

func formatNativeBytes(size int64) string {
	if size < 1024 {
		return fmt.Sprintf("%d B", size)
	}
	if size < 1024*1024 {
		return fmt.Sprintf("%.1f KB", float64(size)/1024)
	}
	return fmt.Sprintf("%.1f MB", float64(size)/(1024*1024))
}
