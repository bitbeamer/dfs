package config

import "testing"

func TestParseSize(t *testing.T) {
	tests := map[string]int64{
		"1":      1,
		"10MiB":  10 << 20,
		"1.5GiB": int64(1.5 * (1 << 30)),
		"2 GB":   2_000_000_000,
	}
	for input, expected := range tests {
		actual, err := ParseSize(input)
		if err != nil {
			t.Fatalf("ParseSize(%q): %v", input, err)
		}
		if actual != expected {
			t.Fatalf("ParseSize(%q) = %d, want %d", input, actual, expected)
		}
	}
}

func TestParseSizeRejectsInvalidValues(t *testing.T) {
	for _, input := range []string{"", "-1GiB", "many"} {
		if _, err := ParseSize(input); err == nil {
			t.Fatalf("ParseSize(%q) unexpectedly succeeded", input)
		}
	}
}
