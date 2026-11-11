package controlapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"

	"incus-for-devs/internal/core"
)

type tokenState struct {
	Principal core.Principal `json:"principal"`
	Hash      []byte         `json:"hash,omitempty"`
}

type Tokens struct {
	activity *activityStore
	mu       sync.Mutex
	path     string
	origin   string
	state    tokenState
	epoch    context.Context
	cancel   context.CancelFunc
}

func OpenTokens(directory, origin string, principal core.Principal) (*Tokens, error) {
	if err := os.MkdirAll(directory, 0700); err != nil {
		return nil, err
	}
	info, err := os.Lstat(directory)
	if err != nil || !info.IsDir() || info.Mode().Perm()&0077 != 0 {
		return nil, errors.New("API state directory must be a private directory with mode 0700")
	}
	t := &Tokens{path: filepath.Join(directory, "token.json"), origin: origin, state: tokenState{Principal: principal}}
	info, err = os.Lstat(t.path)
	if err == nil {
		if !info.Mode().IsRegular() || info.Mode().Perm()&0077 != 0 || info.Size() > 4096 {
			return nil, errors.New("API token state must be a private regular file smaller than 4 KiB")
		}
		data, err := os.ReadFile(t.path)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(data, &t.state); err != nil || t.state.Principal != principal || (len(t.state.Hash) != 0 && len(t.state.Hash) != sha256.Size) {
			return nil, errors.New("invalid API token state or principal changed; restore the matching state/configuration")
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	t.epoch, t.cancel = context.WithCancel(context.Background())
	t.activity, err = openActivity(directory, principal)
	if err != nil {
		t.cancel()
		return nil, err
	}
	return t, nil
}

func (t *Tokens) Close() {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.cancel()
}

func (t *Tokens) Manage(p core.Principal, action string) (core.APIStatus, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	status := core.APIStatus{Enabled: len(t.state.Hash) != 0, REST: t.origin + "/api/v1", MCP: t.origin + "/mcp"}
	if p != t.state.Principal {
		return core.APIStatus{}, errors.New("API access belongs to a different principal")
	}
	if action == "status" {
		return status, nil
	}
	if action == "enable" && status.Enabled {
		return status, errors.New("API is already enabled; use api rotate if you need a new token")
	}
	if action == "rotate" && !status.Enabled {
		return status, errors.New("API is disabled; use api enable")
	}
	next := tokenState{Principal: p}
	switch action {
	case "enable", "rotate":
		var secret [32]byte
		if _, err := rand.Read(secret[:]); err != nil {
			return status, err
		}
		status.Token = "ifd_" + base64.RawURLEncoding.EncodeToString(secret[:])
		hash := sha256.Sum256([]byte(status.Token))
		next.Hash = hash[:]
	case "disable":
	default:
		return status, errors.New("unknown API access action")
	}
	if err := t.persist(next); err != nil {
		return core.APIStatus{}, err
	}
	t.cancel()
	t.epoch, t.cancel = context.WithCancel(context.Background())
	t.state = next
	status.Enabled = len(next.Hash) != 0
	return status, nil
}

func (t *Tokens) authenticate(token string) (core.Principal, context.Context, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	hash := sha256.Sum256([]byte(token))
	if len(token) != 47 || len(t.state.Hash) != sha256.Size || subtle.ConstantTimeCompare(hash[:], t.state.Hash) != 1 || t.epoch.Err() != nil {
		return core.Principal{}, nil, false
	}
	return t.state.Principal, t.epoch, true
}

func (t *Tokens) persist(state tokenState) error {
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(t.path), ".token-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return os.Rename(f.Name(), t.path)
}
