// Copyright 2023 Jigsaw Operations LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package socks5

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"net/netip"
	"strconv"

	"github.com/Jigsaw-Code/outline-sdk/transport"
)

// https://datatracker.ietf.org/doc/html/rfc1929
// Credentials can be nil, and that means no authentication.
type credentials struct {
	username []byte
	password []byte
}

// NewDialer creates a [transport.StreamDialer] that routes connections to a SOCKS5
// proxy listening at the given [transport.StreamEndpoint].
func NewDialer(streamEndpoint transport.StreamEndpoint) (*Dialer, error) {
	if streamEndpoint == nil {
		return nil, errors.New("argument endpoint must not be nil")
	}
	return &Dialer{se: streamEndpoint, cred: nil}, nil
}

type Dialer struct {
	se   transport.StreamEndpoint
	pd   transport.PacketDialer
	cred *credentials
}

var _ transport.StreamDialer = (*Dialer)(nil)
var _ transport.PacketDialer = (*Dialer)(nil)

func (d *Dialer) SetCredentials(username, password []byte) error {
	if len(username) > 255 {
		return errors.New("username exceeds 255 bytes")
	}
	if len(username) == 0 {
		return errors.New("username must be at least 1 byte")
	}

	if len(password) > 255 {
		return errors.New("password exceeds 255 bytes")
	}
	if len(password) == 0 {
		return errors.New("password must be at least 1 byte")
	}

	d.cred = &credentials{username: username, password: password}
	return nil
}

func (d *Dialer) EnablePacket(packetDialer transport.PacketDialer) {
	d.pd = packetDialer
}

func (d *Dialer) request(ctx context.Context, cmd byte, dstAddr string) (transport.StreamConn, string, error) {
	proxyConn, err := d.se.ConnectStream(ctx)
	if err != nil {
		return nil, "", fmt.Errorf("could not connect to SOCKS5 proxy: %w", err)
	}
	dialSuccess := false
	defer func() {
		if !dialSuccess {
			proxyConn.Close()
		}
	}()

	// For protocol details, see https://datatracker.ietf.org/doc/html/rfc1928#section-3
	// Creating a single buffer for method selection, authentication, and connection request
	// Buffer large enough for method, auth, and connect requests with a domain name address.
	// The maximum buffer size is:
	// 3 (1 socks version + 1 method selection + 1 methods)
	// + 1 (auth version) + 1 (username length) + 255 (username) + 1 (password length) + 255 (password)
	// + 256 (max domain name length)
	var buffer [(1 + 1 + 1) + (1 + 1 + 255 + 1 + 255) + 256]byte
	var b []byte

	if d.cred == nil {
		// Method selection part: VER = 5, NMETHODS = 1, METHODS = 0 (no auth)
		// +----+----------+----------+
		// |VER | NMETHODS | METHODS  |
		// +----+----------+----------+
		// | 1  |    1     | 1 to 255 |
		// +----+----------+----------+
		b = append(buffer[:0], 5, 1, 0)
	} else {
		// https://datatracker.ietf.org/doc/html/rfc1929
		// Method selection part: VER = 5, NMETHODS = 1, METHODS = 2 (username/password)
		b = append(buffer[:0], 5, 1, authMethodUserPass)

		// Authentication part: VER = 1, ULEN = 1, UNAME = 1~255, PLEN = 1, PASSWD = 1~255
		// +----+------+----------+------+----------+
		// |VER | ULEN |  UNAME   | PLEN |  PASSWD  |
		// +----+------+----------+------+----------+
		// | 1  |  1   | 1 to 255 |  1   | 1 to 255 |
		// +----+------+----------+------+----------+
		b = append(b, 1)
		b = append(b, byte(len(d.cred.username)))
		b = append(b, d.cred.username...)
		b = append(b, byte(len(d.cred.password)))
		b = append(b, d.cred.password...)
	}

	// CMD Request:
	// VER = 5, CMD = cmd, RSV = 0, DST.ADDR, DST.PORT
	// +----+-----+-------+------+----------+----------+
	// |VER | CMD |  RSV  | ATYP | DST.ADDR | DST.PORT |
	// +----+-----+-------+------+----------+----------+
	// | 1  |  1  | X'00' |  1   | Variable |    2     |
	// +----+-----+-------+------+----------+----------+
	b = append(b, 5, cmd, 0)
	// TODO: Probably more memory efficient if remoteAddr is added to the buffer directly.
	b, err = appendSOCKS5Address(b, dstAddr)
	if err != nil {
		return nil, "", fmt.Errorf("failed to create SOCKS5 address: %w", err)
	}

	// We merge the method and CMD requests and only perform one write
	// because we send a single authentication method, so there's no point
	// in waiting for the response. This eliminates a roundtrip.
	_, err = proxyConn.Write(b)
	if err != nil {
		return nil, "", fmt.Errorf("failed to write combined SOCKS5 request: %w", err)
	}

	// Reading the response:
	// 1. Read method response (VER, METHOD).
	// +----+--------+
	// |VER | METHOD |
	// +----+--------+
	// | 1  |   1    |
	// +----+--------+
	// buffer[0]: VER, buffer[1]: METHOD
	// Reuse buffer for better performance.
	if _, err = io.ReadFull(proxyConn, buffer[:2]); err != nil {
		return nil, "", fmt.Errorf("failed to read method server response: %w", err)
	}
	if buffer[0] != 5 {
		return nil, "", fmt.Errorf("invalid protocol version %v. Expected 5", buffer[0])
	}

	switch buffer[1] {
	case authMethodNoAuth:
		// No authentication required.
	case authMethodUserPass:
		// 2. Read authentication version and status
		// VER = 1, STATUS = 0
		// +----+--------+
		// |VER | STATUS |
		// +----+--------+
		// | 1  |   1    |
		// +----+--------+
		// VER = 1 means the server should be expecting username/password authentication.
		// buffer[2]: VER, buffer[3]: STATUS
		if _, err = io.ReadFull(proxyConn, buffer[2:4]); err != nil {
			return nil, "", fmt.Errorf("failed to read authentication version and status: %w", err)
		}
		if buffer[2] != 1 {
			return nil, "", fmt.Errorf("invalid authentication version %v. Expected 1", buffer[2])
		}
		if buffer[3] != 0 {
			return nil, "", fmt.Errorf("authentication failed: %v", buffer[3])
		}
	default:
		return nil, "", fmt.Errorf("unsupported SOCKS authentication method %v. Expected 2", buffer[1])
	}

	// 3. Read connect response (VER, REP, RSV, ATYP, BND.ADDR, BND.PORT).
	// See https://datatracker.ietf.org/doc/html/rfc1928#section-6.
	// +----+-----+-------+------+----------+----------+
	// |VER | REP |  RSV  | ATYP | BND.ADDR | BND.PORT |
	// +----+-----+-------+------+----------+----------+
	// | 1  |  1  | X'00' |  1   | Variable |    2     |
	// +----+-----+-------+------+----------+----------+
	// buffer[0]: VER
	// buffer[1]: REP
	// buffer[2]: RSV
	// buffer[3]: ATYP
	if _, err = io.ReadFull(proxyConn, buffer[:4]); err != nil {
		return nil, "", fmt.Errorf("failed to read connect server response: %w", err)
	}

	if buffer[0] != 5 {
		return nil, "", fmt.Errorf("invalid protocol version %v. Expected 5", buffer[0])
	}

	// if REP is not 0, it means the server returned an error.
	if buffer[1] != 0 {
		return nil, "", ReplyCode(buffer[1])
	}

	// 4. Read BND.ADDR.
	host := ""
	var bndAddrLen int
	switch buffer[3] {
	case addrTypeIPv4:
		if _, err := io.ReadFull(proxyConn, buffer[:4]); err != nil {
			return nil, "", fmt.Errorf("failed to read bound IPv4 address: %w", err)
		}
		host = netip.AddrFrom4([4]byte(buffer[:4])).String()
	case addrTypeIPv6:
		if _, err := io.ReadFull(proxyConn, buffer[:16]); err != nil {
			return nil, "", fmt.Errorf("failed to read bound IPv6 address: %w", err)
		}
		host = netip.AddrFrom16([16]byte(buffer[:16])).String()
	case addrTypeDomainName:
		// read address length
		_, err := io.ReadFull(proxyConn, buffer[:1])
		if err != nil {
			return nil, "", fmt.Errorf("failed to read address length in connect response: %w", err)
		}
		bndAddrLen = int(buffer[0])
		if _, err := io.ReadFull(proxyConn, buffer[:bndAddrLen]); err != nil {
			return nil, "", fmt.Errorf("failed to read bound domain address: %w", err)
		}
		host = string(buffer[:bndAddrLen])
	default:
		return nil, "", fmt.Errorf("invalid address type %v", buffer[3])
	}

	// Read BND.PORT
	if _, err = io.ReadFull(proxyConn, buffer[:2]); err != nil {
		return nil, "", fmt.Errorf("failed to read bound port: %w", err)
	}
	port := binary.BigEndian.Uint16(buffer[:2])
	portStr := strconv.FormatUint(uint64(port), 10)
	bindAddr := net.JoinHostPort(host, portStr)

	dialSuccess = true
	return proxyConn, bindAddr, nil
}

// DialStream implements [transport.StreamDialer].DialStream using SOCKS5.
// It will send the auth method, auth credentials (if auth is chosen), and
// the connect requests in one packet, to avoid an additional roundtrip.
// The returned [error] will be of type [ReplyCode] if the server sends a SOCKS error reply code, which
// you can check against the error constants in this package using [errors.Is].
func (d *Dialer) DialStream(ctx context.Context, dstAddr string) (transport.StreamConn, error) {
	proxyConn, _, err := d.request(ctx, CmdConnect, dstAddr)
	if err != nil {
		return nil, err
	}
	return proxyConn, nil
}
