package main

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSettingsRoundtrip(t *testing.T) {
	dir := t.TempDir()
	s := &settings{path: filepath.Join(dir, "settings.json"), Bitrate: 96, Denoise: true, Volumes: map[string]int{}}
	s.Mic = "Микрофон"
	s.setVolume("lorens", 80)
	s.setVolume("", 50) // anonymous peer must not be stored
	s.Denoise = false
	s.save()

	raw, err := os.ReadFile(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.path + ".tmp"); err == nil {
		t.Fatal("temp file left behind")
	}

	got := &settings{path: s.path, Volumes: map[string]int{}}
	loadInto(t, raw, got)
	if got.Mic != "Микрофон" {
		t.Fatalf("mic not persisted: %q", got.Mic)
	}
	if got.volume("lorens") != 80 {
		t.Fatalf("lorens volume want 80 got %d", got.volume("lorens"))
	}
	if got.volume("stranger") != 100 {
		t.Fatalf("unknown peer must default to 100, got %d", got.volume("stranger"))
	}
	if _, ok := got.Volumes[""]; ok {
		t.Fatal("empty name must never be stored")
	}
}

func loadInto(t *testing.T, raw []byte, s *settings) {
	t.Helper()
	if err := jsonUnmarshal(raw, s); err != nil {
		t.Fatal(err)
	}
	if s.Volumes == nil {
		s.Volumes = map[string]int{}
	}
}
