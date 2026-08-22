package repository

import "testing"

func TestParseContentFind(t *testing.T) {
	output := `{"file":"Documents/report.pdf","key":"SHA256E-s1048576--abc","bytesize":"1048576"}

{"file":"Photos/Vacation/beach.jpg","key":"SHA256E-s2048--def","bytesize":"2048"}
`
	files, err := parseContentFind(output)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 {
		t.Fatalf("parsed %d files, want 2", len(files))
	}
	if files[0].Path != "Documents/report.pdf" || files[0].Key != "SHA256E-s1048576--abc" || files[0].Size != 1048576 {
		t.Fatalf("first file = %+v", files[0])
	}
	if files[1].Path != "Photos/Vacation/beach.jpg" || files[1].Size != 2048 {
		t.Fatalf("second file = %+v", files[1])
	}
}

func TestParseContentFindEmpty(t *testing.T) {
	files, err := parseContentFind("")
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 0 {
		t.Fatalf("parsed %d files, want 0", len(files))
	}
}

func TestParseContentFindRejectsInvalidSize(t *testing.T) {
	if _, err := parseContentFind(`{"file":"a.txt","key":"k","bytesize":"large"}`); err == nil {
		t.Fatal("expected an error for a non-numeric bytesize")
	}
}
