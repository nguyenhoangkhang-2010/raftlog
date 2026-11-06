package chaos

import (
	"net"
	"sync"
	"time"
)

// link is a TCP proxy for the traffic that one node sends to another. Putting a
// proxy on every ordered pair of nodes lets a test cut or slow one direction of
// one connection between two nodes, without any tool outside the test.
type link struct {
	target string // where the traffic goes: the real address of the receiving node
	ln     net.Listener

	mu    sync.Mutex
	down  bool
	delay time.Duration
	conns map[net.Conn]struct{}
	wg    sync.WaitGroup
}

func newLink(target string) (*link, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, err
	}
	l := &link{target: target, ln: ln, conns: make(map[net.Conn]struct{})}
	l.wg.Add(1)
	go l.accept()
	return l, nil
}

// addr is the address the sending node is told to use for the receiving node.
func (l *link) addr() string { return l.ln.Addr().String() }

func (l *link) accept() {
	defer l.wg.Done()
	for {
		c, err := l.ln.Accept()
		if err != nil {
			return
		}
		l.wg.Add(1)
		go func() {
			defer l.wg.Done()
			l.handle(c)
		}()
	}
}

// track remembers a connection so that setDown can break it. It reports false
// (and closes the connection) if the link is down.
func (l *link) track(c net.Conn) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.down {
		_ = c.Close()
		return false
	}
	l.conns[c] = struct{}{}
	return true
}

func (l *link) handle(client net.Conn) {
	if !l.track(client) {
		return
	}
	server, err := net.DialTimeout("tcp", l.target, time.Second)
	if err != nil {
		_ = client.Close()
		return
	}
	if !l.track(server) {
		_ = client.Close()
		return
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); l.pipe(server, client) }()
	go func() { defer wg.Done(); l.pipe(client, server) }()
	wg.Wait()

	l.mu.Lock()
	delete(l.conns, client)
	delete(l.conns, server)
	l.mu.Unlock()
}

// pipe copies from src to dst. Each chunk is held back by the current delay,
// which keeps the order of the bytes and models a slow network.
func (l *link) pipe(dst, src net.Conn) {
	buf := make([]byte, 32*1024)
	for {
		n, err := src.Read(buf)
		if n > 0 {
			if d := l.currentDelay(); d > 0 {
				time.Sleep(d)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				break
			}
		}
		if err != nil {
			break
		}
	}
	_ = dst.Close()
	_ = src.Close()
}

func (l *link) currentDelay() time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.delay
}

// setDown cuts the link (breaking every open connection and refusing new ones)
// or restores it.
func (l *link) setDown(down bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.down = down
	if down {
		for c := range l.conns {
			_ = c.Close()
		}
		l.conns = make(map[net.Conn]struct{})
	}
}

func (l *link) setDelay(d time.Duration) {
	l.mu.Lock()
	l.delay = d
	l.mu.Unlock()
}

func (l *link) close() {
	_ = l.ln.Close()
	l.setDown(true)
	l.wg.Wait()
}
