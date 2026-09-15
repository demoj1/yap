package main

import (
	"path/filepath"
	"strings"
	"testing"
)

// testModel is a screen over a node with no socket and no sound card: enough
// to draw every frame and press every key.
func testModel(t *testing.T) model {
	set := &settings{path: filepath.Join(t.TempDir(), "settings.json"), Volumes: map[string]int{}, AECTail: 300, AECSuppress: -60, AECSuppressActive: -30}
	n := newNode(newLink(), "me", &controls{}, set)
	n.audio = &audio{}
	n.rebuildRoster()
	m := model{n: n, views: map[*peer]*view{}, cache: &panelCache{}, inputs: []string{"", "USB mic"}, outputs: []string{"", "Speakers"}}
	return m
}

// Every width must render without panicking and every line must fit.
func TestRenderFits(t *testing.T) {
	for _, width := range []int{0, 60, 90, 130} {
		m := testModel(t)
		m.width, m.height = width, 40
		lines, _ := m.render()
		if len(lines) == 0 {
			t.Fatalf("width %d: empty screen", width)
		}
		for _, ln := range lines {
			if width > 0 && visibleWidth(ln) > width {
				t.Errorf("width %d: line overflows: %q", width, ln)
			}
		}
	}
}

// Every key the help advertises must be handled without panicking, and
// the letter keys must flip their toggle.
func TestKeys(t *testing.T) {
	m := testModel(t)
	// "c" is left out: it would put a test link on the real clipboard.
	for _, key := range []string{"m", "d", "g", "e", "a", "l", "p", " ", "tab", "[", "]", "up", "down", "left", "right", "+", "-", "y", "n"} {
		mm, _ := m.act(key)
		m = mm.(model)
	}
	for _, tg := range m.toggles() {
		before := tg.on()
		m.act(tg.key)
		if tg.on() == before && tg.key != "l" { // lock refuses with nobody in the room
			t.Errorf("%s did not flip %s", tg.key, tg.label)
		}
	}
	if _, cmd := m.act("q"); cmd == nil {
		t.Fatal("q must quit")
	}
}

func visibleWidth(s string) int {
	// strip SGR sequences: ESC [ ... m
	n := 0
	for i := 0; i < len(s); {
		if s[i] == 0x1b {
			j := strings.IndexByte(s[i:], 'm')
			if j < 0 {
				break
			}
			i += j + 1
			continue
		}
		n++
		if s[i] >= 0xC0 { // one visible cell per rune, roughly
			for i++; i < len(s) && s[i]&0xC0 == 0x80; i++ {
			}
			continue
		}
		i++
	}
	return n
}
