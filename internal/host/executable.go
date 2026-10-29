// Package host verifies host-level files before a root-run process executes
// them.
package host

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

const maxSymlinkDepth = 40

type statFunc func(string) (os.FileInfo, error)
type readlinkFunc func(string) (string, error)
type openFunc func(string) (*os.File, error)

type fileSystem struct {
	lstat    statFunc
	stat     statFunc
	readlink readlinkFunc
	open     openFunc
}

func systemFileSystem() fileSystem {
	return fileSystem{
		lstat:    os.Lstat,
		stat:     os.Stat,
		readlink: os.Readlink,
		open: func(path string) (*os.File, error) {
			return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		},
	}
}

// TrustedExecutable verifies that path resolves to a regular executable owned
// by root, with no group- or world-writable ancestors. A symlink is allowed
// only when every link in the chain resolves to a trusted location. The final
// file is reopened without following symlinks and rechecked to narrow the
// window between verification and execution.
func TrustedExecutable(path string) error {
	return trustedExecutable(path, systemFileSystem())
}

// TrustedDirectory verifies that path is a directory owned by root with no
// group- or world-writable ancestors. Directory symlinks are followed only when
// their resolved target and the target's ancestors pass the same checks.
func TrustedDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}
	if strings.ContainsRune(path, 0) || strings.ContainsAny(path, "\r\n") {
		return errors.New("path contains invalid characters")
	}
	return checkAncestorPath(filepath.Clean(path), systemFileSystem(), 0, map[string]bool{})
}

func trustedExecutable(path string, fs fileSystem) error {
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}
	if strings.ContainsRune(path, 0) || strings.ContainsAny(path, "\r\n") {
		return errors.New("path contains invalid characters")
	}
	resolved, info, err := resolveExecutable(filepath.Clean(path), fs)
	if err != nil {
		return err
	}
	if err := verifyExecutableInfo(resolved, info); err != nil {
		return err
	}
	if fs.open == nil {
		return nil
	}
	f, err := fs.open(resolved)
	if err != nil {
		return fmt.Errorf("open verified executable %s: %w", resolved, err)
	}
	opened, statErr := f.Stat()
	closeErr := f.Close()
	if statErr != nil {
		return fmt.Errorf("stat verified executable %s: %w", resolved, statErr)
	}
	if !os.SameFile(info, opened) {
		return fmt.Errorf("executable %s changed while it was being verified", resolved)
	}
	if err := verifyExecutableInfo(resolved, opened); err != nil {
		return err
	}
	if closeErr != nil {
		return fmt.Errorf("close verified executable %s: %w", resolved, closeErr)
	}
	return nil
}

func resolveExecutable(path string, fs fileSystem) (string, os.FileInfo, error) {
	current := path
	seen := map[string]bool{}
	for depth := 0; depth <= maxSymlinkDepth; depth++ {
		if err := checkAncestors(current, fs); err != nil {
			return "", nil, err
		}
		info, err := fs.lstat(current)
		if err != nil {
			return "", nil, err
		}
		if info.Mode()&os.ModeSymlink == 0 {
			return current, info, nil
		}
		if depth == maxSymlinkDepth {
			return "", nil, fmt.Errorf("executable %s has too many symlinks", path)
		}
		target, err := fs.readlink(current)
		if err != nil {
			return "", nil, fmt.Errorf("read symlink %s: %w", current, err)
		}
		if !filepath.IsAbs(target) {
			target = filepath.Join(filepath.Dir(current), target)
		}
		target = filepath.Clean(target)
		if !filepath.IsAbs(target) {
			return "", nil, fmt.Errorf("symlink %s has a non-absolute target", current)
		}
		if seen[target] {
			return "", nil, fmt.Errorf("executable %s has a symlink loop", path)
		}
		seen[target] = true
		current = target
	}
	return "", nil, fmt.Errorf("executable %s has too many symlinks", path)
}

func checkAncestors(path string, fs fileSystem) error {
	return checkAncestorPath(filepath.Dir(path), fs, 0, map[string]bool{})
}

func checkAncestorPath(dir string, fs fileSystem, depth int, seen map[string]bool) error {
	if depth > maxSymlinkDepth {
		return fmt.Errorf("directory %s has too many symlinks", dir)
	}
	for {
		info, err := fs.stat(dir)
		if err != nil {
			return fmt.Errorf("directory %s: %w", dir, err)
		}
		if !info.IsDir() {
			return fmt.Errorf("%s is not a directory", dir)
		}
		if info.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("directory %s must not be writable by group or others", dir)
		}
		uid, ok := ownerUID(info)
		if !ok || uid != 0 {
			return fmt.Errorf("directory %s must be owned by root", dir)
		}
		linkInfo, err := fs.lstat(dir)
		if err != nil {
			return fmt.Errorf("directory %s: %w", dir, err)
		}
		if linkInfo.Mode()&os.ModeSymlink != 0 {
			target, err := fs.readlink(dir)
			if err != nil {
				return fmt.Errorf("read symlink %s: %w", dir, err)
			}
			if !filepath.IsAbs(target) {
				target = filepath.Join(filepath.Dir(dir), target)
			}
			target = filepath.Clean(target)
			if !filepath.IsAbs(target) {
				return fmt.Errorf("symlink %s has a non-absolute target", dir)
			}
			if seen[target] {
				return fmt.Errorf("directory %s has a symlink loop", dir)
			}
			seen[target] = true
			err = checkAncestorPath(target, fs, depth+1, seen)
			delete(seen, target)
			if err != nil {
				return err
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return nil
		}
		dir = parent
	}
}

func verifyExecutableInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", path)
	}
	if info.Mode().Perm()&0o111 == 0 {
		return fmt.Errorf("%s must be executable", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s must not be writable by group or others", path)
	}
	uid, ok := ownerUID(info)
	if !ok || uid != 0 {
		return fmt.Errorf("%s must be owned by root", path)
	}
	return nil
}

func ownerUID(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, false
	}
	return stat.Uid, true
}
