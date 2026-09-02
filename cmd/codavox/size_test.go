package main

import "testing"

func TestParseSize(t *testing.T) {
	good := map[string]int64{
		"1024":  1024,
		"512M":  512 << 20,
		"2G":    2 << 30,
		"4GiB":  4 << 30,
		"8k":    8 << 10,
		"1T":    1 << 40,
		" 3g ":  3 << 30,
		"0":     0,
		"100KB": 100 << 10,
	}
	for in, want := range good {
		got, err := parseSize(in)
		if err != nil || got != want {
			t.Errorf("parseSize(%q) = %d, %v; want %d", in, got, err, want)
		}
	}
	for _, bad := range []string{"", "G", "-1", "1.5G", "2X", "2GB0", "99999999999999999999G"} {
		if _, err := parseSize(bad); err == nil {
			t.Errorf("parseSize(%q) accepted, want an error", bad)
		}
	}
}
