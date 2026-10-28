package gateway

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/core"
)

type AuthorizedUser struct {
	Principal      core.Principal
	AuthorizedKeys []string
}

type Config struct {
	HostKeyPath string
	Users       []AuthorizedUser
	Handler     core.Handler
	MaxSessions int
}

const handshakeTimeout = 10 * time.Second
const maxHostKeySize = 64 * 1024

func serverConfig(cfg Config) (*ssh.ServerConfig, map[string]core.Principal, int, error) {
	if cfg.Handler == nil || cfg.HostKeyPath == "" || len(cfg.Users) == 0 {
		return nil, nil, 0, errors.New("host key, handler, and authorized users are required")
	}
	limit := cfg.MaxSessions
	if limit == 0 {
		limit = 64
	}
	if limit < 1 || limit > 4096 {
		return nil, nil, 0, errors.New("max sessions must be between 1 and 4096")
	}
	principals := make(map[string]core.Principal)
	projects := make(map[string]bool)
	keys := make(map[string]map[string]bool)
	owners := make(map[string]string)
	for _, user := range cfg.Users {
		p := user.Principal
		if !validIdentity(p.Name) || !validIdentity(p.Project) || len(user.AuthorizedKeys) == 0 {
			return nil, nil, 0, errors.New("each user requires a valid name, project, and public key")
		}
		if _, exists := principals[p.Name]; exists {
			return nil, nil, 0, fmt.Errorf("duplicate user %q", p.Name)
		}
		if projects[p.Project] {
			return nil, nil, 0, fmt.Errorf("duplicate project %q", p.Project)
		}
		principals[p.Name], projects[p.Project] = p, true
		keys[p.Name] = make(map[string]bool)
		for _, text := range user.AuthorizedKeys {
			text = strings.TrimSpace(text)
			if strings.ContainsAny(text, "\r\n") {
				return nil, nil, 0, fmt.Errorf("authorized key for %q must contain exactly one key line", p.Name)
			}
			key, _, options, rest, err := ssh.ParseAuthorizedKey([]byte(text))
			if err != nil || len(options) != 0 || len(bytes.TrimSpace(rest)) != 0 {
				return nil, nil, 0, fmt.Errorf("invalid or restricted authorized key for %q", p.Name)
			}
			if _, certificate := key.(*ssh.Certificate); certificate {
				return nil, nil, 0, fmt.Errorf("SSH certificates are unsupported for %q", p.Name)
			}
			encoded := string(key.Marshal())
			if owner, exists := owners[encoded]; exists {
				return nil, nil, 0, fmt.Errorf("duplicate authorized key for %q and %q", owner, p.Name)
			}
			owners[encoded], keys[p.Name][encoded] = p.Name, true
		}
	}
	data, err := readHostKey(cfg.HostKeyPath)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("read SSH host key: %w", err)
	}
	signer, err := ssh.ParsePrivateKey(data)
	if err != nil {
		return nil, nil, 0, fmt.Errorf("parse SSH host key: %w", err)
	}
	config := &ssh.ServerConfig{
		MaxAuthTries: 3,
		PublicKeyCallback: func(conn ssh.ConnMetadata, key ssh.PublicKey) (*ssh.Permissions, error) {
			if !keys[conn.User()][string(key.Marshal())] {
				return nil, errors.New("public key authentication denied")
			}
			return &ssh.Permissions{}, nil
		},
	}
	config.AddHostKey(signer)
	return config, principals, limit, nil
}

func readHostKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if err := validateHostKeyFile(info); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !os.SameFile(info, opened) {
		return nil, errors.New("SSH host key changed while opening")
	}
	if err := validateHostKeyFile(opened); err != nil {
		return nil, err
	}
	data, err := io.ReadAll(io.LimitReader(file, maxHostKeySize+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxHostKeySize {
		return nil, errors.New("SSH host key exceeds 64 KiB")
	}
	return data, nil
}

func validateHostKeyFile(info os.FileInfo) error {
	if !info.Mode().IsRegular() {
		return errors.New("SSH host key must be a regular file")
	}
	if info.Mode().Perm()&0077 != 0 {
		return errors.New("SSH host key must not have group or other permissions; use mode 0600")
	}
	if info.Size() > maxHostKeySize {
		return errors.New("SSH host key exceeds 64 KiB")
	}
	return nil
}

func validIdentity(value string) bool {
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-", c)) {
			return false
		}
	}
	return true
}

func Serve(ctx context.Context, listener net.Listener, cfg Config) error {
	if listener == nil {
		return errors.New("listener is required")
	}
	sshConfig, principals, limit, err := serverConfig(cfg)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	connections := make(chan struct{}, limit*2)
	sessions := make(chan struct{}, limit)
	var mu sync.Mutex
	active := make(map[net.Conn]struct{})
	var workers sync.WaitGroup
	stop := context.AfterFunc(ctx, func() {
		listener.Close()
		mu.Lock()
		defer mu.Unlock()
		for conn := range active {
			conn.Close()
		}
	})
	defer func() {
		cancel()
		listener.Close()
		mu.Lock()
		for conn := range active {
			conn.Close()
		}
		mu.Unlock()
		workers.Wait()
		stop()
	}()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
			return err
		}
		select {
		case connections <- struct{}{}:
		default:
			conn.Close()
			continue
		}
		mu.Lock()
		if ctx.Err() != nil {
			mu.Unlock()
			conn.Close()
			<-connections
			return nil
		}
		active[conn] = struct{}{}
		mu.Unlock()
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() {
				conn.Close()
				mu.Lock()
				delete(active, conn)
				mu.Unlock()
				<-connections
			}()
			serveConnection(ctx, conn, sshConfig, principals, cfg.Handler, sessions)
		}()
	}
}

func serveConnection(ctx context.Context, raw net.Conn, config *ssh.ServerConfig, principals map[string]core.Principal, handler core.Handler, slots chan struct{}) {
	if err := raw.SetDeadline(time.Now().Add(handshakeTimeout)); err != nil {
		return
	}
	conn, channels, requests, err := ssh.NewServerConn(raw, config)
	if err != nil {
		return
	}
	defer conn.Close()
	if err := raw.SetDeadline(time.Time{}); err != nil {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		conn.Wait()
		cancel()
	}()
	go func() {
		defer workers.Done()
		for request := range requests {
			request.Reply(request.Type == "keepalive@openssh.com" && len(request.Payload) == 0, nil)
		}
	}()
	defer func() {
		cancel()
		conn.Close()
		workers.Wait()
	}()
	for incoming := range channels {
		if incoming.ChannelType() != "session" || len(incoming.ExtraData()) != 0 {
			incoming.Reject(ssh.UnknownChannelType, "only session channels are supported")
			continue
		}
		select {
		case slots <- struct{}{}:
		default:
			incoming.Reject(ssh.ResourceShortage, "session limit reached")
			continue
		}
		channel, reqs, err := incoming.Accept()
		if err != nil {
			<-slots
			continue
		}
		workers.Add(1)
		go func() {
			defer workers.Done()
			defer func() { <-slots }()
			serveSession(ctx, channel, reqs, principals[conn.User()], handler, conn.Close)
		}()
	}
}
