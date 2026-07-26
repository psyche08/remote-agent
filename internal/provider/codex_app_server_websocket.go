package provider

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"
	"unsafe"
)

const (
	codexAppServerWebSocketMaxMessageSize = 16 << 20
	codexAppServerWebSocketDefaultTimeout = 30 * time.Second

	webSocketContinuationFrame = 0x0
	webSocketTextFrame         = 0x1
	webSocketCloseFrame        = 0x8
	webSocketPingFrame         = 0x9
	webSocketPongFrame         = 0xa
)

const webSocketGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

type codexAppServerWebSocket struct {
	conn    net.Conn
	reader  *bufio.Reader
	timeout time.Duration

	writeMu   sync.Mutex
	closed    bool
	closeErr  error
	closeOnce sync.Once
}

func dialCodexAppServerWebSocket(path string, timeout time.Duration) (*codexAppServerWebSocket, error) {
	return dialCodexAppServerWebSocketExpected(path, nil, timeout)
}

func dialCodexAppServerWebSocketStatus(status CodexSharedDaemonStatus, timeout time.Duration) (*codexAppServerWebSocket, error) {
	if status.socketSnapshot == nil {
		return nil, errors.New("Codex shared app-server daemon socket was not validated during discovery")
	}
	return dialCodexAppServerWebSocketExpected(status.SocketPath, status.socketSnapshot, timeout)
}

func dialCodexAppServerWebSocketExpected(path string, expected os.FileInfo, timeout time.Duration) (*codexAppServerWebSocket, error) {
	if timeout <= 0 {
		timeout = codexAppServerWebSocketDefaultTimeout
	}
	before, err := validateCodexSharedDaemonSocket(path)
	if err != nil {
		return nil, err
	}
	if expected != nil && !os.SameFile(expected, before) {
		return nil, errors.New("Codex shared app-server daemon socket changed after discovery")
	}

	conn, err := (&net.Dialer{Timeout: timeout}).Dial("unix", path)
	if err != nil {
		return nil, fmt.Errorf("connect to Codex shared app-server daemon: %w", err)
	}
	keep := false
	defer func() {
		if !keep {
			_ = conn.Close()
		}
	}()

	after, err := validateCodexSharedDaemonSocket(path)
	if err != nil {
		return nil, err
	}
	if !os.SameFile(before, after) {
		return nil, errors.New("Codex shared app-server daemon socket changed while connecting")
	}
	if err := validateCodexAppServerWebSocketPeer(conn); err != nil {
		return nil, err
	}

	deadline := time.Now().Add(timeout)
	if err := conn.SetDeadline(deadline); err != nil {
		return nil, fmt.Errorf("set Codex shared app-server WebSocket handshake deadline: %w", err)
	}
	reader := bufio.NewReader(conn)
	if err := performCodexAppServerWebSocketHandshake(conn, reader); err != nil {
		return nil, err
	}
	if err := conn.SetDeadline(time.Time{}); err != nil {
		return nil, fmt.Errorf("clear Codex shared app-server WebSocket handshake deadline: %w", err)
	}

	keep = true
	return &codexAppServerWebSocket{conn: conn, reader: reader, timeout: timeout}, nil
}

func validateCodexAppServerWebSocketPeer(conn net.Conn) error {
	syscallConn, ok := conn.(syscall.Conn)
	if !ok {
		return errors.New("Codex shared app-server daemon connection does not expose peer credentials")
	}
	rawConn, err := syscallConn.SyscallConn()
	if err != nil {
		return fmt.Errorf("inspect Codex shared app-server daemon peer: %w", err)
	}
	var peerUID uint32
	var peerErr error
	if err := rawConn.Control(func(fd uintptr) {
		peerUID, peerErr = codexAppServerWebSocketPeerUID(fd)
	}); err != nil {
		return fmt.Errorf("inspect Codex shared app-server daemon peer: %w", err)
	}
	if peerErr != nil {
		return fmt.Errorf("inspect Codex shared app-server daemon peer: %w", peerErr)
	}
	if int(peerUID) != os.Getuid() {
		return errors.New("Codex shared app-server daemon peer is not owned by the current user")
	}
	return nil
}

func codexAppServerWebSocketPeerUID(fd uintptr) (uint32, error) {
	switch runtime.GOOS {
	case "darwin":
		// LOCAL_PEERCRED returns struct xucred. Darwin defines SOL_LOCAL as
		// zero, LOCAL_PEERCRED as one, and NGROUPS as sixteen.
		var credential struct {
			Version uint32
			UID     uint32
			NGroups int16
			_       [2]byte
			Groups  [16]uint32
		}
		size := uint32(unsafe.Sizeof(credential))
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			0,
			1,
			uintptr(unsafe.Pointer(&credential)),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			return 0, errno
		}
		if size < 8 {
			return 0, errors.New("LOCAL_PEERCRED returned a truncated credential")
		}
		return credential.UID, nil
	case "linux":
		// SO_PEERCRED returns pid, uid, gid at SOL_SOCKET. Raw constants keep
		// this macOS product free of a new x/sys dependency while allowing CI
		// to exercise the transport on Linux.
		var credential struct {
			PID int32
			UID uint32
			GID uint32
		}
		size := uint32(unsafe.Sizeof(credential))
		_, _, errno := syscall.Syscall6(
			syscall.SYS_GETSOCKOPT,
			fd,
			1,
			17,
			uintptr(unsafe.Pointer(&credential)),
			uintptr(unsafe.Pointer(&size)),
			0,
		)
		if errno != 0 {
			return 0, errno
		}
		if size < uint32(unsafe.Sizeof(credential)) {
			return 0, errors.New("SO_PEERCRED returned a truncated credential")
		}
		return credential.UID, nil
	default:
		return 0, fmt.Errorf("peer credential validation is unsupported on %s", runtime.GOOS)
	}
}

func performCodexAppServerWebSocketHandshake(conn net.Conn, reader *bufio.Reader) error {
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate Codex shared app-server WebSocket key: %w", err)
	}
	key := base64.StdEncoding.EncodeToString(nonce)
	request := &http.Request{
		Method:     http.MethodGet,
		URL:        &url.URL{Path: "/"},
		Proto:      "HTTP/1.1",
		ProtoMajor: 1,
		ProtoMinor: 1,
		Host:       "localhost",
		Header: http.Header{
			"Connection":            []string{"Upgrade"},
			"Upgrade":               []string{"websocket"},
			"Sec-Websocket-Key":     []string{key},
			"Sec-Websocket-Version": []string{"13"},
		},
	}
	if err := request.Write(conn); err != nil {
		return fmt.Errorf("write Codex shared app-server WebSocket handshake: %w", err)
	}

	response, err := http.ReadResponse(reader, request)
	if err != nil {
		return fmt.Errorf("read Codex shared app-server WebSocket handshake: %w", err)
	}
	if response.Body != nil {
		defer response.Body.Close()
	}
	if response.StatusCode != http.StatusSwitchingProtocols {
		return fmt.Errorf("Codex shared app-server WebSocket upgrade returned HTTP %d", response.StatusCode)
	}
	if !headerContainsToken(response.Header.Values("Connection"), "upgrade") ||
		!headerContainsToken(response.Header.Values("Upgrade"), "websocket") {
		return errors.New("Codex shared app-server WebSocket upgrade response is missing required headers")
	}

	digest := sha1.Sum([]byte(key + webSocketGUID))
	expectedAccept := base64.StdEncoding.EncodeToString(digest[:])
	if strings.TrimSpace(response.Header.Get("Sec-WebSocket-Accept")) != expectedAccept {
		return errors.New("Codex shared app-server WebSocket upgrade returned an invalid accept key")
	}
	return nil
}

func headerContainsToken(values []string, expected string) bool {
	for _, value := range values {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), expected) {
				return true
			}
		}
	}
	return false
}

func (c *codexAppServerWebSocket) WriteJSON(payload []byte) error {
	if len(payload) > codexAppServerWebSocketMaxMessageSize {
		return errors.New("Codex shared app-server WebSocket message exceeds 16 MiB")
	}
	if !utf8.Valid(payload) || !json.Valid(payload) {
		return errors.New("Codex shared app-server WebSocket payload is not valid JSON")
	}
	return c.writeFrame(webSocketTextFrame, payload)
}

func (c *codexAppServerWebSocket) ReadJSON() ([]byte, error) {
	var message []byte
	fragmented := false
	var messageDeadline time.Time

	for {
		fin, opcode, payload, frameDeadline, err := c.readFrame(messageDeadline)
		if err != nil {
			return nil, err
		}
		switch opcode {
		case webSocketPingFrame:
			if err := c.writeFrame(webSocketPongFrame, payload); err != nil {
				return nil, err
			}
			if !fragmented {
				messageDeadline = time.Time{}
			}
			continue
		case webSocketPongFrame:
			if !fragmented {
				messageDeadline = time.Time{}
			}
			continue
		case webSocketCloseFrame:
			if len(payload) == 1 || (len(payload) > 2 && !utf8.Valid(payload[2:])) {
				_ = c.closeWithPayload(nil)
				return nil, errors.New("Codex shared app-server WebSocket received an invalid close frame")
			}
			_ = c.closeWithPayload(payload)
			return nil, io.EOF
		case webSocketTextFrame:
			if fragmented {
				return nil, errors.New("Codex shared app-server WebSocket received a new data frame during a fragmented message")
			}
			if fin {
				if !utf8.Valid(payload) || !json.Valid(payload) {
					return nil, errors.New("Codex shared app-server WebSocket received invalid JSON")
				}
				return payload, nil
			}
			message = append(message, payload...)
			fragmented = true
			messageDeadline = frameDeadline
		case webSocketContinuationFrame:
			if !fragmented {
				return nil, errors.New("Codex shared app-server WebSocket received an unexpected continuation frame")
			}
			if len(message)+len(payload) > codexAppServerWebSocketMaxMessageSize {
				return nil, errors.New("Codex shared app-server WebSocket message exceeds 16 MiB")
			}
			message = append(message, payload...)
			if fin {
				if !utf8.Valid(message) || !json.Valid(message) {
					return nil, errors.New("Codex shared app-server WebSocket received invalid JSON")
				}
				return message, nil
			}
		default:
			return nil, fmt.Errorf("Codex shared app-server WebSocket received unsupported opcode %#x", opcode)
		}
	}
}

func (c *codexAppServerWebSocket) Close() error {
	return c.closeWithPayload(nil)
}

func (c *codexAppServerWebSocket) closeWithPayload(payload []byte) error {
	c.closeOnce.Do(func() {
		c.writeMu.Lock()
		defer c.writeMu.Unlock()
		c.closed = true
		if err := c.writeFrameLocked(webSocketCloseFrame, payload); err != nil {
			c.closeErr = err
		}
		if err := c.conn.Close(); err != nil && c.closeErr == nil {
			c.closeErr = err
		}
	})
	return c.closeErr
}

func (c *codexAppServerWebSocket) readFrame(deadline time.Time) (bool, byte, []byte, time.Time, error) {
	var header [2]byte
	headerOffset := 0
	if deadline.IsZero() {
		// A healthy shared daemon may remain idle indefinitely. Wait for the
		// first byte without a deadline; Close still interrupts the read by
		// closing conn. Once a frame starts, bound the rest of that frame (and
		// any fragmented message) so a partial writer cannot hold us forever.
		if err := c.conn.SetReadDeadline(time.Time{}); err != nil {
			return false, 0, nil, time.Time{}, err
		}
		if _, err := io.ReadFull(c.reader, header[:1]); err != nil {
			return false, 0, nil, time.Time{}, err
		}
		headerOffset = 1
		deadline = time.Now().Add(c.timeout)
	}
	if err := c.conn.SetReadDeadline(deadline); err != nil {
		return false, 0, nil, time.Time{}, err
	}
	if _, err := io.ReadFull(c.reader, header[headerOffset:]); err != nil {
		return false, 0, nil, time.Time{}, err
	}

	fin := header[0]&0x80 != 0
	if header[0]&0x70 != 0 {
		return false, 0, nil, time.Time{}, errors.New("Codex shared app-server WebSocket received a frame with reserved bits set")
	}
	opcode := header[0] & 0x0f
	if header[1]&0x80 != 0 {
		return false, 0, nil, time.Time{}, errors.New("Codex shared app-server WebSocket received a masked server frame")
	}

	payloadLength := uint64(header[1] & 0x7f)
	switch payloadLength {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			return false, 0, nil, time.Time{}, err
		}
		payloadLength = uint64(binary.BigEndian.Uint16(extended[:]))
		if payloadLength < 126 {
			return false, 0, nil, time.Time{}, errors.New("Codex shared app-server WebSocket received a non-canonical frame length")
		}
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(c.reader, extended[:]); err != nil {
			return false, 0, nil, time.Time{}, err
		}
		payloadLength = binary.BigEndian.Uint64(extended[:])
		if payloadLength < 1<<16 || payloadLength&(uint64(1)<<63) != 0 {
			return false, 0, nil, time.Time{}, errors.New("Codex shared app-server WebSocket received an invalid frame length")
		}
	}

	control := opcode&0x8 != 0
	if control && (!fin || payloadLength > 125) {
		return false, 0, nil, time.Time{}, errors.New("Codex shared app-server WebSocket received an invalid control frame")
	}
	if payloadLength > codexAppServerWebSocketMaxMessageSize {
		return false, 0, nil, time.Time{}, errors.New("Codex shared app-server WebSocket message exceeds 16 MiB")
	}

	payload := make([]byte, int(payloadLength))
	if _, err := io.ReadFull(c.reader, payload); err != nil {
		return false, 0, nil, time.Time{}, err
	}
	return fin, opcode, payload, deadline, nil
}

func (c *codexAppServerWebSocket) writeFrame(opcode byte, payload []byte) error {
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if c.closed {
		return net.ErrClosed
	}
	return c.writeFrameLocked(opcode, payload)
}

func (c *codexAppServerWebSocket) writeFrameLocked(opcode byte, payload []byte) error {
	if opcode&0x8 != 0 && len(payload) > 125 {
		return errors.New("Codex shared app-server WebSocket control frame is too large")
	}
	if len(payload) > codexAppServerWebSocketMaxMessageSize {
		return errors.New("Codex shared app-server WebSocket message exceeds 16 MiB")
	}
	if err := c.conn.SetWriteDeadline(time.Now().Add(c.timeout)); err != nil {
		return err
	}

	header := make([]byte, 0, 14)
	header = append(header, 0x80|(opcode&0x0f))
	switch {
	case len(payload) <= 125:
		header = append(header, 0x80|byte(len(payload)))
	case len(payload) <= 0xffff:
		header = append(header, 0x80|126, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(len(payload)))
	default:
		header = append(header, 0x80|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(len(payload)))
	}

	var mask [4]byte
	if _, err := rand.Read(mask[:]); err != nil {
		return fmt.Errorf("generate Codex shared app-server WebSocket frame mask: %w", err)
	}
	header = append(header, mask[:]...)
	masked := make([]byte, len(payload))
	for i := range payload {
		masked[i] = payload[i] ^ mask[i%len(mask)]
	}
	if err := writeAll(c.conn, header); err != nil {
		return err
	}
	return writeAll(c.conn, masked)
}

func writeAll(writer io.Writer, payload []byte) error {
	for len(payload) > 0 {
		n, err := writer.Write(payload)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		payload = payload[n:]
	}
	return nil
}
