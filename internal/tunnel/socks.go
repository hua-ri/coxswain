package tunnel

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"slices"
	"strconv"
	"time"
)

// SOCKS5 (RFC 1928), the minimum: no authentication, CONNECT only, IPv4,
// IPv6 and domain names (resolved on the host, like ssh -D).

const (
	socksOK             = 0
	socksGeneralFailure = 1
	socksRefused        = 5
	socksBadCommand     = 7
	socksBadAddress     = 8
)

// handshakeTimeout bounds the SOCKS negotiation.
const handshakeTimeout = 30 * time.Second

var errSocks = errors.New("socks: bad request")

// socksHandshake negotiates with a client and returns the host:port it
// wants to reach. On error the client has been told, if it can be.
func socksHandshake(c net.Conn) (string, error) {
	c.SetDeadline(time.Now().Add(handshakeTimeout))
	defer c.SetDeadline(time.Time{})
	var hdr [2]byte
	if _, err := io.ReadFull(c, hdr[:]); err != nil {
		return "", err
	}
	if hdr[0] != 5 {
		return "", errSocks
	}
	methods := make([]byte, hdr[1])
	if _, err := io.ReadFull(c, methods); err != nil {
		return "", err
	}
	if !slices.Contains(methods, 0) {
		c.Write([]byte{5, 0xff})
		return "", errSocks
	}
	if _, err := c.Write([]byte{5, 0}); err != nil {
		return "", err
	}
	var req [4]byte // VER CMD RSV ATYP
	if _, err := io.ReadFull(c, req[:]); err != nil {
		return "", err
	}
	if req[0] != 5 {
		return "", errSocks
	}
	if req[1] != 1 {
		socksReply(c, socksBadCommand)
		return "", errSocks
	}
	var host string
	switch req[3] {
	case 1, 4:
		ip := make([]byte, 4)
		if req[3] == 4 {
			ip = make([]byte, 16)
		}
		if _, err := io.ReadFull(c, ip); err != nil {
			return "", err
		}
		host = net.IP(ip).String()
	case 3:
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return "", err
		}
		name := make([]byte, n[0])
		if _, err := io.ReadFull(c, name); err != nil {
			return "", err
		}
		if len(name) == 0 {
			socksReply(c, socksBadAddress)
			return "", errSocks
		}
		host = string(name)
	default:
		socksReply(c, socksBadAddress)
		return "", errSocks
	}
	var port [2]byte
	if _, err := io.ReadFull(c, port[:]); err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(int(binary.BigEndian.Uint16(port[:])))), nil
}

// socksReply answers a CONNECT request (bound address 0.0.0.0:0).
func socksReply(c net.Conn, code byte) error {
	_, err := c.Write([]byte{5, code, 0, 1, 0, 0, 0, 0, 0, 0})
	return err
}
