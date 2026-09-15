package main

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const releasesURL = "https://api.github.com/repos/demoj1/yap/releases/latest"

type release struct {
	Tag    string `json:"tag_name"`
	Assets []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
	} `json:"assets"`
}

// latestRelease asks GitHub for the newest tagged build.
func latestRelease() (release, error) {
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(releasesURL)
	if err != nil {
		return release{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return release{}, fmt.Errorf("github: %s", resp.Status)
	}
	var r release
	if err := json.NewDecoder(resp.Body).Decode(&r); err != nil {
		return release{}, err
	}
	return r, nil
}

// newerThan reports whether tag a is a higher vX.Y.Z than b. Anything that
// does not parse (like "dev") is never newer and never older.
func newerThan(a, b string) bool {
	pa, oka := semver(a)
	pb, okb := semver(b)
	if !oka || !okb {
		return false
	}
	for i := range pa {
		if pa[i] != pb[i] {
			return pa[i] > pb[i]
		}
	}
	return false
}

func semver(s string) ([3]int, bool) {
	var v [3]int
	parts := strings.Split(strings.TrimPrefix(s, "v"), ".")
	if len(parts) != 3 {
		return v, false
	}
	for i, p := range parts {
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

// assetName is the release file built for this OS/arch.
func assetName() string {
	switch runtime.GOOS {
	case "darwin":
		return "yap-macos"
	case "windows":
		return "yap-windows-amd64.exe"
	default:
		return "yap-linux-" + runtime.GOARCH
	}
}

// checkUpdate returns the tag of a newer release, or "".
func checkUpdate() string {
	r, err := latestRelease()
	if err != nil || !newerThan(r.Tag, version) {
		return ""
	}
	return r.Tag
}

// restart replaces this process with the (freshly updated) executable,
// same arguments, so the user lands back in the same room. Windows cannot
// exec in place: there a new process is started and this one exits.
func restart() error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if err := syscall.Exec(exe, os.Args, os.Environ()); err == nil || runtime.GOOS != "windows" {
		return err
	}
	p, err := os.StartProcess(exe, os.Args, &os.ProcAttr{Files: []*os.File{os.Stdin, os.Stdout, os.Stderr}})
	if err != nil {
		return err
	}
	p.Release()
	os.Exit(0)
	return nil
}

// selfUpdate downloads this platform's binary from the latest release and
// swaps it in place of the running executable.
func selfUpdate() error {
	r, err := latestRelease()
	if err != nil {
		return err
	}
	if !newerThan(r.Tag, version) && version != "dev" {
		fmt.Printf("already on %s (latest is %s)\n", version, r.Tag)
		return nil
	}
	want := assetName()
	var url string
	for _, a := range r.Assets {
		if a.Name == want {
			url = a.URL
		}
	}
	if url == "" {
		return fmt.Errorf("release %s has no %s", r.Tag, want)
	}
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if exe, err = filepath.EvalSymlinks(exe); err != nil {
		return err
	}
	fmt.Printf("downloading %s %s → %s\n", r.Tag, want, exe)
	tmp := exe + ".new"
	if err := download(url, tmp); err != nil {
		os.Remove(tmp)
		return err
	}
	if runtime.GOOS == "windows" { // a running .exe can be renamed but not overwritten
		old := exe + ".old"
		os.Remove(old)
		if err := os.Rename(exe, old); err != nil {
			os.Remove(tmp)
			return err
		}
	}
	if err := os.Rename(tmp, exe); err != nil {
		os.Remove(tmp)
		return err
	}
	fmt.Println("updated to", r.Tag)
	return nil
}

func download(url, path string) error {
	c := &http.Client{Timeout: 5 * time.Minute}
	resp, err := c.Get(url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return fmt.Errorf("download: %s", resp.Status)
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		return err
	}
	return f.Close()
}

// autoUpdate runs in relay/daemon mode: every minute it checks for a newer
// release, installs it in place, and re-execs so the daemon keeps itself
// current with no attention. The binary must be on a writable path (a bind
// mount, not baked into a read-only image) for the update to persist.
func autoUpdate() {
	for range time.Tick(time.Minute) {
		r, err := latestRelease()
		if err != nil || !newerThan(r.Tag, version) {
			continue
		}
		log.Println("relay: newer release", r.Tag, "— updating")
		if err := selfUpdate(); err != nil {
			log.Println("relay: update failed:", err)
			continue
		}
		log.Println("relay: restarting into", r.Tag)
		if err := restart(); err != nil {
			log.Println("relay: restart failed:", err)
		}
	}
}
