package filetransfer

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"incus-for-devs/internal/core"
)

type testBackend struct {
	response func([]byte) []byte
}

func (*testBackend) List(context.Context, core.Principal) ([]core.Instance, error) {
	return []core.Instance{{Name: "demo", Status: "Running"}}, nil
}

func (b *testBackend) OpenSFTP(ctx context.Context, _ core.Principal, name string) (io.ReadWriteCloser, error) {
	if name != "demo" {
		return nil, errors.New("not owned")
	}
	left, right := net.Pipe()
	go func() {
		defer right.Close()
		stop := context.AfterFunc(ctx, func() { right.Close() })
		defer stop()
		if _, err := readPacket(right); err != nil {
			return
		}
		if writePacket(right, append([]byte{fxVersion}, u32(3)...)) != nil {
			return
		}
		for {
			packet, err := readPacket(right)
			if err != nil {
				return
			}
			if writePacket(right, b.response(packet)) != nil {
				return
			}
		}
	}()
	return left, nil
}

func startTest(t *testing.T, backend *testBackend) func(byte, uint32, []byte) []byte {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	left, right := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer right.Close()
		Serve(ctx, core.Principal{Name: "alice", Project: "dev-alice"}, backend, core.Streams{In: right, Out: right, Abort: func() { right.Close() }})
	}()
	t.Cleanup(func() {
		cancel()
		left.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("file-transfer server did not shut down")
		}
	})
	left.SetDeadline(time.Now().Add(5 * time.Second))
	if err := writePacket(left, append([]byte{fxInit}, u32(3)...)); err != nil {
		t.Fatal(err)
	}
	if _, err := readPacket(left); err != nil {
		t.Fatal(err)
	}
	return func(kind byte, id uint32, body []byte) []byte {
		t.Helper()
		packet := append(append([]byte{kind}, u32(id)...), body...)
		if err := writePacket(left, packet); err != nil {
			t.Fatal(err)
		}
		response, err := readPacket(left)
		if err != nil {
			t.Fatal(err)
		}
		return response
	}
}

func TestMalformedAttributesFailWithoutPanicOrGuestIO(t *testing.T) {
	backend := &testBackend{response: func([]byte) []byte { t.Error("malformed attributes reached the guest"); return nil }}
	request := startTest(t, backend)
	for _, kind := range []byte{fxSetstat, fxMkdir} {
		response := request(kind, 1, append(text("/demo/file"), u32(1)...))
		if response[0] != fxStatus || binary.BigEndian.Uint32(response[5:9]) != 5 {
			t.Fatal("malformed attributes not rejected", response)
		}
	}
	if response := request(fxRealpath, 2, text(".")); response[0] != fxName {
		t.Fatal("gateway did not survive malformed metadata")
	}
}

func TestOpenAttributesAndHandleMetadataAreForwardedUnchanged(t *testing.T) {
	requests := make(chan []byte, 10)
	backend := &testBackend{response: func(packet []byte) []byte {
		requests <- packet
		id := binary.BigEndian.Uint32(packet[1:5])
		if packet[0] == fxOpen {
			return append(append([]byte{fxHandle}, u32(id)...), text("native-"+string(rune('a'+id)))...)
		}
		return status(id, 0, "")
	}}
	request := startTest(t, backend)
	open := append(text("/demo/a"), u32(26)...)
	open = append(open, u32(4)...)
	open = append(open, u32(0600)...)
	first := request(fxOpen, 1, open)
	payload := first[5:]
	handle, err := takeText(&payload)
	if err != nil {
		t.Fatal(err)
	}
	forwarded := <-requests
	body := forwarded[5:]
	name, _ := takeText(&body)
	flags, _ := take32(&body)
	attrs, _ := take32(&body)
	mode, _ := take32(&body)
	if name != "/a" || flags != 26 || attrs != 4 || mode != 0600 {
		t.Fatal("OPEN attributes or permissions were lost", name, flags, attrs, mode)
	}
	request(fxRename, 2, append(text("/demo/a"), text("/demo/b")...))
	<-requests
	request(fxOpen, 3, open)
	<-requests
	set := append(text(handle), u32(1)...)
	set = binary.BigEndian.AppendUint64(set, 0)
	request(fxFsetstat, 4, set)
	forwarded = <-requests
	body = forwarded[5:]
	native, _ := takeText(&body)
	if forwarded[0] != fxFsetstat || native != "native-b" {
		t.Fatal("FSETSTAT used a pathname or a replacement file", forwarded)
	}
	request(fxFstat, 5, text(handle))
	forwarded = <-requests
	if forwarded[0] != fxFstat {
		t.Fatal("FSTAT was translated into path-based STAT")
	}
}

func TestPathsReachGuestBeforeSymlinkAndDotDotResolution(t *testing.T) {
	observed := make(chan string, 2)
	backend := &testBackend{response: func(packet []byte) []byte {
		body := packet[5:]
		name, _ := takeText(&body)
		observed <- name
		id := binary.BigEndian.Uint32(packet[1:5])
		if packet[0] == fxRealpath {
			return singleName(id, "/file")
		}
		return status(id, 0, "")
	}}
	request := startTest(t, backend)
	response := request(fxRealpath, 1, text("/demo/var/run/../file"))
	if guestPath := <-observed; guestPath != "/var/run/../file" {
		t.Fatal("symlink path was cleaned on the host", guestPath)
	}
	body := response[9:]
	resolved, _ := takeText(&body)
	if resolved != "/demo/file" {
		t.Fatal("guest-resolved path escaped the selector", resolved)
	}
	request(fxStat, 2, text("/demo/../../foreign/root/file"))
	if guestPath := <-observed; guestPath != "/../../foreign/root/file" {
		t.Fatal("path traversal changed the selected machine", guestPath)
	}
}

func TestDirectoryHandlesShareTheSameBoundAsFiles(t *testing.T) {
	backend := &testBackend{response: func(packet []byte) []byte {
		id := binary.BigEndian.Uint32(packet[1:5])
		return append(append([]byte{fxHandle}, u32(id)...), text("directory")...)
	}}
	request := startTest(t, backend)
	for id := uint32(1); id <= maxHandles; id++ {
		response := request(fxOpendir, id, text("/demo/"))
		if response[0] != fxHandle {
			t.Fatal("directory open failed", id)
		}
	}
	response := request(fxOpendir, 100, text("/demo/"))
	if response[0] != fxStatus || binary.BigEndian.Uint32(response[5:9]) != 4 {
		t.Fatal("directory handles bypassed the handle budget")
	}
}

func FuzzAttributeDecoder(f *testing.F) {
	f.Add([]byte{0, 0, 0, 1})
	f.Add([]byte{0, 0, 0, 4, 0, 0, 1, 128})
	f.Fuzz(func(t *testing.T, data []byte) { attributes(&data) })
}
