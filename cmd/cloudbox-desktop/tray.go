//go:build !darwin || cgo

package main

import (
	"fyne.io/systray"
)

var stateText = map[string]string{
	"setup": "нужна настройка", "idle": "синхронизировано", "syncing": "синхронизация…", "paused": "на паузе", "error": "ошибка",
}

// runTray показывает значок в трее и возвращается при выходе.
// Без трея (Linux без DBus) клиент просто работает в фоне — окно открывается в браузере.
func runTray(app *App) {
	if !trayAvailable() {
		app.logf("значок в трее недоступен (нет DBus) — окно открывается повторным запуском программы")
		<-app.quit
		return
	}
	systray.Run(func() {
		systray.SetTitle("CloudBox")
		mOpen := systray.AddMenuItem("Открыть CloudBox", "Настройки и журнал")
		mWeb := systray.AddMenuItem("Открыть сайт CloudBox", "")
		mFolder := systray.AddMenuItem("Открыть папку", "")
		systray.AddSeparator()
		mSync := systray.AddMenuItem("Синхронизировать сейчас", "")
		mPause := systray.AddMenuItem("Пауза", "")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Выход", "")
		update := func() {
			st := app.Status()
			systray.SetIcon(iconFor(st.State))
			tip := "CloudBox — " + stateText[st.State]
			if st.Error != "" {
				tip += ": " + st.Error
			}
			systray.SetTooltip(tip)
			if st.Paused {
				mPause.SetTitle("Продолжить")
			} else {
				mPause.SetTitle("Пауза")
			}
		}
		app.onChange = update
		update()
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					openURL(app.URL())
				case <-mWeb.ClickedCh:
					if s := app.Status().Server; s != "" {
						openURL(s)
					}
				case <-mFolder.ClickedCh:
					if p := app.Status().Local; p != "" {
						openURL(p)
					}
				case <-mSync.ClickedCh:
					app.SyncNow()
				case <-mPause.ClickedCh:
					if err := app.SetPaused(!app.Status().Paused); err != nil {
						app.logf("пауза: %v", err)
					}
				case <-mQuit.ClickedCh:
					app.Quit()
				case <-app.quit:
					systray.Quit()
					return
				}
			}
		}()
	}, func() { app.Quit() })

}
