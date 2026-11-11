package core

import "context"

type HostingCapabilities struct {
	ServerVersion    string      `json:"server_version"`
	Available        bool        `json:"available"`
	Reason           string      `json:"reason,omitempty"`
	Limits           Limits      `json:"limits"`
	Images           []string    `json:"images"`
	Registries       []string    `json:"registries"`
	DevelopmentBoxes bool        `json:"development_boxes"`
	Presets          []AppPreset `json:"presets"`
}

type AppOptions struct {
	Name           string            `json:"name"`
	Image          string            `json:"image"`
	CPU            int               `json:"cpu,omitempty"`
	Memory         string            `json:"memory,omitempty"`
	Disk           string            `json:"disk,omitempty"`
	Environment    map[string]string `json:"environment,omitempty"`
	DevelopmentBox bool              `json:"development_box,omitempty"`
}

type AppPreset struct {
	ID             string `json:"id"`
	Name           string `json:"name"`
	Description    string `json:"description"`
	Image          string `json:"image"`
	DevelopmentBox bool   `json:"development_box"`
}

func DevelopmentPresets() []AppPreset {
	return []AppPreset{
		{ID: "kali", Name: "Kali Linux", Description: "Minimal Kali rolling base. Add the tools you need.", Image: "kalilinux/kali-rolling:latest", DevelopmentBox: true},
		{ID: "alpine", Name: "Alpine Linux", Description: "A small Linux environment with apk package management.", Image: "alpine:latest", DevelopmentBox: true},
		{ID: "go-dev", Name: "Go dev box", Description: "Go compiler and standard tooling from the official image.", Image: "golang:latest", DevelopmentBox: true},
		{ID: "php-dev", Name: "PHP dev box", Description: "PHP CLI environment. Composer is not preinstalled.", Image: "php:cli", DevelopmentBox: true},
		{ID: "python-dev", Name: "Python dev box", Description: "Python 3 and pip from the official image.", Image: "python:3", DevelopmentBox: true},
	}
}

type AppTarget struct {
	Port   int
	BootID string
}

type HostingBackend interface {
	HostingCapabilities(context.Context, Principal) (HostingCapabilities, error)
	CreateApp(context.Context, Principal, AppOptions) (Instance, error)
	AppLogs(context.Context, Principal, string) (string, error)
	PublishApp(context.Context, Principal, string, int) error
	AppTarget(context.Context, Principal, string) (AppTarget, error)
}

type UserError struct{ Message string }

func (e UserError) Error() string { return e.Message }
