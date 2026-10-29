package host

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

type fakeFileInfo struct {
	name string
	mode os.FileMode
	size int64
	stat syscall.Stat_t
}

func (f fakeFileInfo) Name() string       { return f.name }
func (f fakeFileInfo) Size() int64        { return f.size }
func (f fakeFileInfo) Mode() os.FileMode  { return f.mode }
func (f fakeFileInfo) ModTime() time.Time { return time.Time{} }
func (f fakeFileInfo) IsDir() bool        { return f.mode.IsDir() }
func (f fakeFileInfo) Sys() any           { s := f.stat; return &s }

func ownedInfo(name string, mode os.FileMode, uid uint32) os.FileInfo {
	return fakeFileInfo{name: name, mode: mode, stat: syscall.Stat_t{Uid: uid}}
}

func rootFile(name string, mode os.FileMode) os.FileInfo {
	return ownedInfo(name, mode, 0)
}

func fakeFileSystem(files map[string]os.FileInfo, links map[string]string, stat statFunc, open openFunc) fileSystem {
	return fileSystem{
		lstat: func(path string) (os.FileInfo, error) {
			if info, ok := files[path]; ok {
				return info, nil
			}
			return ownedInfo(filepath.Base(path), os.ModeDir|0o755, 0), nil
		},
		stat: stat,
		readlink: func(path string) (string, error) {
			if target, ok := links[path]; ok {
				return target, nil
			}
			return "", fs.ErrNotExist
		},
		open: open,
	}
}

func trustedDirs(string) (os.FileInfo, error) {
	return ownedInfo("dir", os.ModeDir|0o755, 0), nil
}

func TestTrustedExecutableRequiresAbsolutePath(t *testing.T) {
	if err := TrustedExecutable("usr/bin/incus"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Fatalf("expected absolute-path error, got %v", err)
	}
}

func TestTrustedExecutableRejectsNULPath(t *testing.T) {
	if err := TrustedExecutable("/usr/bin/in\x00cus"); err == nil {
		t.Fatal("expected invalid path error")
	}
}

func TestTrustedExecutableRejectsNonRootOwner(t *testing.T) {
	fsys := fakeFileSystem(
		map[string]os.FileInfo{"/usr/bin/incus": ownedInfo("incus", 0o755, 1000)},
		nil,
		trustedDirs,
		nil,
	)
	err := trustedExecutable("/usr/bin/incus", fsys)
	if err == nil || !strings.Contains(err.Error(), "owned by root") {
		t.Fatalf("expected root-owner error, got %v", err)
	}
}

func TestTrustedExecutableAcceptsFakeRootOwnedFile(t *testing.T) {
	fsys := fakeFileSystem(
		map[string]os.FileInfo{"/usr/bin/incus": rootFile("incus", 0o755)},
		nil,
		trustedDirs,
		nil,
	)
	if err := trustedExecutable("/usr/bin/incus", fsys); err != nil {
		t.Fatalf("expected trusted executable, got %v", err)
	}
}

func TestTrustedExecutableRejectsUntrustedFileModes(t *testing.T) {
	tests := []struct {
		name string
		info os.FileInfo
		want string
	}{
		{"not executable", rootFile("incus", 0o644), "executable"},
		{"group writable", rootFile("incus", 0o775), "writable"},
		{"world writable", rootFile("incus", 0o757), "writable"},
		{"directory", ownedInfo("incus", os.ModeDir|0o755, 0), "regular file"},
		{"named pipe", ownedInfo("incus", os.ModeNamedPipe|0o755, 0), "regular file"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fsys := fakeFileSystem(
				map[string]os.FileInfo{"/usr/bin/incus": test.info},
				nil,
				trustedDirs,
				nil,
			)
			err := trustedExecutable("/usr/bin/incus", fsys)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestTrustedExecutableRejectsUntrustedAncestor(t *testing.T) {
	tests := []struct {
		name string
		dir  os.FileInfo
		want string
	}{
		{"non-root owner", ownedInfo("bin", os.ModeDir|0o755, 1000), "owned by root"},
		{"group writable", ownedInfo("bin", os.ModeDir|0o775, 0), "writable"},
		{"not a directory", rootFile("bin", 0o755), "not a directory"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			stat := func(path string) (os.FileInfo, error) {
				if path == "/usr/bin" {
					return test.dir, nil
				}
				return trustedDirs(path)
			}
			fsys := fakeFileSystem(
				map[string]os.FileInfo{"/usr/bin/incus": rootFile("incus", 0o755)},
				nil,
				stat,
				nil,
			)
			err := trustedExecutable("/usr/bin/incus", fsys)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestTrustedExecutableResolvesSymlinkChain(t *testing.T) {
	fsys := fakeFileSystem(
		map[string]os.FileInfo{
			"/usr/bin/incus":       ownedInfo("incus", os.ModeSymlink|0o777, 1000),
			"/usr/lib/incus/incus": ownedInfo("incus", os.ModeSymlink|0o777, 1000),
			"/opt/incus/bin/incus": rootFile("incus", 0o755),
		},
		map[string]string{
			"/usr/bin/incus":       "../lib/incus/incus",
			"/usr/lib/incus/incus": "/opt/incus/bin/incus",
		},
		trustedDirs,
		nil,
	)
	if err := trustedExecutable("/usr/bin/incus", fsys); err != nil {
		t.Fatalf("expected trusted symlink chain, got %v", err)
	}
}

func TestTrustedExecutableRejectsSymlinkLoop(t *testing.T) {
	fsys := fakeFileSystem(
		map[string]os.FileInfo{
			"/usr/bin/a": ownedInfo("a", os.ModeSymlink|0o777, 0),
			"/usr/bin/b": ownedInfo("b", os.ModeSymlink|0o777, 0),
		},
		map[string]string{"/usr/bin/a": "b", "/usr/bin/b": "a"},
		trustedDirs,
		nil,
	)
	err := trustedExecutable("/usr/bin/a", fsys)
	if err == nil || !strings.Contains(err.Error(), "loop") {
		t.Fatalf("expected symlink-loop error, got %v", err)
	}
}

func TestTrustedExecutableRejectsUntrustedSymlinkTarget(t *testing.T) {
	stat := func(path string) (os.FileInfo, error) {
		if path == "/tmp" {
			return ownedInfo("tmp", os.ModeDir|0o755, 1000), nil
		}
		return trustedDirs(path)
	}
	fsys := fakeFileSystem(
		map[string]os.FileInfo{
			"/usr/bin/incus": ownedInfo("incus", os.ModeSymlink|0o777, 0),
			"/tmp/payload":   rootFile("payload", 0o755),
		},
		map[string]string{"/usr/bin/incus": "/tmp/payload"},
		stat,
		nil,
	)
	err := trustedExecutable("/usr/bin/incus", fsys)
	if err == nil || !strings.Contains(err.Error(), "owned by root") {
		t.Fatalf("expected untrusted target ancestor error, got %v", err)
	}
}

func TestTrustedExecutableRejectsUntrustedDirectorySymlinkAncestor(t *testing.T) {
	stat := func(path string) (os.FileInfo, error) {
		if path == "/mnt" {
			return ownedInfo("mnt", os.ModeDir|0o755, 1000), nil
		}
		return trustedDirs(path)
	}
	fsys := fakeFileSystem(
		map[string]os.FileInfo{
			"/usr/bin":       ownedInfo("bin", os.ModeSymlink|0o777, 0),
			"/usr/bin/incus": rootFile("incus", 0o755),
		},
		map[string]string{"/usr/bin": "/mnt/bin"},
		stat,
		nil,
	)
	err := trustedExecutable("/usr/bin/incus", fsys)
	if err == nil || !strings.Contains(err.Error(), "owned by root") {
		t.Fatalf("expected untrusted directory symlink ancestor error, got %v", err)
	}
}

func TestTrustedExecutableRejectsTooManySymlinks(t *testing.T) {
	files := map[string]os.FileInfo{}
	links := map[string]string{}
	for i := 0; i < maxSymlinkDepth+2; i++ {
		name := "/usr/bin/link" + string(rune('a'+i))
		files[name] = ownedInfo(name, os.ModeSymlink|0o777, 0)
		links[name] = "/usr/bin/link" + string(rune('a'+i+1))
	}
	fsys := fakeFileSystem(files, links, trustedDirs, nil)
	err := trustedExecutable("/usr/bin/linka", fsys)
	if err == nil || !strings.Contains(err.Error(), "too many symlinks") {
		t.Fatalf("expected too-many-symlinks error, got %v", err)
	}
}

func TestTrustedExecutableDetectsReplacedFile(t *testing.T) {
	dir := t.TempDir()
	original := filepath.Join(dir, "original")
	replacement := filepath.Join(dir, "replacement")
	for _, path := range []string{original, replacement} {
		if err := os.WriteFile(path, []byte("placeholder"), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	info, err := os.Lstat(original)
	if err != nil {
		t.Fatal(err)
	}
	stat := *info.Sys().(*syscall.Stat_t)
	stat.Uid = 0
	fake := fakeFileInfo{name: info.Name(), mode: info.Mode(), size: info.Size(), stat: stat}
	f, err := os.Open(replacement)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	fsys := fakeFileSystem(
		map[string]os.FileInfo{"/usr/bin/incus": fake},
		nil,
		trustedDirs,
		func(string) (*os.File, error) { return f, nil },
	)
	err = trustedExecutable("/usr/bin/incus", fsys)
	if err == nil || !strings.Contains(err.Error(), "changed") {
		t.Fatalf("expected changed-file error, got %v", err)
	}
}

func TestTrustedDirectoryAcceptsRootOwnedChain(t *testing.T) {
	fsys := fakeFileSystem(nil, nil, trustedDirs, nil)
	if err := checkAncestorPath("/etc/incus-for-devs", fsys, 0, map[string]bool{}); err != nil {
		t.Fatalf("expected trusted directory chain, got %v", err)
	}
}

func TestTrustedDirectoryRejectsUnsafeAncestors(t *testing.T) {
	tests := []struct {
		name string
		stat statFunc
		want string
	}{
		{
			name: "group writable directory",
			stat: func(path string) (os.FileInfo, error) {
				if path == "/etc/incus-for-devs" {
					return ownedInfo("incus-for-devs", os.ModeDir|0o775, 0), nil
				}
				return trustedDirs(path)
			},
			want: "writable",
		},
		{
			name: "non-root original ancestor",
			stat: func(path string) (os.FileInfo, error) {
				if path == "/etc" {
					return ownedInfo("etc", os.ModeDir|0o755, 1000), nil
				}
				return trustedDirs(path)
			},
			want: "owned by root",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			fsys := fakeFileSystem(nil, nil, test.stat, nil)
			err := checkAncestorPath("/etc/incus-for-devs", fsys, 0, map[string]bool{})
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("expected %q error, got %v", test.want, err)
			}
		})
	}
}

func TestTrustedDirectoryRejectsUnsafeSymlinkTarget(t *testing.T) {
	stat := func(path string) (os.FileInfo, error) {
		if path == "/mnt" {
			return ownedInfo("mnt", os.ModeDir|0o755, 1000), nil
		}
		return trustedDirs(path)
	}
	fsys := fakeFileSystem(
		map[string]os.FileInfo{
			"/etc/incus-for-devs": ownedInfo("incus-for-devs", os.ModeSymlink|0o777, 0),
		},
		map[string]string{"/etc/incus-for-devs": "/mnt/ifd"},
		stat,
		nil,
	)
	err := checkAncestorPath("/etc/incus-for-devs", fsys, 0, map[string]bool{})
	if err == nil || !strings.Contains(err.Error(), "owned by root") {
		t.Fatalf("expected unsafe symlink target error, got %v", err)
	}
}

func TestTrustedDirectoryRejectsDanglingSymlink(t *testing.T) {
	stat := func(path string) (os.FileInfo, error) {
		if path == "/etc/incus-for-devs" {
			return ownedInfo("incus-for-devs", os.ModeDir|0o755, 0), nil
		}
		return nil, fs.ErrNotExist
	}
	fsys := fakeFileSystem(
		map[string]os.FileInfo{
			"/etc/incus-for-devs": ownedInfo("incus-for-devs", os.ModeSymlink|0o777, 0),
		},
		map[string]string{"/etc/incus-for-devs": "/missing-ifd"},
		stat,
		nil,
	)
	err := checkAncestorPath("/etc/incus-for-devs", fsys, 0, map[string]bool{})
	if err == nil {
		t.Fatal("expected dangling symlink error")
	}
}

func TestOwnerUIDRequiresUnixStat(t *testing.T) {
	if _, ok := ownerUID(ownedInfo("x", 0o755, 0)); !ok {
		t.Fatal("expected fake unix stat to provide an owner")
	}
	if _, ok := ownerUID(nonUnixInfo{}); ok {
		t.Fatal("expected missing unix stat to be rejected")
	}
}

type nonUnixInfo struct{}

func (nonUnixInfo) Name() string       { return "x" }
func (nonUnixInfo) Size() int64        { return 0 }
func (nonUnixInfo) Mode() os.FileMode  { return 0 }
func (nonUnixInfo) ModTime() time.Time { return time.Time{} }
func (nonUnixInfo) IsDir() bool        { return false }
func (nonUnixInfo) Sys() any           { return "not a unix stat" }
