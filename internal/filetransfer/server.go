package filetransfer

import (
	"context"
	"encoding/binary"
	"errors"
	"io"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"incus-for-devs/internal/core"
)

const (
	maxPacket  = 256 * 1024
	maxHandles = 64
	fxInit     = 1
	fxVersion  = 2
	fxOpen     = 3
	fxClose    = 4
	fxRead     = 5
	fxWrite    = 6
	fxLstat    = 7
	fxFstat    = 8
	fxSetstat  = 9
	fxFsetstat = 10
	fxOpendir  = 11
	fxReaddir  = 12
	fxRemove   = 13
	fxMkdir    = 14
	fxRmdir    = 15
	fxRealpath = 16
	fxStat     = 17
	fxRename   = 18
	fxReadlink = 19
	fxStatus   = 101
	fxHandle   = 102
	fxName     = 104
	fxExtended = 200
)

var machineName = regexp.MustCompile(`^[a-z](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
var errPacket = errors.New("invalid SFTP packet")

type peer struct {
	name   string
	raw    io.ReadWriteCloser
	cancel context.CancelFunc
}

type handle struct {
	peer *peer
	raw  string
	root []core.Instance
	read bool
}

type server struct {
	ctx     context.Context
	user    core.Principal
	backend core.FileBackend
	peers   map[string]*peer
	handles map[string]*handle
	next    uint64
}

func u32(value uint32) []byte { return binary.BigEndian.AppendUint32(nil, value) }
func text(value string) []byte {
	return append(u32(uint32(len(value))), value...)
}

func take32(data *[]byte) (uint32, error) {
	if len(*data) < 4 {
		return 0, errPacket
	}
	value := binary.BigEndian.Uint32(*data)
	*data = (*data)[4:]
	return value, nil
}

func takeText(data *[]byte) (string, error) {
	length, err := take32(data)
	if err != nil || uint64(length) > uint64(len(*data)) {
		return "", errPacket
	}
	value := string((*data)[:length])
	*data = (*data)[length:]
	return value, nil
}

func readPacket(reader io.Reader) ([]byte, error) {
	var length [4]byte
	if _, err := io.ReadFull(reader, length[:]); err != nil {
		return nil, err
	}
	n := binary.BigEndian.Uint32(length[:])
	if n < 5 || n > maxPacket {
		return nil, errPacket
	}
	data := make([]byte, n)
	_, err := io.ReadFull(reader, data)
	return data, err
}

func writePacket(writer io.Writer, payload []byte) error {
	if len(payload) > maxPacket {
		return errPacket
	}
	data := append(u32(uint32(len(payload))), payload...)
	for len(data) > 0 {
		n, err := writer.Write(data)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		data = data[n:]
	}
	return nil
}

func status(id uint32, code uint32, message string) []byte {
	data := append([]byte{fxStatus}, u32(id)...)
	data = append(data, u32(code)...)
	data = append(data, text(message)...)
	return append(data, text("")...)
}

func Serve(ctx context.Context, user core.Principal, backend core.FileBackend, streams core.Streams) error {
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	s := &server{ctx: ctx, user: user, backend: backend, peers: make(map[string]*peer), handles: make(map[string]*handle)}
	defer func() {
		for _, peer := range s.peers {
			peer.cancel()
			peer.raw.Close()
		}
	}()
	stop := context.AfterFunc(ctx, func() {
		if streams.Abort != nil {
			streams.Abort()
		} else if closer, ok := streams.In.(io.Closer); ok {
			closer.Close()
		}
	})
	defer stop()
	init, err := readPacket(streams.In)
	if err != nil || init[0] != fxInit || binary.BigEndian.Uint32(init[1:5]) < 3 {
		return errPacket
	}
	version := append([]byte{fxVersion}, u32(3)...)
	version = append(version, text("posix-rename@openssh.com")...)
	version = append(version, text("1")...)
	if err := writePacket(streams.Out, version); err != nil {
		return err
	}
	for {
		packet, err := readPacket(streams.In)
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		id := binary.BigEndian.Uint32(packet[1:5])
		response := s.request(packet)
		if len(response) < 5 || binary.BigEndian.Uint32(response[1:5]) != id {
			return errPacket
		}
		if err := writePacket(streams.Out, response); err != nil {
			return err
		}
	}
}

func split(file string) (string, string, error) {
	if len(file) > 4096 || strings.ContainsRune(file, 0) {
		return "", "", errPacket
	}
	file = strings.TrimLeft(file, "/")
	if file == "" || file == "." {
		return "", "/", nil
	}
	name, tail, _ := strings.Cut(file, "/")
	if !machineName.MatchString(name) {
		return "", "", errPacket
	}
	// Preserve the guest path: cleaning before guest lookup changes symlink/.. semantics.
	return name, "/" + tail, nil
}

func (s *server) openPeer(name string) (*peer, error) {
	if cached := s.peers[name]; cached != nil {
		return cached, nil
	}
	if len(s.peers) >= 4 {
		return nil, errors.New("too many selected containers")
	}
	ctx, cancel := context.WithCancel(s.ctx)
	setup := time.AfterFunc(10*time.Second, cancel)
	raw, err := s.backend.OpenSFTP(ctx, s.user, name)
	if err != nil {
		setup.Stop()
		cancel()
		return nil, err
	}
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	ready := false
	defer func() {
		if !ready {
			stop()
			cancel()
			raw.Close()
		}
	}()
	if err := writePacket(raw, append([]byte{fxInit}, u32(3)...)); err != nil {
		setup.Stop()
		return nil, err
	}
	version, err := readPacket(raw)
	if !setup.Stop() || err != nil || version[0] != fxVersion || binary.BigEndian.Uint32(version[1:5]) != 3 || ctx.Err() != nil {
		return nil, errors.New("container SFTP unavailable")
	}
	p := &peer{name: name, raw: raw, cancel: cancel}
	s.peers[name] = p
	ready = true
	return p, nil
}

func (s *server) transact(p *peer, packet []byte) ([]byte, error) {
	timer := time.AfterFunc(30*time.Second, p.cancel)
	defer timer.Stop()
	err := writePacket(p.raw, packet)
	var response []byte
	if err == nil {
		response, err = readPacket(p.raw)
		if err == nil && binary.BigEndian.Uint32(response[1:5]) != binary.BigEndian.Uint32(packet[1:5]) {
			err = errPacket
		}
	}
	if err != nil {
		p.cancel()
		p.raw.Close()
		delete(s.peers, p.name)
		for name, h := range s.handles {
			if h.peer == p {
				delete(s.handles, name)
			}
		}
	}
	return response, err
}

func (s *server) newHandle(id uint32, h *handle) []byte {
	s.next++
	name := strconv.FormatUint(s.next, 10)
	s.handles[name] = h
	return append(append([]byte{fxHandle}, u32(id)...), text(name)...)
}

func attributes(data *[]byte) error {
	flags, err := take32(data)
	if err != nil || flags & ^uint32(0x8000000f) != 0 {
		return errPacket
	}
	size := 0
	for flag, count := range map[uint32]int{1: 8, 2: 8, 4: 4, 8: 8} {
		if flags&flag != 0 {
			size += count
		}
	}
	if len(*data) < size {
		return errPacket
	}
	*data = (*data)[size:]
	if flags&0x80000000 != 0 {
		count, err := take32(data)
		if err != nil || count > 256 {
			return errPacket
		}
		for range count {
			if _, err := takeText(data); err != nil {
				return err
			}
			if _, err := takeText(data); err != nil {
				return err
			}
		}
	}
	return nil
}

func (s *server) request(packet []byte) []byte {
	kind, id := packet[0], binary.BigEndian.Uint32(packet[1:5])
	body := packet[5:]
	bad := func() []byte { return status(id, 5, "invalid request") }
	if kind == fxClose || kind == fxRead || kind == fxWrite || kind == fxFstat || kind == fxFsetstat || kind == fxReaddir {
		name, err := takeText(&body)
		h := s.handles[name]
		if err != nil || h == nil {
			return status(id, 4, "invalid handle")
		}
		if kind == fxClose {
			delete(s.handles, name)
		}
		if h.peer == nil {
			if kind == fxClose {
				return status(id, 0, "")
			}
			if kind == fxReaddir && !h.read {
				h.read = true
				return rootNames(id, h.root)
			}
			if kind == fxReaddir {
				return status(id, 1, "end of directory")
			}
			return status(id, 3, "virtual root is read-only")
		}
		if kind == fxFsetstat {
			copy := body
			if attributes(&copy) != nil || len(copy) != 0 {
				return bad()
			}
		}
		forward := append(append([]byte{kind}, u32(id)...), text(h.raw)...)
		forward = append(forward, body...)
		response, err := s.transact(h.peer, forward)
		if err != nil {
			return status(id, 4, "container file request failed")
		}
		return response
	}
	extension := ""
	if kind == fxExtended {
		var err error
		extension, err = takeText(&body)
		if err != nil {
			return bad()
		}
		if extension != "posix-rename@openssh.com" {
			return status(id, 8, "unsupported extension")
		}
	}
	if kind != fxOpen && kind != fxLstat && kind != fxSetstat && kind != fxOpendir && kind != fxRemove && kind != fxMkdir && kind != fxRmdir && kind != fxRealpath && kind != fxStat && kind != fxRename && kind != fxExtended {
		return status(id, 8, "unsupported operation")
	}
	file, err := takeText(&body)
	name, target, splitErr := split(file)
	if err != nil || splitErr != nil {
		return bad()
	}
	if kind == fxOpen || kind == fxOpendir {
		if len(s.handles) >= maxHandles {
			return status(id, 4, "too many open file or directory handles")
		}
	}
	if name == "" {
		if kind == fxRealpath {
			return singleName(id, "/")
		}
		if kind == fxStat || kind == fxLstat {
			return append(append([]byte{105}, u32(id)...), dirAttributes()...)
		}
		if kind == fxOpendir {
			items, err := s.backend.List(s.ctx, s.user)
			if err != nil || len(items) > 256 {
				return status(id, 4, "container listing unavailable")
			}
			return s.newHandle(id, &handle{root: items})
		}
		return status(id, 3, "virtual root is read-only")
	}
	remaining := body
	if kind == fxOpen {
		flags, err := take32(&remaining)
		if err != nil || flags&4 != 0 {
			return status(id, 8, "append mode is unsupported")
		}
	}
	if kind == fxOpen || kind == fxSetstat || kind == fxMkdir {
		if attributes(&remaining) != nil || len(remaining) != 0 {
			return bad()
		}
	}
	if target == "/" && kind != fxStat && kind != fxLstat && kind != fxOpendir && kind != fxRealpath {
		return status(id, 3, "container root selector is read-only")
	}
	if kind == fxRename || extension != "" {
		destination, err := takeText(&body)
		other, destinationPath, splitErr := split(destination)
		if err != nil || splitErr != nil || other != name || destinationPath == "/" || len(body) != 0 {
			return status(id, 3, "cross-container rename is forbidden")
		}
		body = text(destinationPath)
	}
	p, err := s.openPeer(name)
	if err != nil {
		return status(id, 3, "container unavailable or not owned; openssh-sftp-server is required")
	}
	forward := append([]byte{kind}, u32(id)...)
	if extension != "" {
		forward = append(forward, text(extension)...)
	}
	forward = append(forward, text(target)...)
	forward = append(forward, body...)
	response, err := s.transact(p, forward)
	if err != nil {
		return status(id, 4, "container file request failed")
	}
	if response[0] == fxHandle && (kind == fxOpen || kind == fxOpendir) {
		payload := response[5:]
		raw, err := takeText(&payload)
		if err != nil || len(raw) > 4096 || len(payload) != 0 {
			return bad()
		}
		return s.newHandle(id, &handle{peer: p, raw: raw})
	}
	if response[0] == fxName && kind == fxRealpath {
		payload := response[5:]
		count, err := take32(&payload)
		if err != nil || count != 1 {
			return bad()
		}
		resolved, err := takeText(&payload)
		if err != nil || len(resolved) > 4096 || strings.ContainsRune(resolved, 0) {
			return bad()
		}
		return singleName(id, "/"+name+path.Clean("/"+resolved))
	}
	return response
}

func dirAttributes() []byte { return append(u32(4), u32(0040755)...) }

func singleName(id uint32, name string) []byte {
	data := append([]byte{fxName}, u32(id)...)
	data = append(data, u32(1)...)
	data = append(data, text(name)...)
	data = append(data, text(name)...)
	return append(data, u32(0)...)
}

func rootNames(id uint32, items []core.Instance) []byte {
	data := append([]byte{fxName}, u32(id)...)
	entries := make([]byte, 0)
	count := uint32(0)
	for _, item := range items {
		if item.Status != "Running" || !machineName.MatchString(item.Name) {
			continue
		}
		count++
		entries = append(entries, text(item.Name)...)
		entries = append(entries, text(item.Name)...)
		entries = append(entries, dirAttributes()...)
	}
	data = append(data, u32(count)...)
	return append(data, entries...)
}
