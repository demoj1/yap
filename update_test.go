package main

import "testing"

func TestNewerThan(t *testing.T) {
	cases := []struct {
		a, b string
		want bool
	}{
		{"v0.5.4", "v0.5.3", true},
		{"v0.6.0", "v0.5.9", true},
		{"v1.0.0", "v0.9.9", true},
		{"v0.5.3", "v0.5.3", false},
		{"v0.5.2", "v0.5.3", false},
		{"v0.5.4", "dev", false},
		{"dev", "v0.5.3", false},
		{"garbage", "v0.5.3", false},
	}
	for _, c := range cases {
		if got := newerThan(c.a, c.b); got != c.want {
			t.Errorf("newerThan(%q,%q)=%v want %v", c.a, c.b, got, c.want)
		}
	}
}
