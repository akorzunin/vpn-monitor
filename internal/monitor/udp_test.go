package monitor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
	"time"
)

func udpRelay(t *testing.T, drop bool, nonLoopback bool) string {
	t.Helper()
	listener, e := net.Listen("tcp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = listener.Close() })
	relay, e := net.ListenPacket("udp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = relay.Close() })
	go func() {
		buf := make([]byte, 2048)
		for {
			n, addr, e := relay.ReadFrom(buf)
			if e != nil {
				return
			}
			if !drop {
				_, _ = relay.WriteTo(buf[:n], addr)
			}
		}
	}()
	go func() {
		c, e := listener.Accept()
		if e != nil {
			return
		}
		defer c.Close()
		_ = c.SetDeadline(time.Now().Add(3 * time.Second))
		var greeting [3]byte
		if _, e = io.ReadFull(c, greeting[:]); e != nil {
			return
		}
		_, _ = c.Write([]byte{5, 0})
		var request [10]byte
		if _, e = io.ReadFull(c, request[:]); e != nil {
			return
		}
		response := []byte{5, 0, 0, 1, 127, 0, 0, 1}
		if nonLoopback {
			response = []byte{5, 0, 0, 1, 192, 0, 2, 1}
		}
		response = binary.BigEndian.AppendUint16(response, uint16(relay.LocalAddr().(*net.UDPAddr).Port))
		_, _ = c.Write(response)
		_, _ = io.Copy(io.Discard, c)
	}()
	return listener.Addr().String()
}
func TestUDPPingAndLoss(t *testing.T) {
	for _, tc := range []struct {
		name     string
		drop     bool
		nonLoop  bool
		state    string
		received int
		loss     float64
	}{
		{"echo", false, false, "ok", 3, 0}, {"drop", true, false, "failed", 0, 100}, {"unsafe relay", false, true, "error", 0, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			address := udpRelay(t, tc.drop, tc.nonLoop)
			r := udpTest(context.Background(), address, UDPConfig{Endpoint: "192.0.2.1:9444", Token: strings.Repeat("x", 32), Samples: 3, Timeout: 30 * time.Millisecond})
			if r.State != tc.state || r.Received != tc.received || r.LossPercent != tc.loss {
				t.Fatalf("%+v", r)
			}
		})
	}
}
func TestEchoAuthenticationAndCancellation(t *testing.T) {
	listener, e := net.ListenPacket("udp4", "127.0.0.1:0")
	if e != nil {
		t.Fatal(e)
	}
	address := listener.LocalAddr().String()
	_ = listener.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	token := strings.Repeat("x", 32)
	go func() { done <- Echo(ctx, address, token) }()
	conn, e := net.Dial("udp", address)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close()
	payload := bytes.Repeat([]byte{1}, 32)
	mac := hmac.New(sha256.New, []byte(token))
	mac.Write(payload)
	payload = append(payload, mac.Sum(nil)...)
	buf := make([]byte, 128)
	received := false
	for i := 0; i < 20; i++ {
		_ = conn.SetDeadline(time.Now().Add(20 * time.Millisecond))
		_, _ = conn.Write(payload)
		n, e := conn.Read(buf)
		if e == nil && bytes.Equal(payload, buf[:n]) {
			received = true
			break
		}
		time.Sleep(time.Millisecond)
	}
	if !received {
		t.Fatal("valid echo failed")
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Millisecond))
	_, _ = conn.Write(make([]byte, 64))
	if _, e = conn.Read(buf); e == nil {
		t.Fatal("unauthenticated packet echoed")
	}
	cancel()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(time.Second):
		t.Fatal("endpoint did not stop")
	}
}
func TestSOCKSAddressRejectsTruncation(t *testing.T) {
	for _, b := range [][]byte{{9}, {1, 127}, {3, 9, 'x'}, {4, 0}} {
		if _, e := readSOCKSAddress(bytes.NewReader(b)); e == nil {
			t.Fatal("accepted invalid address")
		}
	}
}
