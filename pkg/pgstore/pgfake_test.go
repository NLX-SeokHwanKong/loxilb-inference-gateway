package pgstore

// pgfake is a PostgreSQL wire-protocol server for the stall tests: it speaks
// just enough of the protocol for a database/sql pool over pgx to connect,
// ping and run a parameterless statement, and it can stop answering at any
// moment while keeping every TCP connection open and acknowledged -- which is
// what a paused or hung server does, and what no real server can be told to
// do from a unit test.
//
// It exists because the write-outcome classification (outcome.go) is only
// worth what the DRIVER actually returns in each shape, and those errors
// (pgconn's timeout wrapper, the stdlib's ErrBadConn conversion, the bare
// context error from database/sql's pool wait) cannot be constructed from
// outside pgx. A double that returns an error at once would test the
// classifier against the test author's guess of the driver, not the driver.

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/jackc/pgx/v5/pgproto3"
)

type pgfake struct {
	ln net.Listener
	// stalled: when set, connections are accepted and frontend messages are
	// read and dropped, but nothing is ever answered -- not even a startup.
	stalled atomic.Bool

	mu       sync.Mutex
	conns    []net.Conn
	startups []map[string]string
	received []string // one entry per frontend message, "Type[: sql]"
}

func newPgfake(t *testing.T) *pgfake {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &pgfake{ln: ln}
	go f.serve()
	t.Cleanup(f.Close)
	return f
}

// DSN is a plaintext connection URL to the fake, with a 1 s connect timeout
// and the same server-side second line the product DSN carries.
func (f *pgfake) DSN() string {
	host, port, _ := net.SplitHostPort(f.ln.Addr().String())
	dsn := PostgresDSN("tester", "pw", host, port, "fake", SSLModeDisable)
	return strings.Replace(dsn, "connect_timeout=2", "connect_timeout=1", 1)
}

func (f *pgfake) Close() {
	f.ln.Close()
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.conns {
		c.Close()
	}
}

func (f *pgfake) serve() {
	for {
		c, err := f.ln.Accept()
		if err != nil {
			return
		}
		f.mu.Lock()
		f.conns = append(f.conns, c)
		f.mu.Unlock()
		go f.handle(c)
	}
}

func (f *pgfake) record(s string) {
	f.mu.Lock()
	f.received = append(f.received, s)
	f.mu.Unlock()
}

// Received returns every frontend message seen so far, in order.
func (f *pgfake) Received() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.received...)
}

// Startups returns the parameter map of every startup packet received.
func (f *pgfake) Startups() []map[string]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]map[string]string(nil), f.startups...)
}

func (f *pgfake) handle(c net.Conn) {
	defer c.Close()
	be := pgproto3.NewBackend(c, c)
	for {
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.SSLRequest:
			// sslmode=disable never sends one; refuse it if it comes.
			if _, err := c.Write([]byte{'N'}); err != nil {
				return
			}
			continue
		case *pgproto3.StartupMessage:
			f.mu.Lock()
			f.startups = append(f.startups, m.Parameters)
			f.mu.Unlock()
			f.record("Startup")
		default:
			return
		}
		break
	}
	if f.stalled.Load() {
		f.drain(be)
		return
	}
	be.Send(&pgproto3.AuthenticationOk{})
	for _, p := range [][2]string{
		{"server_version", "16.0"}, {"server_encoding", "UTF8"}, {"client_encoding", "UTF8"},
		{"standard_conforming_strings", "on"}, {"integer_datetimes", "on"}, {"DateStyle", "ISO, MDY"},
	} {
		be.Send(&pgproto3.ParameterStatus{Name: p[0], Value: p[1]})
	}
	be.Send(&pgproto3.BackendKeyData{ProcessID: 4242, SecretKey: []byte("secret")})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if err := be.Flush(); err != nil {
		return
	}
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			f.record("Query: " + m.String)
		case *pgproto3.Parse:
			f.record("Parse: " + m.Query)
		default:
			f.record(fmt.Sprintf("%T", msg))
		}
		if f.stalled.Load() {
			// From here on: read and drop. The socket stays open and the
			// kernel keeps acknowledging, so the client sees neither an
			// error nor a reply, exactly as against a paused server.
			f.drain(be)
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			if strings.TrimSpace(m.String) == ";" || strings.TrimSpace(m.String) == "" {
				be.Send(&pgproto3.EmptyQueryResponse{})
			} else {
				be.Send(&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")})
			}
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		case *pgproto3.Parse:
			be.Send(&pgproto3.ParseComplete{})
		case *pgproto3.Describe:
			if m.ObjectType == 'S' {
				be.Send(&pgproto3.ParameterDescription{ParameterOIDs: []uint32{}})
			}
			be.Send(&pgproto3.NoData{})
		case *pgproto3.Bind:
			be.Send(&pgproto3.BindComplete{})
		case *pgproto3.Execute:
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte("INSERT 0 1")})
		case *pgproto3.Sync:
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
		case *pgproto3.Terminate:
			return
		}
		if _, isSync := msg.(*pgproto3.Sync); isSync {
			if err := be.Flush(); err != nil {
				return
			}
		}
		if _, isQuery := msg.(*pgproto3.Query); isQuery {
			if err := be.Flush(); err != nil {
				return
			}
		}
	}
}

// drain keeps reading so the client's writes never block, and never writes.
func (f *pgfake) drain(be *pgproto3.Backend) {
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			f.record("Query: " + m.String)
		case *pgproto3.Parse:
			f.record("Parse: " + m.Query)
		default:
			f.record(fmt.Sprintf("%T", msg))
		}
	}
}
