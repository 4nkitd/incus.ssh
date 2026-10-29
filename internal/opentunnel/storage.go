package opentunnel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

type identity struct {
	ID                string    `json:"id"`
	Hostname          string    `json:"hostname"`
	CertificateExpiry time.Time `json:"certificateExpiry"`
	token             string
	key               []byte
	certificate       []byte
	chain             []byte
	csr               string
}

type store struct {
	dir  string
	lock *os.File
}

func openStore(directory string) (*store, error) {
	if !filepath.IsAbs(directory) || filepath.Clean(directory) != directory {
		return nil, errors.New("OpenTunnel state directory must be an absolute clean path")
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("OpenTunnel state directory must be a private directory, mode 0700")
	}
	lock, err := os.OpenFile(filepath.Join(directory, ".lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		lock.Close()
		return nil, errors.New("OpenTunnel state directory is already in use")
	}
	return &store{dir: directory, lock: lock}, nil
}

func (s *store) close() error { return s.lock.Close() }

func (s *store) read(name string) ([]byte, error) {
	f, err := os.OpenFile(filepath.Join(s.dir, name), os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 1<<20 {
		return nil, fmt.Errorf("unsafe OpenTunnel state file %s", name)
	}
	return io.ReadAll(io.LimitReader(f, (1<<20)+1))
}

func (s *store) write(name string, data []byte) error {
	f, err := os.CreateTemp(s.dir, ".write-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(f.Name(), filepath.Join(s.dir, name)); err != nil {
		return err
	}
	dir, err := os.Open(s.dir)
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func (s *store) load() (*identity, error) {
	data, err := s.read("tunnel.json")
	pending := false
	if errors.Is(err, os.ErrNotExist) {
		data, err = s.read("pending.json")
		pending = true
	}
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var meta struct {
		ID                string    `json:"id"`
		Hostname          string    `json:"hostname"`
		CertificateExpiry time.Time `json:"certificateExpiry"`
		CSR               string    `json:"csr"`
	}
	if json.Unmarshal(data, &meta) != nil || !label.MatchString(meta.ID) || !validHostname(meta.Hostname) {
		return nil, errors.New("invalid OpenTunnel identity metadata")
	}
	value := &identity{ID: meta.ID, Hostname: meta.Hostname, CertificateExpiry: meta.CertificateExpiry, csr: meta.CSR}
	token, err := s.read("token")
	if err != nil {
		return nil, err
	}
	value.token = strings.TrimSpace(string(token))
	if value.token == "" || len(value.token) > 4096 || strings.ContainsAny(value.token, "\x00\r\n\t ") {
		return nil, errors.New("invalid OpenTunnel token file")
	}
	if value.key, err = s.read("private-key.pem"); err != nil {
		return nil, err
	}
	if !pending {
		if value.certificate, err = s.read("certificate.pem"); err != nil {
			return nil, err
		}
		if value.chain, err = s.read("chain.pem"); err != nil {
			return nil, err
		}
	}
	return value, nil
}

func (s *store) savePending(value *identity) error {
	meta, err := json.Marshal(struct {
		ID       string `json:"id"`
		Hostname string `json:"hostname"`
		CSR      string `json:"csr"`
	}{value.ID, value.Hostname, value.csr})
	if err != nil {
		return err
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"token", []byte(value.token)}, {"private-key.pem", value.key}, {"pending.json", meta}} {
		if err := s.write(file.name, file.data); err != nil {
			return err
		}
	}
	return nil
}

func (s *store) save(value *identity) error {
	meta, err := json.Marshal(value)
	if err != nil {
		return err
	}
	for _, file := range []struct {
		name string
		data []byte
	}{{"certificate.pem", value.certificate}, {"chain.pem", value.chain}, {"tunnel.json", meta}} {
		if err := s.write(file.name, file.data); err != nil {
			return err
		}
	}
	if err := os.Remove(filepath.Join(s.dir, "pending.json")); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
