package app

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/crypto/ssh"

	"incus-for-devs/internal/command"
	"incus-for-devs/internal/core"
	"incus-for-devs/internal/gateway"
	"incus-for-devs/internal/incus"
)

func Run(args []string) error {
	if len(args) > 0 {
		switch args[0] {
		case "server":
			return serve(args[1:])
		case "keygen":
			return keygen(args[1:])
		case "version", "--version":
			fmt.Println("incus-for-devs development")
			return nil
		case "--help", "-h":
			fmt.Print(localHelp)
			return nil
		case "client":
			args = args[1:]
		}
	}
	sshArgs, err := clientArgs(args, os.Getenv("INCUS_FOR_DEVS_TARGET"))
	if err != nil {
		return err
	}
	path, err := exec.LookPath("ssh")
	if err != nil {
		return errors.New("client mode requires OpenSSH on PATH")
	}
	return syscall.Exec(path, append([]string{"ssh"}, sshArgs...), os.Environ())
}

func serve(args []string) error {
	flags := flag.NewFlagSet("server", flag.ContinueOnError)
	path := flags.String("config", "", "path to operator-owned JSON configuration")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if *path == "" || flags.NArg() != 0 {
		return errors.New("usage: incus-for-devs server --config /etc/incus-for-devs/config.json")
	}
	cfg, err := LoadConfig(*path)
	if err != nil {
		return err
	}
	backend, err := incus.New(incus.Config{
		Binary: cfg.IncusBinary, Image: cfg.Image, Profile: cfg.Profile,
		Limits: cfg.Limits, OperationTimeout: time.Duration(cfg.OperationTimeoutSeconds) * time.Second,
	})
	if err != nil {
		return err
	}
	users := make([]gateway.AuthorizedUser, 0, len(cfg.Users))
	for _, u := range cfg.Users {
		users = append(users, gateway.AuthorizedUser{
			Principal: core.Principal{Name: u.Name, Project: u.Project}, AuthorizedKeys: u.AuthorizedKeys,
		})
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	listener, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return err
	}
	defer listener.Close()
	fmt.Fprintln(os.Stderr, "private SSH gateway listening on", listener.Addr())
	return gateway.Serve(ctx, listener, gateway.Config{
		HostKeyPath: cfg.HostKey, Users: users, MaxSessions: cfg.MaxSessions,
		Handler: &command.Handler{Backend: backend, Limits: cfg.Limits},
	})
}

func keygen(args []string) error {
	flags := flag.NewFlagSet("keygen", flag.ContinueOnError)
	path := flags.String("path", "", "new persistent server host-key file")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if !filepath.IsAbs(*path) || flags.NArg() != 0 {
		return errors.New("usage: incus-for-devs keygen --path /absolute/path/to/host.key")
	}
	fingerprint, err := generateHostKey(*path)
	if err == nil {
		fmt.Println("Created server host key:", fingerprint)
	}
	return err
}

func generateHostKey(path string) (string, error) {
	pub, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(private)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return "", err
	}
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", err
	}
	writeErr := pem.Encode(f, &pem.Block{Type: "PRIVATE KEY", Bytes: der})
	closeErr := f.Close()
	if writeErr != nil {
		return "", writeErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	key, err := ssh.NewPublicKey(pub)
	if err != nil {
		return "", err
	}
	return ssh.FingerprintSHA256(key), nil
}

func clientArgs(args []string, envTarget string) ([]string, error) {
	flags := flag.NewFlagSet("client", flag.ContinueOnError)
	target := flags.String("target", envTarget, "app-user@host or OpenSSH alias")
	port := flags.Int("port", 0, "SSH port; omitted uses OpenSSH configuration")
	if err := flags.Parse(args); err != nil {
		return nil, err
	}
	if *target == "" || strings.HasPrefix(*target, "-") || strings.ContainsAny(*target, " \t\r\n\x00/\\") {
		return nil, errors.New("set --target user@host or INCUS_FOR_DEVS_TARGET to an OpenSSH alias")
	}
	if *port < 0 || *port > 65535 {
		return nil, errors.New("invalid SSH port")
	}
	result := []string{"-o", "StrictHostKeyChecking=yes", "-o", "ForwardAgent=no", "-o", "ClearAllForwardings=yes"}
	if *port != 0 {
		result = append(result, "-p", strconv.Itoa(*port))
	}
	commandArgs := flags.Args()
	if len(commandArgs) == 0 || (len(commandArgs) == 2 && commandArgs[0] == "ssh") {
		result = append(result, "-t")
	}
	result = append(result, "--", *target)
	if len(commandArgs) > 0 {
		quoted := make([]string, len(commandArgs))
		for i, a := range commandArgs {
			if strings.ContainsRune(a, 0) {
				return nil, errors.New("NUL in command argument")
			}
			quoted[i] = shellQuote(a)
		}
		if len(commandArgs) == 3 && commandArgs[0] == "ssh" {
			// The third argument is a complete guest shell command, not one executable name.
			quoted[2] = commandArgs[2]
		}
		result = append(result, strings.Join(quoted, " "))
	}
	return result, nil
}

func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "'\\''") + "'"
}

const localHelp = `Incus for devs: a private SSH gateway for trusted Linux containers.

  incus-for-devs server --config /etc/incus-for-devs/config.json
  incus-for-devs keygen --path /etc/incus-for-devs/host.key
  incus-for-devs [client] --target user@host --port 2222 help
  incus-for-devs --target your-ssh-alias ls --json

Set INCUS_FOR_DEVS_TARGET instead of repeating --target.
Pin the gateway host key in known_hosts before connecting.
Stock OpenSSH works too: ssh -p 2222 user@host help

An explicit exe.dev-like command subset, not full exe.dev or VM parity.
`
