//go:build windows

// Package tray is how yap lives on Windows when started by a double-click
// or a yap:// link: the console window it was given is hidden, an icon
// sits in the tray with Open / Copy link / Quit, and the browser page is
// the UI. Started from a terminal, none of this happens.
package tray

import (
	_ "embed"
	"syscall"
	"unsafe"

	"fyne.io/systray"
)

//go:embed yap.ico
var icon []byte

var (
	kernel32              = syscall.NewLazyDLL("kernel32.dll")
	user32                = syscall.NewLazyDLL("user32.dll")
	getConsoleWindow      = kernel32.NewProc("GetConsoleWindow")
	getConsoleProcessList = kernel32.NewProc("GetConsoleProcessList")
	showWindow            = user32.NewProc("ShowWindow")
	messageBox            = user32.NewProc("MessageBoxW")
)

// OwnConsole reports whether this process is the only one on its console:
// Windows made it just for us, so nobody is reading it.
func OwnConsole() bool {
	var pids [2]uint32
	n, _, _ := getConsoleProcessList.Call(uintptr(unsafe.Pointer(&pids[0])), uintptr(len(pids)))
	return n == 1
}

// Console hides or shows the console window Windows gave us.
func Console(show bool) {
	hwnd, _, _ := getConsoleWindow.Call()
	if hwnd == 0 {
		return
	}
	cmd := uintptr(0) // SW_HIDE
	if show {
		cmd = 5 // SW_SHOW
	}
	showWindow.Call(hwnd, cmd)
}

// Alert shows a message box: with the console hidden there is nowhere
// else for a fatal error to be seen.
func Alert(title, text string) {
	t, _ := syscall.UTF16PtrFromString(title)
	m, _ := syscall.UTF16PtrFromString(text)
	messageBox.Call(0, uintptr(unsafe.Pointer(m)), uintptr(unsafe.Pointer(t)), 0x10) // MB_ICONERROR
}

// Run blocks in the tray until Quit is chosen or stop is closed. open,
// copyLink: what the menu items do. Must run on the main goroutine.
// Returns false if the tray could not start.
func Run(open, copyLink func(), stop <-chan struct{}) bool {
	ok := make(chan bool, 1)
	go func() {
		select {
		case <-stop:
			systray.Quit()
		}
	}()
	systray.Run(func() {
		systray.SetIcon(icon)
		systray.SetTitle("yap")
		systray.SetTooltip("yap — voice call")
		mOpen := systray.AddMenuItem("Open yap", "the page in your browser")
		mCopy := systray.AddMenuItem("Copy link", "the room link, for friends")
		systray.AddSeparator()
		mQuit := systray.AddMenuItem("Quit", "leave the room")
		ok <- true
		go func() {
			for {
				select {
				case <-mOpen.ClickedCh:
					open()
				case <-mCopy.ClickedCh:
					copyLink()
				case <-mQuit.ClickedCh:
					systray.Quit()
					return
				}
			}
		}()
	}, func() {
		select {
		case ok <- false:
		default:
		}
	})
	select {
	case started := <-ok:
		return started
	default:
		return false
	}
}
