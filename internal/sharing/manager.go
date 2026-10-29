package sharing

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"

	"incus-for-devs/internal/core"
	"incus-for-devs/internal/opentunnel"
)

const maxShares = 16

var namePattern = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

type Options struct {
	API       string
	Directory string
}

type key struct {
	principal core.Principal
	name      string
	port      int
}

type machine struct {
	principal core.Principal
	name      string
}

type entry struct {
	id         string
	route      string
	cancel     context.CancelFunc
	connection *opentunnel.Connection
}

type Manager struct {
	ctx      context.Context
	cancel   context.CancelFunc
	backend  core.ServiceBackend
	options  Options
	mu       sync.Mutex
	closed   bool
	shares   map[key]*entry
	clients  map[core.Principal]*opentunnel.Client
	changing map[machine]bool
	workers  sync.WaitGroup
}

var _ core.Sharing = (*Manager)(nil)

func New(ctx context.Context, backend core.ServiceBackend, options Options) *Manager {
	ctx, cancel := context.WithCancel(ctx)
	return &Manager{ctx: ctx, cancel: cancel, backend: backend, options: options, shares: make(map[key]*entry), clients: make(map[core.Principal]*opentunnel.Client), changing: make(map[machine]bool)}
}

func validate(p core.Principal, name string, port int) error {
	if !namePattern.MatchString(p.Name) || !namePattern.MatchString(p.Project) || !namePattern.MatchString(name) || port < 1 || port > 65535 {
		return errors.New("sharing requires an authenticated user, one container name and one port from 1 to 65535")
	}
	return nil
}

func (m *Manager) Enable(ctx context.Context, p core.Principal, name string, port int) (core.Share, error) {
	if err := validate(p, name, port); err != nil {
		return core.Share{}, err
	}
	ctx, timeout := context.WithTimeout(ctx, 2*time.Minute)
	defer timeout()
	stopManager := context.AfterFunc(m.ctx, timeout)
	defer stopManager()
	if err := ctx.Err(); err != nil {
		return core.Share{}, err
	}
	k := key{p, name, port}
	m.mu.Lock()
	if m.closed || m.changing[machine{p, name}] {
		m.mu.Unlock()
		return core.Share{}, errors.New("sharing is shutting down or the machine lifecycle is changing")
	}
	id := ""
	if old := m.shares[k]; old != nil {
		if old.connection == nil {
			m.mu.Unlock()
			return core.Share{}, errors.New("this machine and port is already being enabled")
		}
		m.mu.Unlock()
		var err error
		id, err = m.backend.ServiceIdentity(ctx, p, name)
		if err != nil {
			return core.Share{}, err
		}
		m.mu.Lock()
		if m.shares[k] != old || m.closed || m.changing[machine{p, name}] {
			m.mu.Unlock()
			return core.Share{}, errors.New("sharing was revoked while enabling")
		}
		if old.id == id {
			state, _ := old.connection.Status()
			if state != "stopped" {
				share := core.Share{Name: name, Port: port, URL: old.connection.URL(old.route), Status: state}
				m.mu.Unlock()
				return share, nil
			}
		}
		old.cancel()
		delete(m.shares, k)
	}
	if len(m.shares) >= maxShares {
		m.mu.Unlock()
		return core.Share{}, fmt.Errorf("at most %d individual ports may be published", maxShares)
	}
	var random [12]byte
	if _, err := rand.Read(random[:]); err != nil {
		m.mu.Unlock()
		return core.Share{}, err
	}
	runCtx, cancel := context.WithCancel(m.ctx)
	item := &entry{id: id, route: "p-" + hex.EncodeToString(random[:]), cancel: cancel}
	m.shares[k] = item
	m.workers.Add(1)
	m.mu.Unlock()
	defer m.workers.Done()
	stopRequest := context.AfterFunc(ctx, cancel)
	defer stopRequest()
	var err error
	if id == "" {
		id, err = m.backend.ServiceIdentity(runCtx, p, name)
	}
	m.mu.Lock()
	if err == nil && (m.shares[k] != item || m.closed || m.changing[machine{p, name}] || runCtx.Err() != nil) {
		err = errors.New("sharing was revoked while enabling")
	}
	client := m.clients[p]
	if err == nil && client == nil {
		digest := sha256.Sum256([]byte(p.Project + "\x00" + p.Name))
		client, err = opentunnel.New(opentunnel.Options{API: m.options.API, Directory: filepath.Join(m.options.Directory, hex.EncodeToString(digest[:16]))})
		if err == nil {
			m.clients[p] = client
		}
	}
	item.id = id
	m.mu.Unlock()
	dial := func(ctx context.Context) (net.Conn, error) {
		connection, err := m.backend.DialService(ctx, p, name, id, port)
		if err != nil {
			current, checkErr := m.backend.ServiceIdentity(ctx, p, name)
			if checkErr != nil || current != id {
				m.mu.Lock()
				if m.shares[k] == item {
					delete(m.shares, k)
					item.cancel()
				}
				m.mu.Unlock()
			}
		}
		return connection, err
	}
	var connection *opentunnel.Connection
	if err == nil {
		connection, err = client.Connect(runCtx, opentunnel.Routes{item.route: dial})
	}
	if err == nil {
		err = connection.Ready(ctx)
	}
	if err == nil {
		var current string
		current, err = m.backend.ServiceIdentity(ctx, p, name)
		if err == nil && current != id {
			err = errors.New("container changed while enabling; run share enable again")
		}
	}
	if err == nil && (!stopRequest() || ctx.Err() != nil) {
		err = ctx.Err()
		if err == nil {
			err = context.Canceled
		}
	}
	m.mu.Lock()
	if err == nil && (m.closed || m.shares[k] != item || runCtx.Err() != nil) {
		err = errors.New("sharing was revoked while enabling")
	}
	if err != nil {
		if m.shares[k] == item {
			delete(m.shares, k)
		}
		m.mu.Unlock()
		cancel()
		if connection != nil {
			connection.Close()
		}
		return core.Share{}, err
	}
	item.connection = connection
	m.mu.Unlock()
	return core.Share{Name: name, Port: port, URL: connection.URL(item.route), Status: "connected"}, nil
}

func (m *Manager) Disable(ctx context.Context, p core.Principal, name string, port int) error {
	if err := validate(p, name, port); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	m.revoke(func(k key) bool { return k == (key{p, name, port}) })
	return nil
}

func (m *Manager) Revoke(p core.Principal, name string) {
	m.revoke(func(k key) bool { return k.principal == p && k.name == name })
}

func (m *Manager) ChangeMachine(ctx context.Context, p core.Principal, name string, apply func() error) error {
	if !namePattern.MatchString(p.Name) || !namePattern.MatchString(p.Project) || !namePattern.MatchString(name) {
		return errors.New("invalid machine lifecycle scope")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	k := machine{p, name}
	m.mu.Lock()
	if m.closed || m.changing[k] {
		m.mu.Unlock()
		return errors.New("machine lifecycle is already changing or sharing is shutting down")
	}
	m.changing[k] = true
	m.mu.Unlock()
	defer func() { m.mu.Lock(); delete(m.changing, k); m.mu.Unlock() }()
	m.Revoke(p, name)
	return apply()
}

func (m *Manager) revoke(match func(key) bool) {
	m.mu.Lock()
	var connections []*opentunnel.Connection
	for k, item := range m.shares {
		if match(k) {
			delete(m.shares, k)
			item.cancel()
			if item.connection != nil {
				connections = append(connections, item.connection)
			}
		}
	}
	m.mu.Unlock()
	for _, connection := range connections {
		connection.Close()
	}
}

func (m *Manager) List(ctx context.Context, p core.Principal, name string) ([]core.Share, error) {
	if !namePattern.MatchString(p.Name) || !namePattern.MatchString(p.Project) || (name != "" && !namePattern.MatchString(name)) {
		return nil, errors.New("invalid sharing scope")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	result := make([]core.Share, 0)
	for k, item := range m.shares {
		if k.principal != p || (name != "" && k.name != name) {
			continue
		}
		share := core.Share{Name: k.name, Port: k.port, Status: "enabling"}
		if item.connection != nil {
			share.URL = item.connection.URL(item.route)
			share.Status, _ = item.connection.Status()
		}
		result = append(result, share)
	}
	sort.Slice(result, func(i, j int) bool {
		if result[i].Name != result[j].Name {
			return result[i].Name < result[j].Name
		}
		return result[i].Port < result[j].Port
	})
	return result, nil
}

func (m *Manager) Close() {
	m.mu.Lock()
	m.closed = true
	m.cancel()
	m.mu.Unlock()
	m.workers.Wait()
	m.revoke(func(key) bool { return true })
	for _, client := range m.clients {
		client.Close()
	}
}
