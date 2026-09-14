package main

import (
	"fmt"

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
	if len(s) < len(prefix) {
		return false
	}
	for i := 0; i < len(prefix); i++ {
		if lower(s[i]) != lower(prefix[i]) {
			return false
		}
	}
	return true
}

func lower(b byte) byte {
	if b >= 'A' && b <= 'Z' {
		return b + 32
	}
	return b
}

// deviceID resolves a device name to a copied ID within an existing context.
// The copy is stable (not a moving slice element) so it can be pinned and
// handed to C. Returns nil for "" or no match, i.e. the system default.
func deviceID(ctx malgo.Context, kind malgo.DeviceType, name string) *malgo.DeviceID {
	if name == "" {
		return nil
	}
	devs, err := ctx.Devices(kind)
	if err != nil {
		return nil
	}
	pick := func() *malgo.DeviceID {
		for i := range devs {
			if devs[i].Name() == name {
				id := devs[i].ID
				return &id
			}
		}
		for i := range devs {
			if hasPrefixFold(devs[i].Name(), name) {
				id := devs[i].ID
				return &id
			}
		}
		return nil
	}
	return pick()
}
