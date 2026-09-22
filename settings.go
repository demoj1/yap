package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
)

// settings is the on-disk state under <config>/yap/settings.json: chosen
// devices, codec knobs, and a remembered volume per friend (by name).
type settings struct {
	mu      sync.Mutex
	path    string
	Mic     string         `json:"mic,omitempty"`     // capture device name, "" = system default
	Out     string         `json:"out,omitempty"`     // playback device name, "" = system default
	Bitrate int            `json:"bitrate,omitempty"` // kbps
	Denoise bool           `json:"denoise"`
	Gate    bool           `json:"gate"`
	Echo    bool           `json:"echo"` // echo cancellation; it only kicks in while the mic hears the speakers
	AGC     bool           `json:"agc"`
	PTT     bool           `json:"ptt"`                // push-to-talk: mic open only while space is held
	Web     bool           `json:"web"`                // serve the browser UI on localhost
	Sounds  bool           `json:"sounds"`             // chimes on join/leave and chat
	Theme   string         `json:"theme,omitempty"`    // web page: "light" or "dark"; "" follows the system
	MicGain int            `json:"mic_gain,omitempty"` // percent, 100 = as captured
	Share   shareCfg       `json:"share"`              // screen share settings: the browser encodes with them
	Volumes map[string]int `json:"volumes,omitempty"`  // friend name -> percent

	AECNLP int `json:"aec_nlp"` // residual echo suppression: 0 soft, 1 normal, 2 hard
}

// configDir is where the link, settings and log live: <user config>/yap.
func configDir() string {
	dir, err := os.UserConfigDir()
	if err != nil {
		panic(err)
	}
	dir = filepath.Join(dir, "yap")
	must(os.MkdirAll(dir, 0o700))
	return dir
}

func loadSettings() *settings {
	s := &settings{Bitrate: 96, Denoise: true, Gate: true, AGC: true, Echo: true, Web: true, Sounds: true, AECNLP: 1, Volumes: map[string]int{}}
	s.path = filepath.Join(configDir(), "settings.json")
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, s) // a corrupt file just falls back to defaults
	}
	if s.Volumes == nil {
		s.Volumes = map[string]int{}
	}
	if s.Bitrate == 0 {
		s.Bitrate = 96
	}
	if s.MicGain == 0 {
		s.MicGain = 100
	}
	if s.Share.Codec == "" || s.Share.Quality == 0 { // also the old "res" shape
		s.Share = shareCfg{Codec: "vp8", Quality: 100, FPS: 30, Kbps: 15000}
	}
	return s
}

func (s *settings) save() {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		panic(err)
	}
	tmp := s.path + ".tmp"
	must(os.WriteFile(tmp, append(raw, '\n'), 0o600))
	must(os.Rename(tmp, s.path))
}

func (s *settings) volume(name string) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.Volumes[name]; ok {
		return v
	}
	return 100
}

func (s *settings) setVolume(name string, v int) {
	s.mu.Lock()
	if name != "" {
		s.Volumes[name] = v
	}
	s.mu.Unlock()
	s.save()
}

// shareCfg is how the browser encodes our screen; kept here so it follows
// the person, not the browser.
type shareCfg struct {
	Codec   string `json:"codec"`   // "vp8" or "vp9"
	Quality int    `json:"quality"` // percent of the captured size (20…100)
	FPS     int    `json:"fps"`
	Kbps    int    `json:"kbps"`
}
