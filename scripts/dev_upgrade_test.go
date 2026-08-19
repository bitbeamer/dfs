package scripts_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestDevUpgradeDiscoversManagedBinaryWithoutDFSOnPath(t *testing.T) {
	root := t.TempDir()
	scripts := filepath.Join(root, "scripts")
	bin := filepath.Join(root, "bin")
	tools := filepath.Join(root, "tools")
	installed := filepath.Join(root, "installed dfs")
	for _, directory := range []string{scripts, bin, tools} {
		if err := os.MkdirAll(directory, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	source, err := os.ReadFile("dev-upgrade.sh")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(scripts, "dev-upgrade.sh"), source, 0o755); err != nil {
		t.Fatal(err)
	}
	candidate := `#!/usr/bin/env bash
case "$*" in
  "service list --output json") printf '%s\n' '{"result":[{"binary":"` + installed + `","filesystem_id":"abc123"}]}' ;;
  "service list") printf '%s\n' 'RUNNING' ;;
  "health --filesystem abc123") printf '%s\n' 'HEALTHY' ;;
  "--version") printf '%s\n' 'dfs candidate' ;;
  "upgrade --from "*) printf '%s\n' 'Dry run' ;;
  *) printf 'unexpected candidate arguments: %s\n' "$*" >&2; exit 1 ;;
esac
`
	writeExecutable(t, filepath.Join(bin, "dfs"), candidate)
	writeExecutable(t, installed, "#!/usr/bin/env bash\nprintf '%s\\n' 'dfs installed'\n")
	writeExecutable(t, filepath.Join(tools, "git"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(tools, "go"), "#!/usr/bin/env bash\nexit 0\n")
	writeExecutable(t, filepath.Join(tools, "make"), "#!/usr/bin/env bash\nexit 0\n")

	command := exec.Command(filepath.Join(scripts, "dev-upgrade.sh"), "--no-fetch", "--dry-run")
	command.Env = append(os.Environ(), "PATH="+tools+":/bin:/usr/bin")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("dev upgrade failed without dfs on PATH: %v\n%s", err, output)
	}
	if text := string(output); !strings.Contains(text, "Installed: dfs installed") || !strings.Contains(text, "Dry run complete") {
		t.Fatalf("dev upgrade output = %q", text)
	}
}

func writeExecutable(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0o755); err != nil {
		t.Fatal(err)
	}
}
