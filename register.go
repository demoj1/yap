package main

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// registerScheme makes yap:// links open this binary: click one in a
// browser or a chat and yap joins the room. Windows keeps it in the
// registry, Linux in a .desktop file; both are per-user and idempotent.
// macOS wants an .app bundle for that, so nothing is done there.
func registerScheme() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	switch runtime.GOOS {
	case "windows":
		key := `HKCU\Software\Classes\yap`
		for _, args := range [][]string{
			{"add", key, "/ve", "/d", "URL:yap", "/f"},
			{"add", key, "/v", "URL Protocol", "/d", "", "/f"},
			{"add", key + `\shell\open\command`, "/ve", "/d", fmt.Sprintf(`"%s" join "%%1"`, exe), "/f"},
		} {
			if out, err := exec.Command("reg", args...).CombinedOutput(); err != nil {
				log.Printf("yap:// handler: %v: %s", err, out)
				return
			}
		}
	case "linux":
		data := os.Getenv("XDG_DATA_HOME")
		if data == "" {
			data = filepath.Join(os.Getenv("HOME"), ".local", "share")
		}
		dir := filepath.Join(data, "applications")
		if os.MkdirAll(dir, 0o755) != nil {
			return
		}
		entry := fmt.Sprintf("[Desktop Entry]\nType=Application\nName=yap\nExec=%s join %%u\nTerminal=true\nNoDisplay=true\nMimeType=x-scheme-handler/yap;\n", exe)
		path := filepath.Join(dir, "yap.desktop")
		if old, err := os.ReadFile(path); err == nil && string(old) == entry {
			return // already ours
		}
		if err := os.WriteFile(path, []byte(entry), 0o644); err != nil {
			return
		}
		exec.Command("xdg-mime", "default", "yap.desktop", "x-scheme-handler/yap").Run()
		exec.Command("update-desktop-database", dir).Run()
	}
}
