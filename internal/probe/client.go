package probe

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// RouteCheck proves that the already established tunnel belongs to the
// expected live Mihomo route. It runs before an HTTP request is sent.
type RouteCheck func(context.Context, net.Conn) error

var ErrRoute = errors.New("live proxy route verification failed")

func openTunnel(ctx context.Context, proxyURL string, target *url.URL) (net.Conn, error) {
	destination, err := targetAddress(target)
	if err != nil {
		return nil, err
	}
	if proxyURL == "" {
		return (&net.Dialer{}).DialContext(ctx, "tcp", destination)
	}
	proxy, err := url.Parse(proxyURL)
	if err != nil {
		return nil, fmt.Errorf("parse proxy URL: %w", err)
	}
	switch strings.ToLower(proxy.Scheme) {
	case "http", "https":
		return dialHTTPTunnel(ctx, proxy, destination)
	case "socks5", "socks5h":
		dialer, err := newSOCKS5Dialer(proxy)
		if err != nil {
			return nil, err
		}
		return dialer.DialContext(ctx, "tcp", destination)
	default:
		return nil, fmt.Errorf("unsupported proxy scheme %q", proxy.Scheme)
	}
}

func targetAddress(target *url.URL) (string, error) {
	if target.Scheme != "http" && target.Scheme != "https" {
		return "", fmt.Errorf("unsupported target URL scheme %q", target.Scheme)
	}
	if target.Hostname() == "" {
		return "", errors.New("target URL has no host")
	}
	port := target.Port()
	if port == "" {
		if target.Scheme == "https" {
			port = "443"
		} else {
			port = "80"
		}
	}
	return net.JoinHostPort(target.Hostname(), port), nil
}

func dialHTTPTunnel(ctx context.Context, proxy *url.URL, destination string) (_ net.Conn, err error) {
	if proxy.Hostname() == "" {
		return nil, errors.New("HTTP proxy URL has no host")
	}
	proxyPort := proxy.Port()
	if proxyPort == "" {
		if strings.EqualFold(proxy.Scheme, "https") {
			proxyPort = "443"
		} else {
			proxyPort = "80"
		}
	}
	connection, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(proxy.Hostname(), proxyPort))
	if err != nil {
		return nil, fmt.Errorf("connect to HTTP proxy: %w", err)
	}
	rawConnection := connection
	stopCancellation := context.AfterFunc(ctx, func() { _ = rawConnection.Close() })
	defer func() {
		_ = stopCancellation()
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	ok := false
	defer func() {
		if !ok {
			_ = connection.Close()
		}
	}()
	if deadline, exists := ctx.Deadline(); exists {
		_ = connection.SetDeadline(deadline)
	}
	if strings.EqualFold(proxy.Scheme, "https") {
		tlsConnection := tls.Client(connection, &tls.Config{ServerName: proxy.Hostname(), MinVersion: tls.VersionTLS12})
		if err := tlsConnection.HandshakeContext(ctx); err != nil {
			return nil, fmt.Errorf("TLS handshake with HTTP proxy: %w", err)
		}
		connection = tlsConnection
	}

	request := &http.Request{
		Method: http.MethodConnect,
		URL:    &url.URL{Opaque: destination},
		Host:   destination,
		Header: make(http.Header),
	}
	if proxy.User != nil {
		password, _ := proxy.User.Password()
		credentials := base64.StdEncoding.EncodeToString([]byte(proxy.User.Username() + ":" + password))
		request.Header.Set("Proxy-Authorization", "Basic "+credentials)
	}
	if err := request.Write(connection); err != nil {
		return nil, fmt.Errorf("write HTTP CONNECT request: %w", err)
	}
	response, err := http.ReadResponse(bufio.NewReader(connection), request)
	if err != nil {
		return nil, fmt.Errorf("read HTTP CONNECT response: %w", err)
	}
	if response.StatusCode != http.StatusOK {
		_ = response.Body.Close()
		return nil, fmt.Errorf("HTTP CONNECT returned %s", response.Status)
	}
	if !stopCancellation() {
		return nil, ctx.Err()
	}
	_ = connection.SetDeadline(time.Time{})
	ok = true
	return connection, nil
}

func verifyTunnel(ctx context.Context, connection net.Conn, check RouteCheck) error {
	if check == nil {
		return fmt.Errorf("%w: missing verifier", ErrRoute)
	}
	if err := check(ctx, connection); err != nil {
		return fmt.Errorf("%w: %w", ErrRoute, err)
	}
	return nil
}

type oneConnDialer struct {
	mu   sync.Mutex
	conn net.Conn
}

func (d *oneConnDialer) DialContext(context.Context, string, string) (net.Conn, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.conn == nil {
		return nil, errors.New("verified tunnel was already consumed")
	}
	connection := d.conn
	d.conn = nil
	return connection, nil
}

func clientForTunnel(connection net.Conn) *http.Client {
	dialer := &oneConnDialer{conn: connection}
	return &http.Client{
		Transport: &http.Transport{
			Proxy:               nil,
			DialContext:         dialer.DialContext,
			DisableKeepAlives:   true,
			TLSHandshakeTimeout: 10 * time.Second,
			TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12},
		},
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type socks5Dialer struct {
	address  string
	username string
	password string
}

func newSOCKS5Dialer(proxy *url.URL) (*socks5Dialer, error) {
	if proxy.Host == "" {
		return nil, errors.New("SOCKS5 proxy URL has no host")
	}
	username := ""
	password := ""
	if proxy.User != nil {
		username = proxy.User.Username()
		password, _ = proxy.User.Password()
		if len(username) > 255 || len(password) > 255 {
			return nil, errors.New("SOCKS5 credentials exceed 255 bytes")
		}
	}
	return &socks5Dialer{address: proxy.Host, username: username, password: password}, nil
}

func (d *socks5Dialer) DialContext(ctx context.Context, network, address string) (_ net.Conn, err error) {
	connection, err := (&net.Dialer{}).DialContext(ctx, network, d.address)
	if err != nil {
		return nil, err
	}
	stopCancellation := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer func() {
		_ = stopCancellation()
		if err != nil && ctx.Err() != nil {
			err = ctx.Err()
		}
	}()
	ok := false
	defer func() {
		if !ok {
			_ = connection.Close()
		}
	}()
	if deadline, exists := ctx.Deadline(); exists {
		_ = connection.SetDeadline(deadline)
	}

	methods := []byte{0x00}
	if d.username != "" || d.password != "" {
		methods = append(methods, 0x02)
	}
	if _, err := connection.Write(append([]byte{0x05, byte(len(methods))}, methods...)); err != nil {
		return nil, fmt.Errorf("SOCKS5 greeting: %w", err)
	}
	selection := []byte{0, 0}
	if _, err := io.ReadFull(connection, selection); err != nil {
		return nil, fmt.Errorf("SOCKS5 greeting response: %w", err)
	}
	if selection[0] != 0x05 {
		return nil, fmt.Errorf("SOCKS5 proxy returned version %d", selection[0])
	}
	switch selection[1] {
	case 0x00:
	case 0x02:
		if err := d.authenticate(connection); err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("SOCKS5 proxy selected unsupported authentication method %d", selection[1])
	}

	request, err := socks5ConnectRequest(address)
	if err != nil {
		return nil, err
	}
	if _, err := connection.Write(request); err != nil {
		return nil, fmt.Errorf("SOCKS5 CONNECT request: %w", err)
	}
	reader := bufio.NewReader(connection)
	header := make([]byte, 4)
	if _, err := io.ReadFull(reader, header); err != nil {
		return nil, fmt.Errorf("SOCKS5 CONNECT response: %w", err)
	}
	if header[0] != 0x05 || header[1] != 0x00 {
		return nil, fmt.Errorf("SOCKS5 CONNECT failed with code %d", header[1])
	}
	if err := discardSOCKS5Address(reader, header[3]); err != nil {
		return nil, err
	}
	if !stopCancellation() {
		return nil, ctx.Err()
	}
	_ = connection.SetDeadline(time.Time{})
	ok = true
	return connection, nil
}

func (d *socks5Dialer) authenticate(connection net.Conn) error {
	request := []byte{0x01, byte(len(d.username))}
	request = append(request, d.username...)
	request = append(request, byte(len(d.password)))
	request = append(request, d.password...)
	if _, err := connection.Write(request); err != nil {
		return fmt.Errorf("SOCKS5 authentication: %w", err)
	}
	response := []byte{0, 0}
	if _, err := io.ReadFull(connection, response); err != nil {
		return fmt.Errorf("SOCKS5 authentication response: %w", err)
	}
	if response[1] != 0x00 {
		return errors.New("SOCKS5 authentication failed")
	}
	return nil
}

func socks5ConnectRequest(address string) ([]byte, error) {
	host, portText, err := net.SplitHostPort(address)
	if err != nil {
		return nil, fmt.Errorf("parse SOCKS5 destination: %w", err)
	}
	port, err := strconv.ParseUint(portText, 10, 16)
	if err != nil {
		return nil, fmt.Errorf("parse SOCKS5 destination port: %w", err)
	}
	request := []byte{0x05, 0x01, 0x00}
	if ip := net.ParseIP(host); ip != nil {
		if ipv4 := ip.To4(); ipv4 != nil {
			request = append(request, 0x01)
			request = append(request, ipv4...)
		} else {
			request = append(request, 0x04)
			request = append(request, ip.To16()...)
		}
	} else {
		if len(host) > 255 {
			return nil, errors.New("SOCKS5 destination hostname exceeds 255 bytes")
		}
		request = append(request, 0x03, byte(len(host)))
		request = append(request, host...)
	}
	portBytes := []byte{0, 0}
	binary.BigEndian.PutUint16(portBytes, uint16(port))
	return append(request, portBytes...), nil
}

func discardSOCKS5Address(reader io.Reader, addressType byte) error {
	length := 0
	switch addressType {
	case 0x01:
		length = net.IPv4len
	case 0x04:
		length = net.IPv6len
	case 0x03:
		value := []byte{0}
		if _, err := io.ReadFull(reader, value); err != nil {
			return fmt.Errorf("read SOCKS5 bound hostname length: %w", err)
		}
		length = int(value[0])
	default:
		return fmt.Errorf("SOCKS5 proxy returned unknown address type %d", addressType)
	}
	_, err := io.CopyN(io.Discard, reader, int64(length+2))
	if err != nil {
		return fmt.Errorf("read SOCKS5 bound address: %w", err)
	}
	return nil
}
