//go:build !windows

package main

func setAutostartWindows(bool, string) error { return nil }
