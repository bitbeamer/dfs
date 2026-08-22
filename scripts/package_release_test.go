package scripts_test

import (
	"archive/tar"
	"compress/gzip"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestPackageReleaseCreatesInstallableArchiveAndChecksum(t *testing.T) {
	output := t.TempDir()
	command := exec.Command("./package-release.sh", "v1.2.3-alpha.1", "linux", "amd64", output)
	if combined, err := command.CombinedOutput(); err != nil {
		t.Fatalf("package release: %v\n%s", err, combined)
	}

	base := "dfs-v1.2.3-alpha.1-linux-amd64"
	archive := filepath.Join(output, base+".tar.gz")
	want := map[string]int64{
		base + "/bin/dfs":                    0o755,
		base + "/scripts/install-cachyos.sh": 0o755,
		base + "/README.md":                  0o644,
		base + "/INSTALL.md":                 0o644,
		base + "/LICENSE":                    0o644,
	}
	file, err := os.Open(archive)
	if err != nil {
		t.Fatal(err)
	}
	decompressed, err := gzip.NewReader(file)
	if err != nil {
		file.Close()
		t.Fatal(err)
	}
	for reader := tar.NewReader(decompressed); ; {
		header, err := reader.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if mode, ok := want[header.Name]; ok {
			if header.Mode&0o777 != mode {
				t.Errorf("%s mode = %o, want %o", header.Name, header.Mode&0o777, mode)
			}
			delete(want, header.Name)
		}
	}
	decompressed.Close()
	file.Close()
	if len(want) != 0 {
		t.Fatalf("archive is missing entries: %v", want)
	}

	checksum, err := os.ReadFile(archive + ".sha256")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(strings.TrimSpace(string(checksum)), "  "+filepath.Base(archive)) {
		t.Fatalf("checksum does not name archive: %q", checksum)
	}
}

func TestPackageReleaseRejectsInvalidTarget(t *testing.T) {
	command := exec.Command("./package-release.sh", "latest", "windows", "386", t.TempDir())
	if err := command.Run(); err == nil {
		t.Fatal("invalid release target unexpectedly succeeded")
	}
}
