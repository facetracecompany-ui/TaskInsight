package main

import (
	_ "embed"
	"os"

	"github.com/energye/systray"
	"golang.org/x/sys/windows/registry"
)

//go:embed icon.ico
var iconICO []byte

const (
	runKey   = `Software\Microsoft\Windows\CurrentVersion\Run`
	runValue = "TaskInsight"
)

func autostartEnabled() bool {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.QUERY_VALUE)
	if err != nil {
		return false
	}
	defer k.Close()
	_, _, err = k.GetStringValue(runValue)
	return err == nil
}

func setAutostart(on bool) error {
	k, err := registry.OpenKey(registry.CURRENT_USER, runKey, registry.SET_VALUE)
	if err != nil {
		return err
	}
	defer k.Close()
	if !on {
		if err := k.DeleteValue(runValue); err != nil && err != registry.ErrNotExist {
			return err
		}
		return nil
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	return k.SetStringValue(runValue, `"`+exe+`" --autostart`)
}

// runTray puts the app in the notification area and blocks until Exit.
func runTray(open func()) {
	systray.Run(func() {
		systray.SetIcon(iconICO)
		systray.SetTitle("TaskInsight")
		systray.SetTooltip("TaskInsight — диспетчер задач")
		systray.SetOnClick(func(systray.IMenu) { open() })
		systray.SetOnDClick(func(systray.IMenu) { open() })
		systray.SetOnRClick(func(m systray.IMenu) { _ = m.ShowMenu() })

		show := systray.AddMenuItem("Открыть", "Показать окно")
		show.Click(open)
		systray.AddSeparator()
		auto := systray.AddMenuItemCheckbox("Запускать с Windows", "Автозапуск при входе в систему", autostartEnabled())
		auto.Click(func() {
			want := !auto.Checked()
			if err := setAutostart(want); err != nil {
				return
			}
			if want {
				auto.Check()
			} else {
				auto.Uncheck()
			}
		})
		systray.AddSeparator()
		systray.AddMenuItem("Выход", "Закрыть TaskInsight").Click(systray.Quit)
	}, func() { os.Exit(0) })
}

