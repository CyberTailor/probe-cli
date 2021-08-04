package internal

import (
	"context"
	"net"

	"github.com/ooni/probe-cli/v3/internal/engine/experiment/nwebconnectivity"
	"github.com/ooni/probe-cli/v3/internal/engine/netx"
	"github.com/ooni/probe-cli/v3/internal/engine/netx/archival"
)

// newfailure is a convenience shortcut to save typing
var newfailure = archival.NewFailure

// CtrlDNSResult is the result of the DNS check performed by
// the Web Connectivity test helper.
type CtrlDNSResult = nwebconnectivity.ControlDNS

// DNSConfig configures the DNS check.
type DNSConfig struct {
	Domain   string
	Resolver netx.Resolver
}

// DNSDo performs the DNS check.
func DNSDo(ctx context.Context, config *DNSConfig) CtrlDNSResult {
	if net.ParseIP(config.Domain) != nil {
		// handle IP address format input
		return CtrlDNSResult{Failure: nil, Addrs: []string{config.Domain}}
	}
	addrs, err := config.Resolver.LookupHost(ctx, config.Domain)
	return CtrlDNSResult{Failure: newfailure(err), Addrs: addrs}
}
