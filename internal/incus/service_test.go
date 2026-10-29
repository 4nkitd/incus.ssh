package incus

import (
	"context"
	"io"
	"net"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const serviceUUID = "11111111-2222-4333-8444-555555555555"
const serviceID = serviceUUID + "@2026-10-29T01:02:03Z"

func serviceFixture(t *testing.T) (*fixture, instance) {
	t.Helper()
	item := ownedInstance("demo", "alice")
	item.Config["volatile.uuid"] = serviceUUID
	item.LastUsedAt = time.Date(2026, 10, 29, 1, 2, 3, 0, time.UTC)
	item.Config["volatile.eth0.hwaddr"] = "00:16:3e:11:22:33"
	item.ExpandedDevices = map[string]map[string]string{"eth0": {"type": "nic", "network": "incusbr0"}}
	f := newFixture(t, []instance{item}, scenario{})
	writeJSON(t, filepath.Join(f.dir, "network.json"), map[string]any{"type": "bridge", "managed": true, "config": map[string]string{"ipv4.address": "10.245.239.1/24"}})
	writeJSON(t, filepath.Join(f.dir, "leases.json"), []map[string]string{{"type": "dynamic", "hwaddr": "00:16:3e:11:22:33", "address": "10.245.239.20"}})
	return f, item
}

func TestServiceIdentityRequiresOwnedRunningImmutableContainer(t *testing.T) {
	f, item := serviceFixture(t)
	id, err := f.backend.ServiceIdentity(context.Background(), alice, "demo")
	if err != nil || id != serviceID {
		t.Fatalf("id=%s error=%v", id, err)
	}
	for _, call := range f.calls(t) {
		if len(call) > 2 && call[2] == "query" && (len(call) != 4 || call[3] != "/1.0/networks/incusbr0?project="+alice.Project) {
			t.Fatal("raw query lost its API project scope", call)
		}
	}
	for _, mutate := range []func(*instance){
		func(i *instance) { i.Config[ownerKey] = "bob" },
		func(i *instance) { i.Status = "Stopped" },
		func(i *instance) { i.Config["volatile.uuid"] = "" },
		func(i *instance) { i.LastUsedAt = time.Time{} },
		func(i *instance) { i.Type = "virtual-machine" },
	} {
		f, item = serviceFixture(t)
		mutate(&item)
		writeJSON(t, filepath.Join(f.dir, "instances.json"), []instance{item})
		if _, err := f.backend.ServiceIdentity(context.Background(), alice, "demo"); err == nil {
			t.Fatal("accepted an unowned, stopped, unidentifiable or VM target")
		}
	}
}

func TestGuestInitiatedRebootCannotRetainConsent(t *testing.T) {
	f, item := serviceFixture(t)
	item.LastUsedAt = item.LastUsedAt.Add(time.Nanosecond)
	writeJSON(t, filepath.Join(f.dir, "instances.json"), []instance{item})
	if _, err := f.backend.DialService(context.Background(), alice, "demo", serviceID, 8080); err == nil || !strings.Contains(err.Error(), "restarted") {
		t.Fatal("a new boot inherited old exposure approval", err)
	}
}

func TestBootChangeDuringDialClosesSocketBeforeForwarding(t *testing.T) {
	f, item := serviceFixture(t)
	left, right := net.Pipe()
	defer right.Close()
	connection, err := f.backend.dialVerified(context.Background(), alice, "demo", serviceID, func() (net.Conn, error) {
		item.LastUsedAt = item.LastUsedAt.Add(time.Second)
		writeJSON(t, filepath.Join(f.dir, "instances.json"), []instance{item})
		return left, nil
	})
	if err == nil || connection != nil {
		t.Fatal("a connection reached an unapproved boot", err)
	}
	right.SetReadDeadline(time.Now().Add(time.Second))
	var data [1]byte
	if _, err := right.Read(data[:]); err != io.EOF {
		t.Fatal("socket was not closed before any guest bytes could be sent", err)
	}
}

func TestPublishingRejectsUnverifiedAndHostAddresses(t *testing.T) {
	for _, lease := range []map[string]string{
		{"type": "dynamic", "hwaddr": "00:16:3e:11:22:33", "address": "10.245.239.1"},
		{"type": "dynamic", "hwaddr": "00:16:3e:11:22:33", "address": "127.0.0.1"},
		{"type": "dynamic", "hwaddr": "00:16:3e:11:22:33", "address": "100.64.0.1"},
		{"type": "dynamic", "hwaddr": "00:16:3e:11:22:33", "address": "10.245.238.20"},
		{"type": "dynamic", "hwaddr": "00:16:3e:aa:bb:cc", "address": "10.245.239.20"},
		{"type": "dynamic", "hwaddr": "00:16:3e:11:22:33", "address": "10.245.239.0"},
		{"type": "unknown", "hwaddr": "00:16:3e:11:22:33", "address": "10.245.239.20"},
	} {
		f, _ := serviceFixture(t)
		writeJSON(t, filepath.Join(f.dir, "leases.json"), []map[string]string{lease})
		if _, err := f.backend.ServiceIdentity(context.Background(), alice, "demo"); err == nil {
			t.Fatalf("accepted unsafe lease %+v", lease)
		}
	}
}

func TestRecreatedContainerCannotInheritPublishedPort(t *testing.T) {
	f, _ := serviceFixture(t)
	_, err := f.backend.DialService(context.Background(), alice, "demo", "aaaaaaaa-bbbb-4ccc-8ddd-eeeeeeeeeeee@2026-10-29T01:02:03Z", 8080)
	if err == nil || !strings.Contains(err.Error(), "recreated") {
		t.Fatal("recreated machine inherited approval", err)
	}
	if calls := f.calls(t); len(calls) != 1 || calls[0][4] != "list" {
		t.Fatal("resolved or dialed the wrong instance", calls)
	}
	for _, port := range []int{0, -1, 65536} {
		if _, err := f.backend.DialService(context.Background(), alice, "demo", serviceID, port); err == nil {
			t.Fatal("accepted a non-explicit port")
		}
	}
}
