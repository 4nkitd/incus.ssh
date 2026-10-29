package core

import (
	"context"
	"io"
)

type Principal struct {
	Name    string `json:"name"`
	Project string `json:"project"`
}

type Window struct {
	Width  uint32
	Height uint32
}

// ContextReader lets an exec command release stdin without closing its SSH session.
type ContextReader interface {
	ReadContext(context.Context, []byte) (int, error)
}

type Streams struct {
	In      io.Reader
	Out     io.Writer
	Err     io.Writer
	PTY     bool
	Term    string
	Window  Window
	Resize  <-chan Window
	Signals <-chan string
	// Abort closes the transport when a stream cannot complete without losing data.
	Abort func()
}

type CreateOptions struct {
	Name      string
	Image     string
	CPU       int
	Memory    string
	Processes int
	Disk      string
}

type Instance struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	CPU       string `json:"cpu,omitempty"`
	Memory    string `json:"memory,omitempty"`
	Processes string `json:"processes,omitempty"`
	Disk      string `json:"disk,omitempty"`
	Image     string `json:"image,omitempty"`
}

type ImageInfo struct {
	Fingerprint  string   `json:"fingerprint"`
	Aliases      []string `json:"aliases"`
	Description  string   `json:"description,omitempty"`
	OS           string   `json:"os,omitempty"`
	Release      string   `json:"release,omitempty"`
	Architecture string   `json:"architecture,omitempty"`
}

type Backend interface {
	List(context.Context, Principal) ([]Instance, error)
	Images(context.Context, Principal) ([]ImageInfo, error)
	Create(context.Context, Principal, CreateOptions) (Instance, error)
	Remove(context.Context, Principal, string) error
	Resume(context.Context, Principal, string) error
	Stop(context.Context, Principal, string) error
	Start(context.Context, Principal, string) error
	Restart(context.Context, Principal, string) error
	Exec(context.Context, Principal, string, string, Streams) (int, error)
}

type Handler interface {
	Handle(context.Context, Principal, string, Streams) int
}

type Limits struct {
	MaxInstances int    `json:"max_instances"`
	CPU          int    `json:"cpu"`
	Memory       string `json:"memory"`
	Processes    int    `json:"processes,omitempty"`
	Disk         string `json:"disk,omitempty"`
}
