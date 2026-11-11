package incus

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"incus-for-devs/internal/core"
)

const environmentChangedKey = "user.incus-for-devs.environment-changed"

func (b *Backend) InspectApp(ctx context.Context, p core.Principal, name string) (core.AppDetails, error) {
	result := core.AppDetails{EnvironmentNames: []string{}}
	if err := validatePrincipal(p); err != nil {
		return result, err
	}
	if !validName(name) {
		return result, errors.New("invalid instance name")
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return result, err
	}
	defer func() { <-b.gate }()
	items, err := b.list(ctx, p)
	if err != nil {
		return result, err
	}
	item, err := findOwned(items, p, name)
	if err != nil {
		return result, err
	}
	result.Instance = b.summary(item)
	for key := range item.Config {
		if strings.HasPrefix(key, "environment.") {
			name := strings.TrimPrefix(key, "environment.")
			if envNamePattern.MatchString(name) {
				result.EnvironmentNames = append(result.EnvironmentNames, name)
			}
		}
	}
	sort.Strings(result.EnvironmentNames)
	changed, err := time.Parse(time.RFC3339Nano, item.Config[environmentChangedKey])
	result.EnvironmentPendingRestart = err == nil && item.Status == "Running" && changed.After(item.LastUsedAt)
	data, err := b.run(ctx, p, "query", "/1.0/instances/"+name+"/state?project="+p.Project)
	if err != nil {
		return result, err
	}
	var state struct {
		CPU struct {
			Usage *int64 `json:"usage"`
		} `json:"cpu"`
		Memory struct {
			Usage *int64 `json:"usage"`
		} `json:"memory"`
		Processes *int64 `json:"processes"`
		Disk      map[string]struct {
			Usage int64 `json:"usage"`
		} `json:"disk"`
		Network map[string]struct {
			Counters struct {
				Received int64 `json:"bytes_received"`
				Sent     int64 `json:"bytes_sent"`
			} `json:"counters"`
		} `json:"network"`
	}
	if json.Unmarshal(data, &state) != nil {
		return result, errors.New("invalid instance state")
	}
	metrics := core.AppMetrics{SampledAt: time.Now().UTC(), CPUUsageNS: nonnegative(state.CPU.Usage), MemoryBytes: nonnegative(state.Memory.Usage), Processes: nonnegative(state.Processes)}
	if root, ok := state.Disk["root"]; ok && root.Usage >= 0 {
		metrics.DiskBytes = &root.Usage
	}
	var rx, tx int64
	valid := len(state.Network) > 0
	for device, network := range state.Network {
		if device == "lo" {
			continue
		}
		r, s := network.Counters.Received, network.Counters.Sent
		if r < 0 || s < 0 || rx > math.MaxInt64-r || tx > math.MaxInt64-s {
			valid = false
			break
		}
		rx += r
		tx += s
	}
	if valid {
		metrics.NetworkRXBytes = &rx
		metrics.NetworkTXBytes = &tx
	}
	result.Metrics = metrics
	return result, nil
}

func nonnegative(value *int64) *int64 {
	if value == nil || *value < 0 {
		return nil
	}
	return value
}

func (b *Backend) UpdateEnvironment(ctx context.Context, p core.Principal, name string, in core.EnvironmentUpdate) error {
	if err := validatePrincipal(p); err != nil {
		return err
	}
	if !validName(name) || len(in.Values) == 0 || len(in.Values) > 64 {
		return core.UserError{Message: "Provide an app name and between 1 and 64 environment changes."}
	}
	total := 0
	for key, value := range in.Values {
		if !envNamePattern.MatchString(key) {
			return core.UserError{Message: "Environment names can contain only letters, digits and underscores."}
		}
		if value != nil {
			total += len(key) + len(*value)
			if !utf8.ValidString(*value) || strings.ContainsRune(*value, 0) || len(*value) > 4096 || total > 32<<10 {
				return core.UserError{Message: "Environment values must be valid text, up to 4 KiB each and 32 KiB total."}
			}
		}
	}
	ctx, cancel := context.WithTimeout(ctx, b.config.OperationTimeout)
	defer cancel()
	if err := b.lock(ctx); err != nil {
		return err
	}
	defer func() { <-b.gate }()
	items, err := b.list(ctx, p)
	if err != nil {
		return err
	}
	item, err := findOwned(items, p, name)
	if err != nil {
		return err
	}
	if item.Config[appImageKey] == "" {
		return core.UserError{Message: "Environment editing is limited to apps deployed through this panel. Existing Linux containers keep their operator-managed configuration."}
	}
	data, err := b.run(ctx, p, "query", "/1.0/instances/"+name+"?project="+p.Project)
	if err != nil {
		return err
	}
	var current struct {
		Architecture string                       `json:"architecture"`
		Config       map[string]string            `json:"config"`
		Devices      map[string]map[string]string `json:"devices"`
		Ephemeral    bool                         `json:"ephemeral"`
		Profiles     []string                     `json:"profiles"`
		Description  string                       `json:"description"`
	}
	if json.Unmarshal(data, &current) != nil || current.Config[ownerKey] != p.Name || current.Config[appImageKey] == "" {
		return errors.New("instance identity changed while reading configuration")
	}
	for key, value := range in.Values {
		if value == nil {
			delete(current.Config, "environment."+key)
		} else {
			current.Config["environment."+key] = *value
		}
	}
	count, size := 0, 0
	for key, value := range current.Config {
		if strings.HasPrefix(key, "environment.") {
			count++
			size += len(key) + len(value)
		}
	}
	if count > 128 || size > 64<<10 {
		return core.UserError{Message: "Combined image and app environment exceeds 128 entries or 64 KiB."}
	}
	current.Config[environmentChangedKey] = time.Now().UTC().Format(time.RFC3339Nano)
	data, err = json.Marshal(current)
	if err != nil {
		return err
	}
	_, err = b.runInput(ctx, p, data, "config", "edit", name)
	return err
}
