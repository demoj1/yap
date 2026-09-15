package main

import (
	"fmt"
	"strings"

	"github.com/gen2brain/malgo"
)

// listDevices returns capture (or playback) device names, default first-marked.
func listDevices(kind malgo.DeviceType) ([]malgo.DeviceInfo, error) {
	ctx, err := malgo.InitContext(nil, malgo.ContextConfig{}, nil)
	if err != nil {
		return nil, err
	}
	defer func() { ctx.Uninit(); ctx.Free() }()
	return ctx.Devices(kind)
}

func printDevices() {
	for _, kind := range []malgo.DeviceType{malgo.Capture, malgo.Playback} {
		label := "microphones"
		if kind == malgo.Playback {
			label = "speakers"
		}
		fmt.Printf("\n%s:\n", label)
		devs, err := listDevices(kind)
		if err != nil {
			fmt.Println("  error:", err)
			continue
		}
		for _, d := range devs {
			mark := " "
			if d.IsDefault != 0 {
				mark = "*"
			}
			fmt.Printf(" %s %s\n", mark, d.Name())
		}
	}
	fmt.Println("\n* = system default. Use -mic / -out with a name (or a unique prefix).")
}

func hasPrefixFold(s, prefix string) bool {
	return len(s) >= len(prefix) && strings.EqualFold(s[:len(prefix)], prefix)
}

// deviceID resolves a device name to a copied ID within an existing context:
// "" is the system default, otherwise an exact name, then a case-insensitive
// prefix. The copy is stable (not a moving slice element) so it can be
// pinned and handed to C. nil means let miniaudio choose.
func deviceID(ctx malgo.Context, kind malgo.DeviceType, name string) *malgo.DeviceID {
	devs, err := ctx.Devices(kind)
	if err != nil {
		return nil
	}
	first := func(ok func(d *malgo.DeviceInfo) bool) *malgo.DeviceID {
		for i := range devs {
			if ok(&devs[i]) {
				id := devs[i].ID
				return &id
			}
		}
		return nil
	}
	if name == "" {
		return first(func(d *malgo.DeviceInfo) bool { return d.IsDefault != 0 })
	}
	if id := first(func(d *malgo.DeviceInfo) bool { return d.Name() == name }); id != nil {
		return id
	}
	return first(func(d *malgo.DeviceInfo) bool { return hasPrefixFold(d.Name(), name) })
}

// isMonitor reports whether a capture device is a PulseAudio/PipeWire monitor
// (loopback of an output) rather than a real microphone. These are useless as
// a call input, so the picker skips them.
func isMonitor(name string) bool {
	return hasPrefixFold(name, "Monitor of ") || hasPrefixFold(name, "Monitor ")
}
