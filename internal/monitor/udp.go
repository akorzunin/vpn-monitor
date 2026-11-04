package monitor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"strconv"
	"time"
)

// Authenticated fixed-size echo: 32 random bytes followed by HMAC-SHA256.
// No unauthenticated responses or amplification; restrict the endpoint firewall too.
func Echo(ctx context.Context, address, token string) error {
	if len(token) < 24 {
		return errors.New("echo secret must contain at least 24 characters")
	}
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	pc, err := net.ListenPacket("udp", address)
	if err != nil {
		return err
	}
	defer pc.Close()
	go func() { <-ctx.Done(); _ = pc.Close() }()
	buf := make([]byte, 2048)
	for {
		n, peer, e := pc.ReadFrom(buf)
		if e != nil {
			if ctx.Err() != nil {
				return nil
			}
			return e
		}
		if n != 64 {
			continue
		}
		mac := hmac.New(sha256.New, []byte(token))
		mac.Write(buf[:32])
		if !hmac.Equal(mac.Sum(nil), buf[32:64]) {
			continue
		}
		_, _ = pc.WriteTo(buf[:n], peer)
	}
}
func readSOCKSAddress(r io.Reader) (string, error) {
	var typ [1]byte
	if _, e := io.ReadFull(r, typ[:]); e != nil {
		return "", e
	}
	var host string
	switch typ[0] {
	case 1:
		b := make([]byte, 4)
		if _, e := io.ReadFull(r, b); e != nil {
			return "", e
		}
		host = net.IP(b).String()
	case 4:
		b := make([]byte, 16)
		if _, e := io.ReadFull(r, b); e != nil {
			return "", e
		}
		host = net.IP(b).String()
	case 3:
		var n [1]byte
		if _, e := io.ReadFull(r, n[:]); e != nil {
			return "", e
		}
		b := make([]byte, int(n[0]))
		if _, e := io.ReadFull(r, b); e != nil {
			return "", e
		}
		host = string(b)
	default:
		return "", errors.New("invalid SOCKS address")
	}
	var p [2]byte
	if _, e := io.ReadFull(r, p[:]); e != nil {
		return "", e
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(p[:])))), nil
}
func udpTest(ctx context.Context, address string, cfg UDPConfig) (result UDPResult) {
	result = UDPResult{State: "error"}
	var rtts []float64
	defer func() {
		if result.Sent > 0 {
			result.LossPercent = 100 * float64(result.Sent-result.Received) / float64(result.Sent)
		}
		result.RTTMS, result.P95MS = latencies(rtts)
	}()
	dialer := net.Dialer{Timeout: cfg.Timeout}
	control, err := dialer.DialContext(ctx, "tcp", address)
	if err != nil {
		result.Error = "SOCKS connection failed"
		return result
	}
	defer control.Close()
	_ = control.SetDeadline(time.Now().Add(cfg.Timeout))
	if _, err = control.Write([]byte{5, 1, 0}); err != nil {
		result.Error = "SOCKS handshake failed"
		return result
	}
	var response [2]byte
	if _, err = io.ReadFull(control, response[:]); err != nil || response != [2]byte{5, 0} {
		result.Error = "SOCKS handshake failed"
		return result
	}
	if _, err = control.Write([]byte{5, 3, 0, 1, 0, 0, 0, 0, 0, 0}); err != nil {
		result.Error = "UDP associate failed"
		return result
	}
	var header [3]byte
	if _, err = io.ReadFull(control, header[:]); err != nil || header != [3]byte{5, 0, 0} {
		result.Error = "UDP associate rejected"
		return result
	}
	relay, err := readSOCKSAddress(control)
	if err != nil {
		result.Error = "invalid UDP relay"
		return result
	}
	h, p, _ := net.SplitHostPort(relay)
	ip := net.ParseIP(h)
	if ip == nil {
		result.Error = "non-IP UDP relay rejected"
		return result
	}
	if ip.IsUnspecified() {
		h = "127.0.0.1"
	} else if !ip.IsLoopback() {
		result.Error = "non-loopback UDP relay rejected"
		return result
	}
	udp, err := dialer.DialContext(ctx, "udp", net.JoinHostPort(h, p))
	if err != nil {
		result.Error = "UDP relay unavailable"
		return result
	}
	defer udp.Close()
	target, err := net.ResolveUDPAddr("udp", cfg.Endpoint)
	if err != nil {
		result.Error = "invalid endpoint"
		return result
	}
	prefix := []byte{0, 0, 0}
	if ip4 := target.IP.To4(); ip4 != nil {
		prefix = append(prefix, 1)
		prefix = append(prefix, ip4...)
	} else {
		prefix = append(prefix, 4)
		prefix = append(prefix, target.IP.To16()...)
	}
	prefix = binary.BigEndian.AppendUint16(prefix, uint16(target.Port))
	buf := make([]byte, 2048)
	for i := 0; i < cfg.Samples; i++ {
		if ctx.Err() != nil {
			result.Error = "cancelled"
			return result
		}
		payload := make([]byte, 32)
		if _, err = rand.Read(payload); err != nil {
			result.Error = "random source failed"
			return result
		}
		mac := hmac.New(sha256.New, []byte(cfg.Token))
		mac.Write(payload)
		payload = append(payload, mac.Sum(nil)...)
		packet := append(append([]byte{}, prefix...), payload...)
		start := time.Now()
		_ = udp.SetDeadline(start.Add(cfg.Timeout))
		result.Sent++
		if _, err = udp.Write(packet); err != nil {
			continue
		}
		for {
			n, e := udp.Read(buf)
			if e != nil {
				break
			}
			if n != len(prefix)+64 || !bytes.Equal(buf[:len(prefix)], prefix) || !bytes.Equal(buf[len(prefix):n], payload) {
				continue
			}
			result.Received++
			rtts = append(rtts, float64(time.Since(start).Microseconds())/1000)
			break
		}
	}
	result.State = "ok"
	if result.Received == 0 {
		result.State = "failed"
	} else if result.Received < result.Sent {
		result.State = "degraded"
	}
	return result
}
