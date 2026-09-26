package peer

import (
	"errors"
	"net"
	"sync"
)

// CloseSignalConn wraps a net.Conn and signals via Done the first time
// Close() is called. The phone-home server uses it to stop the single-conn
// listener as soon as the host drops the connection.
type CloseSignalConn struct {
	net.Conn
	once   sync.Once
	doneCh chan struct{}
}

// NewCloseSignalConn wraps conn for phone-home serving.
func NewCloseSignalConn(conn net.Conn) *CloseSignalConn {
	return &CloseSignalConn{Conn: conn, doneCh: make(chan struct{})}
}

func (c *CloseSignalConn) Close() error {
	c.once.Do(func() { close(c.doneCh) })
	return c.Conn.Close()
}

// Done is closed the first time Close is called (including host-initiated
// close on the wrapped connection).
func (c *CloseSignalConn) Done() <-chan struct{} { return c.doneCh }

// SingleConnListener is a net.Listener that returns a pre-opened connection
// on its first Accept() and then blocks until Close() is called. It lets a
// grpc.Server serve on a connection that was dialed outbound (the phone-home
// model: the host accepts by dialing, so both endpoints are clients at the
// transport level).
type SingleConnListener struct {
	conn   net.Conn
	mu     sync.Mutex
	used   bool
	closed chan struct{}
}

// NewSingleConnListener serves exactly one connection through Accept.
func NewSingleConnListener(conn net.Conn) *SingleConnListener {
	return &SingleConnListener{conn: conn, closed: make(chan struct{})}
}

func (l *SingleConnListener) Accept() (net.Conn, error) {
	l.mu.Lock()
	if !l.used {
		l.used = true
		l.mu.Unlock()
		return l.conn, nil
	}
	l.mu.Unlock()
	<-l.closed
	return nil, errors.New("listener closed")
}

func (l *SingleConnListener) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	select {
	case <-l.closed:
	default:
		close(l.closed)
	}
	return nil
}

func (l *SingleConnListener) Addr() net.Addr {
	if l.conn != nil {
		return l.conn.LocalAddr()
	}
	return nil
}