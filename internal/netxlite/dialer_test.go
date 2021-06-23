package netxlite

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/apex/log"
	"github.com/ooni/probe-cli/v3/internal/netxmocks"
)

func TestDialerResolverNoPort(t *testing.T) {
	dialer := &DialerResolver{Dialer: new(net.Dialer), Resolver: DefaultResolver}
	conn, err := dialer.DialContext(context.Background(), "tcp", "antani.ooni.nu")
	if err == nil {
		t.Fatal("expected an error here")
	}
	if conn != nil {
		t.Fatal("expected a nil conn here")
	}
}

func TestDialerResolverLookupHostAddress(t *testing.T) {
	dialer := &DialerResolver{Dialer: new(net.Dialer), Resolver: MockableResolver{
		Err: errors.New("mocked error"),
	}}
	addrs, err := dialer.lookupHost(context.Background(), "1.1.1.1")
	if err != nil {
		t.Fatal(err)
	}
	if len(addrs) != 1 || addrs[0] != "1.1.1.1" {
		t.Fatal("not the result we expected")
	}
}

func TestDialerResolverLookupHostFailure(t *testing.T) {
	expected := errors.New("mocked error")
	dialer := &DialerResolver{Dialer: new(net.Dialer), Resolver: MockableResolver{
		Err: expected,
	}}
	conn, err := dialer.DialContext(context.Background(), "tcp", "dns.google.com:853")
	if !errors.Is(err, expected) {
		t.Fatal("not the error we expected")
	}
	if conn != nil {
		t.Fatal("expected nil conn")
	}
}

type MockableResolver struct {
	Addresses []string
	Err       error
}

func (r MockableResolver) LookupHost(ctx context.Context, host string) ([]string, error) {
	return r.Addresses, r.Err
}

func (r MockableResolver) Network() string {
	return "mockable"
}

func (r MockableResolver) Address() string {
	return ""
}

func TestDialerResolverDialForSingleIPFails(t *testing.T) {
	dialer := &DialerResolver{Dialer: &netxmocks.Dialer{
		MockDialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return nil, io.EOF
		},
	}, Resolver: DefaultResolver}
	conn, err := dialer.DialContext(context.Background(), "tcp", "1.1.1.1:853")
	if !errors.Is(err, io.EOF) {
		t.Fatal("not the error we expected")
	}
	if conn != nil {
		t.Fatal("expected nil conn")
	}
}

func TestDialerResolverDialForManyIPFails(t *testing.T) {
	dialer := &DialerResolver{
		Dialer: &netxmocks.Dialer{
			MockDialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				return nil, io.EOF
			},
		}, Resolver: MockableResolver{
			Addresses: []string{"1.1.1.1", "8.8.8.8"},
		}}
	conn, err := dialer.DialContext(context.Background(), "tcp", "dot.dns:853")
	if !errors.Is(err, io.EOF) {
		t.Fatal("not the error we expected")
	}
	if conn != nil {
		t.Fatal("expected nil conn")
	}
}

func TestDialerResolverDialForManyIPSuccess(t *testing.T) {
	dialer := &DialerResolver{Dialer: &netxmocks.Dialer{
		MockDialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
			return &netxmocks.Conn{
				MockClose: func() error {
					return nil
				},
			}, nil
		},
	}, Resolver: MockableResolver{
		Addresses: []string{"1.1.1.1", "8.8.8.8"},
	}}
	conn, err := dialer.DialContext(context.Background(), "tcp", "dot.dns:853")
	if err != nil {
		t.Fatal("expected nil error here")
	}
	if conn == nil {
		t.Fatal("expected non-nil conn")
	}
	conn.Close()
}

func TestDialerLoggerFailure(t *testing.T) {
	d := &DialerLogger{
		Dialer: &netxmocks.Dialer{
			MockDialContext: func(ctx context.Context, network string, address string) (net.Conn, error) {
				return nil, io.EOF
			},
		},
		Logger: log.Log,
	}
	conn, err := d.DialContext(context.Background(), "tcp", "www.google.com:443")
	if !errors.Is(err, io.EOF) {
		t.Fatal("not the error we expected")
	}
	if conn != nil {
		t.Fatal("expected nil conn here")
	}
}

func TestDefaultDialerWorks(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // fail immediately
	conn, err := DefaultDialer.DialContext(ctx, "tcp", "8.8.8.8:853")
	if err == nil || !strings.HasSuffix(err.Error(), "operation was canceled") {
		t.Fatal("not the error we expected", err)
	}
	if conn != nil {
		t.Fatal("expected nil conn here")
	}
}

func TestDefaultDialerHasTimeout(t *testing.T) {
	expected := 15 * time.Second
	if DefaultDialer.Timeout != expected {
		t.Fatal("unexpected timeout value")
	}
}