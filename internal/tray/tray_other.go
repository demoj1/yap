//go:build !windows

// Package tray is Windows-only; elsewhere yap has a terminal.
package tray

func OwnConsole() bool                                     { return false }
func Console(show bool)                                    {}
func Alert(title, text string)                             {}
func Run(open, copyLink func(), stop <-chan struct{}) bool { return false }
