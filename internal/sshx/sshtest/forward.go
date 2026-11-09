package sshtest

import (
	"encoding/binary"
	"io"
	"net"
	"strconv"
	"sync"
	"sync/atomic"

	"golang.org/x/crypto/ssh"
)

// forwards serves one connection's remote forwards ("ssh -R"): each
// tcpip-forward request listens on 127.0.0.1 and sends accepted
// connections back as forwarded-tcpip channels.
type forwards struct {
	conn   *ssh.ServerConn
	refuse *atomic.Bool // refuse every tcpip-forward

	mu        sync.Mutex
	listeners map[string]net.Listener // by "addr:port" as the client asked
	closed    bool
}

type forwardReq struct {
	Addr string
	Port uint32
}

func (f *forwards) serve(reqs <-chan *ssh.Request) {
	for req := range reqs {
		switch req.Type {
		case "tcpip-forward":
			if f.refuse != nil && f.refuse.Load() {
				req.Reply(false, nil)
				continue
			}
			f.listen(req)
		case "cancel-tcpip-forward":
			var p forwardReq
			ssh.Unmarshal(req.Payload, &p)
			f.mu.Lock()
			key := net.JoinHostPort(p.Addr, strconv.Itoa(int(p.Port)))
			ln, ok := f.listeners[key]
			delete(f.listeners, key)
			f.mu.Unlock()
			if ok {
				ln.Close()
			}
			req.Reply(ok, nil)
		default:
			if req.WantReply {
				req.Reply(false, nil)
			}
		}
	}
}

func (f *forwards) listen(req *ssh.Request) {
	var p forwardReq
	if err := ssh.Unmarshal(req.Payload, &p); err != nil {
		req.Reply(false, nil)
		return
	}
	ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(int(p.Port))))
	if err != nil {
		req.Reply(false, nil)
		return
	}
	port := uint32(ln.Addr().(*net.TCPAddr).Port)
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		ln.Close()
		req.Reply(false, nil)
		return
	}
	if f.listeners == nil {
		f.listeners = map[string]net.Listener{}
	}
	f.listeners[net.JoinHostPort(p.Addr, strconv.Itoa(int(port)))] = ln
	f.mu.Unlock()
	reply := make([]byte, 4)
	binary.BigEndian.PutUint32(reply, port)
	req.Reply(true, reply)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.forward(c, p.Addr, port)
		}
	}()
}

// forward sends c to the client as a forwarded-tcpip channel.
func (f *forwards) forward(c net.Conn, addr string, port uint32) {
	defer c.Close()
	orig := c.RemoteAddr().(*net.TCPAddr)
	payload := ssh.Marshal(struct {
		Addr       string
		Port       uint32
		OriginAddr string
		OriginPort uint32
	}{addr, port, orig.IP.String(), uint32(orig.Port)})
	ch, reqs, err := f.conn.OpenChannel("forwarded-tcpip", payload)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	go func() { io.Copy(ch, c); ch.CloseWrite() }()
	io.Copy(c, ch)
	ch.Close()
}

// closeAll stops every listener; the connection has ended.
func (f *forwards) closeAll() {
	f.mu.Lock()
	f.closed = true
	ls := f.listeners
	f.listeners = nil
	f.mu.Unlock()
	for _, ln := range ls {
		ln.Close()
	}
}
