package treefs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"
	"time"
)

func TestLocalAndVirtualTraversal(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "nested"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "nested", "file"), []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	virtual := fstest.MapFS{"nested/file": &fstest.MapFile{Data: []byte("content"), Mode: 0o600}}
	for name, tree := range map[string]fs.ReadLinkFS{"local": Local(root), "virtual": virtual} {
		t.Run(name, func(t *testing.T) {
			var paths []string
			err := Walk(tree, func(name string, parent fs.ReadLinkFS, entry fs.DirEntry) error {
				paths = append(paths, name)
				if entry.IsDir() {
					return nil
				}
				f, err := OpenFile(parent, entry.Name())
				if err != nil {
					return err
				}
				data, err := io.ReadAll(f)
				if string(data) != "content" {
					t.Errorf("content = %q", data)
				}
				return errors.Join(err, f.Close())
			})
			if err != nil || strings.Join(paths, ",") != "nested,nested/file" {
				t.Fatalf("walk = %v: %v", paths, err)
			}
		})
	}
}

func TestRejectsSymlinkPaths(t *testing.T) {
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "real"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "real", "file"), []byte("data"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("real", filepath.Join(dir, "alias")); err != nil {
		t.Skip(err)
	}
	if err := os.Symlink("real/file", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	for _, name := range []string{"alias/file", "link", "../file", "/file"} {
		f, err := OpenFile(Local(root), name)
		if err == nil {
			_ = f.Close()
			t.Errorf("accepted %s", name)
		}
	}
}

func TestFileMutation(t *testing.T) {
	for _, mutation := range []string{"replace", "append", "mode", "parent"} {
		t.Run(mutation, func(t *testing.T) {
			dir := t.TempDir()
			nested := filepath.Join(dir, "nested")
			if err := os.Mkdir(nested, 0o700); err != nil {
				t.Fatal(err)
			}
			name := filepath.Join(nested, "file")
			if err := os.WriteFile(name, []byte("content"), 0o600); err != nil {
				t.Fatal(err)
			}
			root, err := os.OpenRoot(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = root.Close() }()
			f, err := OpenFile(Local(root), "nested/file")
			if err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "replace":
				if err := os.Rename(name, name+".old"); err != nil {
					t.Fatal(err)
				}
				err = os.WriteFile(name, []byte("content"), 0o600)
			case "append":
				err = os.WriteFile(name, []byte("longer content"), 0o600)
			case "mode":
				err = os.Chmod(name, 0o400)
			case "parent":
				err = os.Rename(nested, nested+".old")
			}
			if err != nil {
				t.Fatal(err)
			}
			if err := f.Close(); err == nil {
				t.Fatal("accepted changed file")
			}
		})
	}
}

func TestWalkRejectsEntryMutation(t *testing.T) {
	dir := t.TempDir()
	name := filepath.Join(dir, "file")
	if err := os.WriteFile(name, []byte("content"), 0o600); err != nil {
		t.Fatal(err)
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	err = Walk(Local(root), func(_ string, _ fs.ReadLinkFS, _ fs.DirEntry) error {
		return os.WriteFile(name, []byte("changed content"), 0o600)
	})
	if err == nil {
		t.Fatal("accepted mutation during walk")
	}
}

func TestConcurrentWalkWaitsForBorrowers(t *testing.T) {
	dir := t.TempDir()
	nested := filepath.Join(dir, "nested")
	if err := os.Mkdir(nested, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"a", "b"} {
		if err := os.WriteFile(filepath.Join(nested, name), []byte("payload"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = root.Close() }()
	started := make(chan fs.ReadLinkFS, 2)
	release := make(chan struct{})
	result := make(chan error, 1)
	failure := errors.New("verification failed")
	go func() {
		result <- WalkConcurrent(Local(root), func(_ string, parent fs.ReadLinkFS, entry fs.DirEntry) error {
			if entry.IsDir() {
				return nil
			}
			started <- parent
			<-release
			if entry.Name() == "a" {
				return failure
			}
			f, err := OpenFile(parent, entry.Name())
			if err != nil {
				return err
			}
			_, err = io.Copy(io.Discard, f)
			return errors.Join(err, f.Close())
		})
	}()
	var parent fs.ReadLinkFS
	for range 2 {
		select {
		case parent = <-started:
		case <-time.After(5 * time.Second):
			close(release)
			<-result
			t.Fatal("local callbacks did not run concurrently")
		}
	}
	if _, err := parent.Lstat("."); err != nil {
		t.Error("closed borrowed directory:", err)
	}
	close(release)
	if err := <-result; !errors.Is(err, failure) || err.Error() != failure.Error() {
		t.Fatalf("lost callback failure or closed a live borrower: %v", err)
	}
	if _, err := parent.Lstat("."); err == nil {
		t.Fatal("retained directory after walk")
	}
}
