package repository

import "testing"

func TestParseAnnexHealthFiles(t *testing.T) {
	files, err := parseAnnexHealthFiles("{\"file\":\"Archive/item.txt\",\"bytesize\":\"42\"}\n")
	if err != nil || len(files) != 1 || files[0].Path != "Archive/item.txt" || files[0].Size != 42 {
		t.Fatalf("files = %#v, %v", files, err)
	}
}

func TestPathMatchesAnyPin(t *testing.T) {
	for _, test := range []struct {
		path string
		pins []string
		want bool
	}{
		{"Archive/item.txt", []string{"Archive"}, true},
		{"Archive/item.txt", []string{"Archive/item.txt"}, true},
		{"Other/item.txt", []string{"Archive"}, false},
	} {
		if got := pathMatchesAnyPin(test.path, test.pins); got != test.want {
			t.Errorf("pathMatchesAnyPin(%q, %q) = %v", test.path, test.pins, got)
		}
	}
}
