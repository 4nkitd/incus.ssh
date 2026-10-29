package operator

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"incus-for-devs/internal/host"
)

const maxConfigBytes = 64 << 10

var projectPattern = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,46}[a-z0-9])?$`)

type operatorConfig struct {
	Binary  string
	Project string
}

type configFile interface {
	Stat() (os.FileInfo, error)
	Read([]byte) (int, error)
	Close() error
}

type configFS struct {
	lstat    func(string) (os.FileInfo, error)
	open     func(string) (configFile, error)
	sameFile func(os.FileInfo, os.FileInfo) bool
}

func (files configFS) sameFileInfo(a, b os.FileInfo) bool {
	if files.sameFile != nil {
		return files.sameFile(a, b)
	}
	return os.SameFile(a, b)
}

var (
	configFiles = configFS{
		lstat: os.Lstat,
		open: func(path string) (configFile, error) {
			return os.OpenFile(path, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0)
		},
	}
	trustedDirectory = host.TrustedDirectory
)

func loadOperatorConfig(path string, explicit bool) (operatorConfig, error) {
	return loadOperatorConfigWith(path, explicit, configFiles, trustedDirectory)
}

func loadOperatorConfigWith(path string, explicit bool, files configFS, trustedDir func(string) error) (operatorConfig, error) {
	cfg := operatorConfig{Binary: defaultIncusBinary}
	if path == "" {
		if explicit {
			return cfg, errors.New("--config requires a path")
		}
		return cfg, nil
	}
	path = filepath.Clean(path)
	info, err := files.lstat(path)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return cfg, fmt.Errorf("read operator configuration %s: %w", path, err)
		}
		if explicit {
			return cfg, fmt.Errorf("read operator configuration %s: %w", path, err)
		}
		if err := verifyAbsentConfig(path, files, trustedDir); err != nil {
			return cfg, fmt.Errorf("operator configuration %s is absent but its parent is not trusted: %w", path, err)
		}
		return cfg, nil
	}
	if err := trustedDir(filepath.Dir(filepath.Clean(path))); err != nil {
		return cfg, fmt.Errorf("operator configuration %s has an untrusted parent: %w", path, err)
	}
	if err := verifyConfigInfo(path, info); err != nil {
		return cfg, err
	}
	f, err := files.open(filepath.Clean(path))
	if err != nil {
		return cfg, fmt.Errorf("open operator configuration %s: %w", path, err)
	}
	opened, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return cfg, fmt.Errorf("stat operator configuration %s: %w", path, err)
	}
	if !files.sameFileInfo(info, opened) {
		_ = f.Close()
		return cfg, fmt.Errorf("operator configuration %s changed while it was being read", path)
	}
	if err := verifyConfigInfo(path, opened); err != nil {
		_ = f.Close()
		return cfg, err
	}
	data, err := io.ReadAll(io.LimitReader(f, maxConfigBytes+1))
	closeErr := f.Close()
	if err != nil {
		return cfg, fmt.Errorf("read operator configuration %s: %w", path, err)
	}
	if closeErr != nil {
		return cfg, fmt.Errorf("close operator configuration %s: %w", path, closeErr)
	}
	if len(data) > maxConfigBytes {
		return cfg, fmt.Errorf("operator configuration %s exceeds %d bytes", path, maxConfigBytes)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		return cfg, fmt.Errorf("operator configuration %s is empty", path)
	}
	parsed, err := parseOperatorConfig(data)
	if err != nil {
		return cfg, fmt.Errorf("invalid operator configuration %s: %w", path, err)
	}
	return parsed, nil
}

func verifyConfigInfo(path string, info os.FileInfo) error {
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("operator configuration %s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("operator configuration %s must be a regular file", path)
	}
	if info.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("operator configuration %s must not be writable by group or others", path)
	}
	if info.Size() > maxConfigBytes {
		return fmt.Errorf("operator configuration %s exceeds %d bytes", path, maxConfigBytes)
	}
	uid, ok := rootOwnerUID(info)
	if !ok || uid != 0 {
		return fmt.Errorf("operator configuration %s must be owned by root", path)
	}
	return nil
}

func rootOwnerUID(info os.FileInfo) (uint32, bool) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat == nil {
		return 0, false
	}
	return stat.Uid, true
}

func verifyAbsentConfig(path string, files configFS, trustedDir func(string) error) error {
	if !filepath.IsAbs(path) {
		return errors.New("path must be absolute")
	}
	for dir := filepath.Dir(filepath.Clean(path)); ; dir = filepath.Dir(dir) {
		_, err := files.lstat(dir)
		if err == nil {
			return trustedDir(dir)
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return err
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return err
		}
	}
}

func parseOperatorConfig(data []byte) (operatorConfig, error) {
	if len(bytes.TrimSpace(data)) == 0 {
		return operatorConfig{}, errors.New("configuration is empty")
	}
	if err := validateJSONDocument(data); err != nil {
		return operatorConfig{}, err
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(data, &top); err != nil {
		return operatorConfig{}, fmt.Errorf("decode configuration: %w", err)
	}
	if top == nil {
		return operatorConfig{}, errors.New("configuration must be a JSON object")
	}
	cfg := operatorConfig{Binary: defaultIncusBinary}
	if raw, ok := top["incus_binary"]; ok {
		var binary string
		if err := json.Unmarshal(raw, &binary); err != nil {
			return operatorConfig{}, errors.New("incus_binary must be a string")
		}
		if binary != "" {
			cfg.Binary = binary
		}
	}
	raw, ok := top["users"]
	if !ok {
		return operatorConfig{}, errors.New("configuration has no users; users[0].project is required")
	}
	var users []struct {
		Project string `json:"project"`
	}
	if err := json.Unmarshal(raw, &users); err != nil {
		return operatorConfig{}, errors.New("users must be an array of objects")
	}
	if len(users) == 0 {
		return operatorConfig{}, errors.New("configuration has no users; users[0].project is required")
	}
	cfg.Project = users[0].Project
	if !projectPattern.MatchString(cfg.Project) || cfg.Project == "default" {
		return operatorConfig{}, errors.New("users[0].project must be a valid non-default lowercase project name")
	}
	return cfg, nil
}

func validateJSONDocument(data []byte) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	if tok != json.Delim('{') {
		return errors.New("configuration must be a JSON object")
	}
	if err := scanObject(dec); err != nil {
		return err
	}
	tok, err = dec.Token()
	if err == nil {
		return fmt.Errorf("configuration must contain exactly one JSON object (found %v)", tok)
	}
	if !errors.Is(err, io.EOF) {
		return fmt.Errorf("decode configuration: %w", err)
	}
	return nil
}

func scanValue(dec *json.Decoder) error {
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	delim, ok := tok.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		return scanObject(dec)
	case '[':
		return scanArray(dec)
	default:
		return fmt.Errorf("decode configuration: unexpected delimiter %q", delim)
	}
}

func scanObject(dec *json.Decoder) error {
	seen := map[string]bool{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return fmt.Errorf("decode configuration: %w", err)
		}
		key, ok := tok.(string)
		if !ok {
			return errors.New("configuration object key must be a string")
		}
		if seen[key] {
			return fmt.Errorf("duplicate JSON key %q", key)
		}
		seen[key] = true
		if err := scanValue(dec); err != nil {
			return err
		}
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	if tok != json.Delim('}') {
		return errors.New("configuration has an unterminated JSON object")
	}
	return nil
}

func scanArray(dec *json.Decoder) error {
	for dec.More() {
		if err := scanValue(dec); err != nil {
			return err
		}
	}
	tok, err := dec.Token()
	if err != nil {
		return fmt.Errorf("decode configuration: %w", err)
	}
	if tok != json.Delim(']') {
		return errors.New("configuration has an unterminated JSON array")
	}
	return nil
}
