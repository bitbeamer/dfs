package repository

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func TestTwoPeerConcurrentChangesConverge(t *testing.T) {
	if _, err := exec.LookPath("git-annex"); err != nil {
		t.Skip("git-annex is not installed")
	}

	t.Run("edits retain both versions", func(t *testing.T) {
		linux, mac, ctx := newConvergencePeers(t, "shared.txt", "baseline\n")

		writePeerFile(t, ctx, linux, "shared.txt", "edited on linux\n")
		writePeerFile(t, ctx, mac, "shared.txt", "edited on macOS\n")
		commitPeerChange(t, ctx, linux)
		commitPeerChange(t, ctx, mac)

		paths := convergePeers(t, ctx, linux, mac)
		shared := pathsForConflict(paths, "shared.txt")
		if len(shared) != 2 {
			t.Fatalf("concurrent edits produced paths %q, want original and one variant", paths)
		}
		assertPeerContents(t, ctx, linux, shared, []string{"edited on linux\n", "edited on macOS\n"})
		assertPeerContents(t, ctx, mac, shared, []string{"edited on linux\n", "edited on macOS\n"})
	})

	t.Run("rename and move retain both destinations", func(t *testing.T) {
		linux, mac, ctx := newConvergencePeers(t, "document.txt", "same content\n")

		renamePeerFile(t, linux, "document.txt", "renamed.txt")
		renamePeerFile(t, mac, "document.txt", "Archive/document.txt")
		commitPeerChange(t, ctx, linux)
		commitPeerChange(t, ctx, mac)

		paths := convergePeers(t, ctx, mac, linux)
		if len(paths) != 2 || paths[0] != "Archive/document.txt" || !isConflictPath(paths[1], "renamed.txt") {
			t.Fatalf("converged paths = %q, want Archive/document.txt and renamed[.variant-*].txt", paths)
		}
		assertPeerContents(t, ctx, linux, paths, []string{"same content\n", "same content\n"})
		assertPeerContents(t, ctx, mac, paths, []string{"same content\n", "same content\n"})
	})

	t.Run("modification survives concurrent deletion", func(t *testing.T) {
		linux, mac, ctx := newConvergencePeers(t, "notes.txt", "baseline\n")

		if err := os.Remove(filepath.Join(linux.Config.Repository, "notes.txt")); err != nil {
			t.Fatal(err)
		}
		writePeerFile(t, ctx, mac, "notes.txt", "important macOS edit\n")
		commitPeerChange(t, ctx, linux)
		commitPeerChange(t, ctx, mac)

		paths := convergePeers(t, ctx, linux, mac)
		if len(paths) != 1 || !isVariantPath(paths[0], "notes.txt") {
			t.Fatalf("modify/delete paths = %q, want one notes.variant-*.txt", paths)
		}
		assertPeerContents(t, ctx, linux, paths, []string{"important macOS edit\n"})
		assertPeerContents(t, ctx, mac, paths, []string{"important macOS edit\n"})
	})

	t.Run("connected operations propagate without variants", func(t *testing.T) {
		linux, mac, ctx := newConvergencePeers(t, "online.txt", "baseline\n")

		writePeerFile(t, ctx, linux, "online.txt", "connected edit\n")
		commitPeerChange(t, ctx, linux)
		paths := convergePeers(t, ctx, linux, mac)
		if len(paths) != 1 || paths[0] != "online.txt" {
			t.Fatalf("connected edit produced paths %q", paths)
		}
		assertPeerContents(t, ctx, mac, paths, []string{"connected edit\n"})

		renamePeerFile(t, mac, "online.txt", "Connected/renamed.txt")
		commitPeerChange(t, ctx, mac)
		paths = convergePeers(t, ctx, mac, linux)
		if len(paths) != 1 || paths[0] != "Connected/renamed.txt" {
			t.Fatalf("connected move produced paths %q", paths)
		}

		if err := os.Remove(filepath.Join(linux.Config.Repository, "Connected", "renamed.txt")); err != nil {
			t.Fatal(err)
		}
		commitPeerChange(t, ctx, linux)
		if paths = convergePeers(t, ctx, linux, mac); len(paths) != 0 {
			t.Fatalf("connected delete left paths %q", paths)
		}
	})

	t.Run("identical baselines do not cross-pair concurrent operations", func(t *testing.T) {
		linux, mac, ctx := newConvergencePeers(t, "edit.txt", "identical baseline\n")
		for _, path := range []string{"relocate.txt", "delete.txt"} {
			writePeerFile(t, ctx, linux, path, "identical baseline\n")
		}
		commitPeerChange(t, ctx, linux)
		convergePeers(t, ctx, linux, mac)
		for _, path := range []string{"relocate.txt", "delete.txt"} {
			if err := mac.Fetch(ctx, path, "origin"); err != nil {
				t.Fatal(err)
			}
		}

		writePeerFile(t, ctx, linux, "edit.txt", "linux edit\n")
		renamePeerFile(t, linux, "relocate.txt", "renamed.txt")
		if err := os.Remove(filepath.Join(linux.Config.Repository, "delete.txt")); err != nil {
			t.Fatal(err)
		}
		writePeerFile(t, ctx, mac, "edit.txt", "macOS edit\n")
		renamePeerFile(t, mac, "relocate.txt", "Archive/relocate.txt")
		writePeerFile(t, ctx, mac, "delete.txt", "modified before delete arrived\n")
		commitPeerChange(t, ctx, linux)
		commitPeerChange(t, ctx, mac)

		paths := convergePeers(t, ctx, linux, mac)
		if len(paths) != 5 {
			t.Fatalf("combined conflicts retained %d paths in %q, want 5", len(paths), paths)
		}
		if !containsPath(paths, "Archive/relocate.txt") || len(pathsForConflict(paths, "renamed.txt")) != 1 {
			t.Fatalf("rename/move conflict did not retain both destinations: %q", paths)
		}
		wantContents := []string{
			"identical baseline\n", "identical baseline\n", "linux edit\n", "macOS edit\n", "modified before delete arrived\n",
		}
		assertPeerContents(t, ctx, linux, paths, append([]string(nil), wantContents...))
		assertPeerContents(t, ctx, mac, paths, append([]string(nil), wantContents...))
	})
}

func newConvergencePeers(t *testing.T, path, content string) (*Repository, *Repository, context.Context) {
	t.Helper()
	home := t.TempDir()
	t.Cleanup(func() { makeTreeWritable(home) })
	gitconfig := []byte("[user]\n\tname = DFS Test\n\temail = dfs@example.invalid\n")
	if err := os.WriteFile(filepath.Join(home, ".gitconfig"), gitconfig, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("HOME", home)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	t.Cleanup(cancel)

	linux, err := Init(ctx, filepath.Join(home, "linux"), "linux", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = linux.Close() })
	writePeerFile(t, ctx, linux, path, content)
	commitPeerChange(t, ctx, linux)

	mac, err := Join(ctx, linux.Config.Repository, filepath.Join(home, "mac"), "mac", 10<<20)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = mac.Close() })
	if err := linux.AddRemote(ctx, "mac", mac.Config.Repository); err != nil {
		t.Fatal(err)
	}
	if err := mac.Fetch(ctx, path, "origin"); err != nil {
		t.Fatal(err)
	}
	return linux, mac, ctx
}

func writePeerFile(t *testing.T, ctx context.Context, repo *Repository, path, content string) {
	t.Helper()
	fullPath := filepath.Join(repo.Config.Repository, filepath.FromSlash(path))
	if _, err := os.Lstat(fullPath); err == nil {
		if err := repo.Unlock(ctx, path); err != nil {
			t.Fatal(err)
		}
	} else if !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(fullPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func renamePeerFile(t *testing.T, repo *Repository, oldPath, newPath string) {
	t.Helper()
	oldFullPath := filepath.Join(repo.Config.Repository, filepath.FromSlash(oldPath))
	newFullPath := filepath.Join(repo.Config.Repository, filepath.FromSlash(newPath))
	if err := os.MkdirAll(filepath.Dir(newFullPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(oldFullPath, newFullPath); err != nil {
		t.Fatal(err)
	}
}

func commitPeerChange(t *testing.T, ctx context.Context, repo *Repository) {
	t.Helper()
	committed, err := repo.CommitPending(ctx, "Concurrent peer test change")
	if err != nil {
		t.Fatal(err)
	}
	if !committed {
		t.Fatal("peer change was not committed")
	}
}

// convergePeers deliberately starts in either order, then repeats a round to
// prove that convergence is a stable fixed point rather than a one-sided merge.
func convergePeers(t *testing.T, ctx context.Context, first, second *Repository) []string {
	t.Helper()
	for range 2 {
		if err := first.Sync(ctx, true); err != nil {
			t.Fatal(err)
		}
		if err := second.Sync(ctx, true); err != nil {
			t.Fatal(err)
		}
	}
	firstTree := gitOutput(t, ctx, first, "rev-parse", "HEAD^{tree}")
	secondTree := gitOutput(t, ctx, second, "rev-parse", "HEAD^{tree}")
	if firstTree != secondTree {
		t.Fatalf("peers did not converge: first tree %s, second tree %s", firstTree, secondTree)
	}
	if unmerged := gitOutput(t, ctx, first, "diff", "--name-only", "--diff-filter=U"); unmerged != "" {
		t.Fatalf("first peer has unresolved paths: %s", unmerged)
	}
	if unmerged := gitOutput(t, ctx, second, "diff", "--name-only", "--diff-filter=U"); unmerged != "" {
		t.Fatalf("second peer has unresolved paths: %s", unmerged)
	}
	return trackedPaths(t, ctx, first)
}

func trackedPaths(t *testing.T, ctx context.Context, repo *Repository) []string {
	t.Helper()
	out := gitOutput(t, ctx, repo, "ls-tree", "-r", "--name-only", "HEAD")
	var paths []string
	for _, path := range strings.Split(out, "\n") {
		if path != "" && path != ".gitignore" {
			paths = append(paths, path)
		}
	}
	sort.Strings(paths)
	return paths
}

func pathsForConflict(paths []string, original string) []string {
	var matches []string
	for _, path := range paths {
		if isConflictPath(path, original) {
			matches = append(matches, path)
		}
	}
	return matches
}

func containsPath(paths []string, want string) bool {
	for _, path := range paths {
		if path == want {
			return true
		}
	}
	return false
}

func isConflictPath(path, original string) bool {
	return path == original || isVariantPath(path, original)
}

func isVariantPath(path, original string) bool {
	ext := filepath.Ext(original)
	stem := strings.TrimSuffix(original, ext)
	return strings.HasPrefix(path, stem+".variant-") && strings.HasSuffix(path, ext)
}

func assertPeerContents(t *testing.T, ctx context.Context, repo *Repository, paths, want []string) {
	t.Helper()
	var got []string
	for _, path := range paths {
		if err := repo.Fetch(ctx, path, ""); err != nil {
			t.Fatalf("fetch %s: %v", path, err)
		}
		content, err := os.ReadFile(filepath.Join(repo.Config.Repository, filepath.FromSlash(path)))
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		got = append(got, string(content))
	}
	sort.Strings(got)
	sort.Strings(want)
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("peer contents = %q, want %q", got, want)
	}
}

func gitOutput(t *testing.T, ctx context.Context, repo *Repository, args ...string) string {
	t.Helper()
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Dir = repo.Config.Repository
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v: %s", strings.Join(args, " "), err, out)
	}
	return strings.TrimSpace(string(out))
}

func makeTreeWritable(root string) {
	_ = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err == nil {
			_ = os.Chmod(path, info.Mode()|0o700)
		}
		return nil
	})
}
