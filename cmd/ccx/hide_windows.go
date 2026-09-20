//go:build windows

package main

import "syscall"

// hideConsole hides the console window the process is attached to, so the
// autostart task runs without a visible terminal. It hides whatever console
// this process owns, which is why only the --hidden service path calls it.
func hideConsole() {
	getConsoleWindow := syscall.NewLazyDLL("kernel32.dll").NewProc("GetConsoleWindow")
	showWindow := syscall.NewLazyDLL("user32.dll").NewProc("ShowWindow")
	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		return
	}
	const swHide = 0
	showWindow.Call(hwnd, swHide)
}
