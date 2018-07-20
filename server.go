package qrpc

import (
	"context"
	"errors"
	"net"
	"sync"
	"sync/atomic"
	"time"
)

// FrameWriter looks like writes a qrpc resp
// but it internally needs be scheduled, thus maintains a simple yet powerful interface
type FrameWriter interface {
	StartWrite(requestID uint64, cmd Cmd, flags PacketFlag)
	WriteBytes(v []byte) // v is copied in WriteBytes
	EndWrite() error     // block until scheduled
}

// A Handler responds to an qrpc request.
type Handler interface {
	ServeQRPC(FrameWriter, *RequestFrame)
}

// The HandlerFunc type is an adapter to allow the use of
// ordinary functions as qrpc handlers. If f is a function
// with the appropriate signature, HandlerFunc(f) is a
// Handler that calls f.
type HandlerFunc func(FrameWriter, *RequestFrame)

// ServeQRPC calls f(w, r).
func (f HandlerFunc) ServeQRPC(w FrameWriter, r *RequestFrame) {
	f(w, r)
}

// ServeMux is qrpc request multiplexer.
type ServeMux struct {
	mu sync.RWMutex
	m  map[Cmd]Handler
}

// NewServeMux allocates and returns a new ServeMux.
func NewServeMux() *ServeMux { return new(ServeMux) }

// HandleFunc registers the handler function for the given pattern.
func (mux *ServeMux) HandleFunc(cmd Cmd, handler func(FrameWriter, *RequestFrame)) {
	mux.Handle(cmd, HandlerFunc(handler))
}

// Handle registers the handler for the given pattern.
// If a handler already exists for pattern, handle panics.
func (mux *ServeMux) Handle(cmd Cmd, handler Handler) {
	mux.mu.Lock()
	defer mux.mu.Unlock()

	if handler == nil {
		panic("qrpc: nil handler")
	}
	if _, exist := mux.m[cmd]; exist {
		panic("qrpc: multiple registrations for " + string(cmd))
	}

	if mux.m == nil {
		mux.m = make(map[Cmd]Handler)
	}
	mux.m[cmd] = handler
}

// ServeQRPC dispatches the request to the handler whose
// cmd matches the request.
func (mux *ServeMux) ServeQRPC(w FrameWriter, r *RequestFrame) {
	mux.mu.RLock()
	h, ok := mux.m[r.Cmd]
	if !ok {
		// TODO error response
		return
	}
	mux.mu.RUnlock()
	h.ServeQRPC(w, r)
}

// Server defines parameters for running an qrpc server.
type Server struct {
	// one handler for each listening address
	bindings []ServerBinding

	// manages below
	mu         sync.Mutex
	listeners  map[net.Listener]struct{}
	doneChan   chan struct{}
	id2Conn    []map[string]*serveconn
	activeConn []sync.Map // for better iterate when write, map[*serveconn]struct{}

	wg sync.WaitGroup // wait group for goroutines

	pushID uint64
}

// NewServer creates a server
func NewServer(bindings []ServerBinding) *Server {
	return &Server{
		bindings:   bindings,
		listeners:  make(map[net.Listener]struct{}),
		doneChan:   make(chan struct{}),
		id2Conn:    []map[string]*serveconn{map[string]*serveconn{}, map[string]*serveconn{}},
		activeConn: make([]sync.Map, len(bindings))}
}

// ListenAndServe starts listening on all bindings
func (srv *Server) ListenAndServe() error {

	for idx, binding := range srv.bindings {
		ln, err := net.Listen("tcp", binding.Addr)
		if err != nil {
			srv.Shutdown()
			return err
		}

		goFunc(&srv.wg, func(idx int) func() {
			return func() {
				srv.serve(tcpKeepAliveListener{ln.(*net.TCPListener)}, idx)
			}
		}(idx))

	}

	srv.wg.Wait()
	return nil
}

// ErrServerClosed is returned by the Server's Serve, ListenAndServe,
// methods after a call to Shutdown or Close.
var ErrServerClosed = errors.New("qrpc: Server closed")

var defaultAcceptTimeout = 5 * time.Second

// serve accepts incoming connections on the Listener l, creating a
// new service goroutine for each. The service goroutines read requests and
// then call srv.Handler to reply to them.
//
// serve always returns a non-nil error. After Shutdown or Close, the
// returned error is ErrServerClosed.
func (srv *Server) serve(l tcpKeepAliveListener, idx int) error {

	defer l.Close()
	var tempDelay time.Duration // how long to sleep on accept failure

	srv.trackListener(l, true)
	defer srv.trackListener(l, false)

	serveCtx, cancelFunc := context.WithCancel(context.Background())
	defer cancelFunc()
	for {
		l.SetDeadline(time.Now().Add(defaultAcceptTimeout))
		rw, e := l.AcceptTCP()
		if e != nil {
			select {
			case <-srv.doneChan:
				return ErrServerClosed
			default:
			}
			if opError, ok := e.(*net.OpError); ok && opError.Timeout() {
				// don't log the scheduled timeout
				continue
			}
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				if tempDelay == 0 {
					tempDelay = 5 * time.Millisecond
				} else {
					tempDelay *= 2
				}
				if max := 1 * time.Second; tempDelay > max {
					tempDelay = max
				}
				srv.logf("http: Accept error: %v; retrying in %v", e, tempDelay)
				time.Sleep(tempDelay)
				continue
			}
			return e
		}
		tempDelay = 0
		c := srv.newConn(rw, idx)

		goFunc(&srv.wg, func() {
			c.serve(serveCtx)
		})
	}
}

// tcpKeepAliveListener sets TCP keep-alive timeouts on accepted
// connections.
type tcpKeepAliveListener struct {
	*net.TCPListener
}

func (ln tcpKeepAliveListener) Accept() (net.Conn, error) {
	tc, err := ln.AcceptTCP()
	if err != nil {
		return nil, err
	}
	tc.SetKeepAlive(true)
	tc.SetKeepAlivePeriod(3 * time.Minute)
	return tc, nil
}

func (srv *Server) trackListener(ln net.Listener, add bool) {
	srv.mu.Lock()
	defer srv.mu.Unlock()
	if add {
		srv.listeners[ln] = struct{}{}
	} else {
		delete(srv.listeners, ln)
	}
}

// Create new connection from rwc.
func (srv *Server) newConn(rwc net.Conn, idx int) *serveconn {
	c := &serveconn{
		server:       srv,
		rwc:          rwc,
		idx:          idx,
		closeCh:      make(chan struct{}),
		readFrameCh:  make(chan readFrameResult),
		writeFrameCh: make(chan writeFrameRequest)}

	srv.activeConn[idx].Store(c, struct{}{})
	return c
}

// bindID bind the id to sc
func (srv *Server) bindID(sc *serveconn, id string) {

	idx := sc.idx
	srv.mu.Lock()
	defer srv.mu.Unlock()
	v, ok := srv.id2Conn[idx][id]
	if ok {
		if v == sc {
			return
		}
		ch, _ := v.closeLocked(true)
		<-ch
	}

	srv.id2Conn[idx][id] = sc
}

func (srv *Server) untrack(sc *serveconn, inLock bool) {

	idx := sc.idx
	if !inLock {
		srv.mu.Lock()
		defer srv.mu.Unlock()
	}

	if sc.id != "" {
		delete(srv.id2Conn[idx], sc.id)
	}
	srv.activeConn[idx].Delete(sc)

}

func (srv *Server) logf(format string, args ...interface{}) {

}

var shutdownPollInterval = 500 * time.Millisecond

// Shutdown gracefully shutdown the server
func (srv *Server) Shutdown() error {

	srv.mu.Lock()
	lnerr := srv.closeListenersLocked()
	if lnerr != nil {
		return lnerr
	}
	srv.mu.Unlock()

	close(srv.doneChan)

	srv.wg.Wait()

	return nil
}

// PushFrame pushes a frame to specified connection
// it is thread safe
func (srv *Server) PushFrame(conn *serveconn, cmd Cmd, flags PacketFlag, payload []byte) error {

	pushID := atomic.AddUint64(&srv.pushID, 1)
	flags &= PushFlag
	w := conn.GetWriter()
	w.StartWrite(pushID, cmd, flags)
	w.WriteBytes(payload)
	return w.EndWrite()

}

func (srv *Server) closeListenersLocked() error {
	var err error
	for ln := range srv.listeners {
		if cerr := ln.Close(); cerr != nil && err == nil {
			err = cerr
		}
		delete(srv.listeners, ln)
	}
	return err
}
