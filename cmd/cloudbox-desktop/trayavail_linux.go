//go:build linux

package main

import "github.com/godbus/dbus/v5"

// trayAvailable — есть ли сессионная шина DBus для значка в трее (StatusNotifierItem).
// Без неё systray падает при выходе, поэтому клиент тогда работает без значка.
func trayAvailable() bool {
	conn, err := dbus.ConnectSessionBus()
	if err != nil {
		return false
	}
	conn.Close()
	return true
}
