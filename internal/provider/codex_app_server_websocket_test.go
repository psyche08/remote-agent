package provider

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCodexAppServerWebSocketHandshakeAndJSON(t *testing.T) {
	received := make(chan []byte, 1)
	socketPath, done := startCodexAppServerWebSocketTestServer(t, "", func(conn net.Conn, reader *bufio.Reader) error {
		fin, opcode, masked, payload, err := readCodexAppServerWebSocketTestFrame(reader)
		if err != nil {
			return err
		}
		if !fin || opcode != webSocketTextFrame || !masked {
			return fmt.Errorf("unexpected client frame fin=%v opcode=%#x masked=%v", fin, opcode, masked)
		}
		received <- payload
		if err := writeCodexAppServerWebSocketTestFrame(conn, true, webSocketTextFrame, false, []byte(`{"result":"ok"}`)); err != nil {
			return err
		}
		fin, opcode, masked, _, err = readCodexAppServerWebSocketTestFrame(reader)
		if err != nil {
			return err
		}
		if !fin || opcode != webSocketCloseFrame || !masked {
			return fmt.Errorf("unexpected client close fin=%v opcode=%#x masked=%v", fin, opcode, masked)
		}
		return nil
	})

	client, err := dialCodexAppServerWebSocket(socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	request := []byte(`{"id":1,"method":"initialize"}`)
	if err := client.WriteJSON(request); err != nil {
		t.Fatal(err)
	}
	response, err := client.ReadJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(response) != `{"result":"ok"}` {
		t.Fatalf("response = %q", response)
	}
	if got := <-received; string(got) != string(request) {
		t.Fatalf("server received %q, want %q", got, request)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCodexAppServerWebSocketFragmentPingPongAndClose(t *testing.T) {
	socketPath, done := startCodexAppServerWebSocketTestServer(t, "", func(conn net.Conn, reader *bufio.Reader) error {
		if err := writeCodexAppServerWebSocketTestFrame(conn, false, webSocketTextFrame, false, []byte(`{"fragmented":`)); err != nil {
			return err
		}
		if err := writeCodexAppServerWebSocketTestFrame(conn, true, webSocketPingFrame, false, []byte("probe")); err != nil {
			return err
		}
		if err := writeCodexAppServerWebSocketTestFrame(conn, true, webSocketContinuationFrame, false, []byte(`true}`)); err != nil {
			return err
		}
		fin, opcode, masked, payload, err := readCodexAppServerWebSocketTestFrame(reader)
		if err != nil {
			return err
		}
		if !fin || opcode != webSocketPongFrame || !masked || string(payload) != "probe" {
			return fmt.Errorf("unexpected pong fin=%v opcode=%#x masked=%v payload=%q", fin, opcode, masked, payload)
		}
		closePayload := []byte{0x03, 0xe8}
		if err := writeCodexAppServerWebSocketTestFrame(conn, true, webSocketCloseFrame, false, closePayload); err != nil {
			return err
		}
		fin, opcode, masked, payload, err = readCodexAppServerWebSocketTestFrame(reader)
		if err != nil {
			return err
		}
		if !fin || opcode != webSocketCloseFrame || !masked || string(payload) != string(closePayload) {
			return fmt.Errorf("unexpected close reply fin=%v opcode=%#x masked=%v payload=%v", fin, opcode, masked, payload)
		}
		return nil
	})

	client, err := dialCodexAppServerWebSocket(socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	payload, err := client.ReadJSON()
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != `{"fragmented":true}` {
		t.Fatalf("payload = %q", payload)
	}
	if _, err := client.ReadJSON(); !errors.Is(err, io.EOF) {
		t.Fatalf("close read error = %v, want EOF", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCodexAppServerWebSocketIdleWaitAndPartialFrameDeadline(t *testing.T) {
	const timeout = 50 * time.Millisecond
	socketPath, done := startCodexAppServerWebSocketTestServer(t, "", func(conn net.Conn, _ *bufio.Reader) error {
		// Idle time must not expire a healthy shared-daemon connection.
		time.Sleep(3 * timeout)
		if _, err := conn.Write([]byte{0x81}); err != nil {
			return err
		}
		// Once a frame begins, the rest must arrive within the operation bound.
		time.Sleep(3 * timeout)
		return nil
	})

	client, err := dialCodexAppServerWebSocket(socketPath, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	client.timeout = timeout
	defer client.Close()
	_, err = client.ReadJSON()
	var netErr net.Error
	if !errors.As(err, &netErr) || !netErr.Timeout() {
		t.Fatalf("ReadJSON error = %v, want timeout after partial frame", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestCodexAppServerWebSocketRejectsBadHandshakeAndOversizedFrame(t *testing.T) {
	t.Run("bad accept", func(t *testing.T) {
		socketPath, done := startCodexAppServerWebSocketTestServer(t, "invalid", nil)
		if _, err := dialCodexAppServerWebSocket(socketPath, time.Second); err == nil ||
			!strings.Contains(err.Error(), "invalid accept key") {
			t.Fatalf("dial error = %v", err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("oversized message", func(t *testing.T) {
		socketPath, done := startCodexAppServerWebSocketTestServer(t, "", func(conn net.Conn, _ *bufio.Reader) error {
			header := []byte{0x81, 127, 0, 0, 0, 0, 0, 0, 0, 0}
			binary.BigEndian.PutUint64(header[2:], codexAppServerWebSocketMaxMessageSize+1)
			return writeAll(conn, header)
		})
		client, err := dialCodexAppServerWebSocket(socketPath, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		defer client.Close()
		if _, err := client.ReadJSON(); err == nil || !strings.Contains(err.Error(), "exceeds 16 MiB") {
			t.Fatalf("ReadJSON error = %v", err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})
}

func TestCodexAppServerWebSocketValidatesSocketAndPeer(t *testing.T) {
	t.Run("unsafe socket mode", func(t *testing.T) {
		dir := codexAppServerWebSocketTestDir(t)
		socketPath := filepath.Join(dir, "daemon.sock")
		listener, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatal(err)
		}
		defer listener.Close()
		if err := os.Chmod(socketPath, 0o666); err != nil {
			t.Fatal(err)
		}
		if _, err := dialCodexAppServerWebSocket(socketPath, time.Second); err == nil ||
			!strings.Contains(err.Error(), "permissions must be 0600") {
			t.Fatalf("dial error = %v", err)
		}
	})

	t.Run("current uid peer", func(t *testing.T) {
		socketPath, done := startCodexAppServerWebSocketTestServer(t, "", func(conn net.Conn, reader *bufio.Reader) error {
			_, opcode, _, _, err := readCodexAppServerWebSocketTestFrame(reader)
			if err != nil {
				return err
			}
			if opcode != webSocketCloseFrame {
				return fmt.Errorf("opcode = %#x, want close", opcode)
			}
			return nil
		})
		client, err := dialCodexAppServerWebSocket(socketPath, time.Second)
		if err != nil {
			t.Fatal(err)
		}
		if err := validateCodexAppServerWebSocketPeer(client.conn); err != nil {
			t.Fatal(err)
		}
		if err := client.Close(); err != nil {
			t.Fatal(err)
		}
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	})

	t.Run("discovery snapshot", func(t *testing.T) {
		dir := codexAppServerWebSocketTestDir(t)
		socketPath := filepath.Join(dir, "daemon.sock")
		first, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.Chmod(socketPath, 0o600); err != nil {
			t.Fatal(err)
		}
		snapshot, err := os.Lstat(socketPath)
		if err != nil {
			t.Fatal(err)
		}
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		if err := os.Remove(socketPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		second, err := net.Listen("unix", socketPath)
		if err != nil {
			t.Fatal(err)
		}
		defer second.Close()
		if err := os.Chmod(socketPath, 0o600); err != nil {
			t.Fatal(err)
		}
		status := CodexSharedDaemonStatus{SocketPath: socketPath, socketSnapshot: snapshot}
		if _, err := dialCodexAppServerWebSocketStatus(status, time.Second); err == nil ||
			!strings.Contains(err.Error(), "changed after discovery") {
			t.Fatalf("dial error = %v", err)
		}
	})
}

func startCodexAppServerWebSocketTestServer(
	t *testing.T,
	acceptOverride string,
	handler func(net.Conn, *bufio.Reader) error,
) (string, <-chan error) {
	t.Helper()
	dir := codexAppServerWebSocketTestDir(t)
	socketPath := filepath.Join(dir, "daemon.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(socketPath, 0o600); err != nil {
		listener.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	done := make(chan error, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			done <- err
			return
		}
		defer conn.Close()
		reader := bufio.NewReader(conn)
		request, err := http.ReadRequest(reader)
		if err != nil {
			done <- err
			return
		}
		if request.Method != http.MethodGet || request.URL.Path != "/" ||
			!headerContainsToken(request.Header.Values("Connection"), "upgrade") ||
			!headerContainsToken(request.Header.Values("Upgrade"), "websocket") ||
			request.Header.Get("Sec-WebSocket-Version") != "13" {
			done <- errors.New("invalid WebSocket upgrade request")
			return
		}
		accept := acceptOverride
		if accept == "" {
			sum := sha1.Sum([]byte(request.Header.Get("Sec-WebSocket-Key") + webSocketGUID))
			accept = base64.StdEncoding.EncodeToString(sum[:])
		}
		response := "HTTP/1.1 101 Switching Protocols\r\n" +
			"Connection: Upgrade\r\n" +
			"Upgrade: websocket\r\n" +
			"Sec-WebSocket-Accept: " + accept + "\r\n\r\n"
		if err := writeAll(conn, []byte(response)); err != nil {
			done <- err
			return
		}
		if handler != nil {
			err = handler(conn, reader)
		}
		done <- err
	}()
	return socketPath, done
}

func codexAppServerWebSocketTestDir(t *testing.T) string {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "rcws-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}

func readCodexAppServerWebSocketTestFrame(reader *bufio.Reader) (bool, byte, bool, []byte, error) {
	var header [2]byte
	if _, err := io.ReadFull(reader, header[:]); err != nil {
		return false, 0, false, nil, err
	}
	fin := header[0]&0x80 != 0
	opcode := header[0] & 0x0f
	masked := header[1]&0x80 != 0
	length := uint64(header[1] & 0x7f)
	switch length {
	case 126:
		var extended [2]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return false, 0, false, nil, err
		}
		length = uint64(binary.BigEndian.Uint16(extended[:]))
	case 127:
		var extended [8]byte
		if _, err := io.ReadFull(reader, extended[:]); err != nil {
			return false, 0, false, nil, err
		}
		length = binary.BigEndian.Uint64(extended[:])
	}
	var mask [4]byte
	if masked {
		if _, err := io.ReadFull(reader, mask[:]); err != nil {
			return false, 0, false, nil, err
		}
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(reader, payload); err != nil {
		return false, 0, false, nil, err
	}
	if masked {
		for i := range payload {
			payload[i] ^= mask[i%len(mask)]
		}
	}
	return fin, opcode, masked, payload, nil
}

func writeCodexAppServerWebSocketTestFrame(
	writer io.Writer,
	fin bool,
	opcode byte,
	masked bool,
	payload []byte,
) error {
	first := opcode & 0x0f
	if fin {
		first |= 0x80
	}
	header := []byte{first}
	maskBit := byte(0)
	if masked {
		maskBit = 0x80
	}
	switch {
	case len(payload) <= 125:
		header = append(header, maskBit|byte(len(payload)))
	case len(payload) <= 0xffff:
		header = append(header, maskBit|126, 0, 0)
		binary.BigEndian.PutUint16(header[len(header)-2:], uint16(len(payload)))
	default:
		header = append(header, maskBit|127, 0, 0, 0, 0, 0, 0, 0, 0)
		binary.BigEndian.PutUint64(header[len(header)-8:], uint64(len(payload)))
	}
	var mask [4]byte
	if masked {
		mask = [4]byte{1, 2, 3, 4}
		header = append(header, mask[:]...)
	}
	body := append([]byte(nil), payload...)
	if masked {
		for i := range body {
			body[i] ^= mask[i%len(mask)]
		}
	}
	if err := writeAll(writer, header); err != nil {
		return err
	}
	return writeAll(writer, body)
}
