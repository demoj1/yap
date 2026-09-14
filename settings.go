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
	Volumes map[string]int `json:"volumes,omitempty"` // friend name -> percent
}

func loadSettings() *settings {
	s := &settings{Bitrate: 96, Denoise: true, Volumes: map[string]int{}}
	dir, err := os.UserConfigDir()
	if err != nil {
		panic(err)
	}
	s.path = filepath.Join(dir, "yap", "settings.json")
	if raw, err := os.ReadFile(s.path); err == nil {
		_ = json.Unmarshal(raw, s) // a corrupt file just falls back to defaults
	}
	if s.Volumes == nil {
		s.Volumes = map[string]int{}
	}
	if s.Bitrate == 0 {
		s.Bitrate = 96
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
	must(os.MkdirAll(filepath.Dir(s.path), 0o700))
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

func jsonUnmarshal(raw []byte, s *settings) error { return json.Unmarshal(raw, s) }
