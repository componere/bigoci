package oci

import (
	"context"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/netip"
	"net/url"
	"reflect"
	"strings"
	"sync"
)

// standardDialContextPC snapshots the standard library's direct dial hook
// before caller code can replace the mutable [http.DefaultTransport].
//
//nolint:gochecknoglobals // Immutable security snapshot.
var standardDialContextPC = snapshotStandardDialContextPC()

// snapshotStandardDialContextPC records the standard library's direct dial
// hook without assuming the mutable default still has its documented type.
func snapshotStandardDialContextPC() uintptr {
	transport, ok := http.DefaultTransport.(*http.Transport)
	if !ok || transport.DialContext == nil {
		return 0
	}

	return reflect.ValueOf(transport.DialContext).Pointer()
}

// externalTargetError reports that the registry selected an endpoint whose
// destination bigoci could not safely verify or was not permitted to reach.
type externalTargetError struct {
	// reason describes the refused boundary without rendering the endpoint URL.
	reason string
}

// Error renders the endpoint-policy refusal.
func (e *externalTargetError) Error() string {
	return "refuse the registry-selected external endpoint: " + e.reason
}

// proxyDecisionKey is the request-context key that pins one proxy decision to
// the external transport clone which consumes it.
type proxyDecisionKey struct{}

// proxyDecision is the proxy URL the caller's policy selected for one request.
type proxyDecision struct {
	// endpoint is nil for a direct request.
	endpoint *url.URL
}

// externalTransport applies the registry-selected endpoint policy before and
// during one trip through the caller-derived transport.
type externalTransport struct {
	// registry is the caller's original transport used for the registry's own
	// hostname, preserving its connection pool and every custom behavior.
	registry http.RoundTripper
	// next is the inspectable transport clone used by the secure default. When
	// the caller's transport is opaque, the secure default fails before using it.
	next http.RoundTripper
	// registryHost is the host the caller selected in the artifact reference.
	registryHost string
	// proxy is the caller's proxy policy, evaluated once and pinned to the trip.
	proxy func(*http.Request) (*url.URL, error)
	// inspectable reports that next bottoms out in a cloned [http.Transport].
	inspectable bool
	// unverifiedDial reports that a caller dial hook can return a tunnel whose
	// remote address identifies a proxy rather than the selected endpoint.
	unverifiedDial bool
	// allowUnverified permits a custom dial hook, opaque transport, or proxy to
	// route a cross-registry request where bigoci cannot observe the destination.
	allowUnverified bool
}

// externalTransportLayer is an observer-style transport wrapper that can be
// rebuilt around a guarded base transport without losing its observation.
//
// The reference CLI's debug tap implements this structural seam. Other opaque
// transports remain opaque and require the caller's explicit authorization.
type externalTransportLayer interface {
	// BigociExternalBase returns the transport this layer forwards to.
	BigociExternalBase() http.RoundTripper
	// BigociWrapExternal returns the same layer forwarding to next.
	BigociWrapExternal(next http.RoundTripper) http.RoundTripper
}

// ExternalTransportBase is the caller-derived transport and proxy policy
// shared by every repository one public bigoci client builds. The caller's
// original transport keeps same-registry behavior and pooling; its concrete
// clone owns one reusable guarded cross-host connection pool. An explicitly
// unverified repository uses the original transport for cross-host requests as
// well, preserving caller-owned behavior that Clone cannot carry.
//
// This type is exported only across bigoci's internal package boundary. The
// root package prepares it lazily so the public Client's zero value remains
// usable.
type ExternalTransportBase struct {
	// registry is the caller's original transport for same-registry endpoints.
	registry http.RoundTripper
	// next is the inspectable clone used by the secure default, or the opaque
	// original retained only so its lack of inspectability can be classified.
	next http.RoundTripper
	// proxy is the concrete transport's caller-supplied proxy policy.
	proxy func(*http.Request) (*url.URL, error)
	// inspectable reports that next bottoms out in net/http's trace-aware
	// transport implementation.
	inspectable bool
	// unverifiedDial reports that the concrete transport uses a caller dial
	// hook whose returned connection may represent a tunnel.
	unverifiedDial bool
}

// connectionCheck records a direct connection that landed on a destination a
// registry-selected cross-host request may not use.
type connectionCheck struct {
	// mu guards err because transport hooks may run on transport goroutines.
	mu sync.Mutex
	// err is the first connection-policy failure.
	err *externalTargetError
}

// NewExternalTransportBase derives one reusable guarded-transport base from
// next. A nil next selects [http.DefaultTransport].
func NewExternalTransportBase(next http.RoundTripper) *ExternalTransportBase {
	if next == nil {
		next = http.DefaultTransport
	}

	guarded, proxy, unverifiedDial, inspectable := inspectableExternalTransport(next)
	if !inspectable {
		guarded = next
	}

	return &ExternalTransportBase{
		registry:       next,
		next:           guarded,
		proxy:          proxy,
		inspectable:    inspectable,
		unverifiedDial: unverifiedDial,
	}
}

// forRegistry binds the shared base to the host and authorization policy of
// one repository.
func (b *ExternalTransportBase) forRegistry(registryHost string, allowUnverified bool) http.RoundTripper {
	return &externalTransport{
		registry:        b.registry,
		next:            b.next,
		registryHost:    registryHost,
		proxy:           b.proxy,
		inspectable:     b.inspectable,
		unverifiedDial:  b.unverifiedDial,
		allowUnverified: allowUnverified,
	}
}

// newExternalTransport derives a standalone external transport for an
// internal Repository whose caller did not supply a shared base.
func newExternalTransport(next http.RoundTripper, registryHost string, allowUnverified bool) http.RoundTripper {
	return NewExternalTransportBase(next).forRegistry(registryHost, allowUnverified)
}

// inspectableExternalTransport clones a concrete [http.Transport], rebuilding
// observer layers around it, and returns the original proxy policy separately
// so one decision can be pinned to each request.
func inspectableExternalTransport(
	next http.RoundTripper,
) (http.RoundTripper, func(*http.Request) (*url.URL, error), bool, bool) {
	switch transport := next.(type) {
	case *http.Transport:
		clone := transport.Clone()
		// A caller-supplied TLSNextProto handler becomes the RoundTripper after
		// TLS negotiation and is not required to honor httptrace.GotConn. An
		// internally configured HTTP/2 transport is different: Clone leaves
		// TLSNextProto nil when the caller did, and net/http's implementation
		// invokes GotConn before opening the stream.
		if len(clone.TLSNextProto) > 0 {
			return nil, nil, false, false
		}

		proxy := clone.Proxy
		if proxy != nil {
			clone.Proxy = pinnedProxy
		}

		return clone, proxy, unverifiedDialHooks(transport), true
	case externalTransportLayer:
		base, proxy, unverifiedDial, ok := inspectableExternalTransport(transport.BigociExternalBase())
		if !ok {
			return nil, nil, false, false
		}

		return transport.BigociWrapExternal(base), proxy, unverifiedDial, true
	default:
		return nil, nil, false, false
	}
}

// unverifiedDialHooks reports whether transport's effective dial path uses a
// caller hook other than the standard [net.Dialer] hook used by
// [http.DefaultTransport]. Such a hook may establish a tunnel whose
// [net.Conn.RemoteAddr] identifies a proxy rather than the endpoint.
func unverifiedDialHooks(transport *http.Transport) bool {
	if transport.DialTLSContext != nil || transport.DialTLS != nil { //nolint:staticcheck // Legacy hook is effective.
		return true
	}

	if transport.DialContext != nil {
		return reflect.ValueOf(transport.DialContext).Pointer() != standardDialContextPC
	}

	return transport.Dial != nil //nolint:staticcheck // Legacy hook is effective.
}

// pinnedProxy returns the proxy decision [externalTransport.RoundTrip] stored
// on the request before the cloned [http.Transport] began its trip.
func pinnedProxy(req *http.Request) (*url.URL, error) {
	decision, ok := req.Context().Value(proxyDecisionKey{}).(proxyDecision)
	if !ok {
		return nil, &externalTargetError{reason: "the caller's proxy decision was not bound to the request"}
	}

	return decision.endpoint, nil
}

// RoundTrip sends one registry-selected request. The secure default validates a
// direct connection's actual peer before HTTP request bytes leave; an explicit
// unverified option delegates the whole cross-host boundary to the caller.
func (t *externalTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	sameRegistry := sameEndpointHost(req.URL.Hostname(), t.registryHost)
	if sameRegistry {
		return t.registry.RoundTrip(req)
	}
	if t.allowUnverified {
		// The caller explicitly owns this boundary. Use the original transport,
		// not the inspectable clone: http.Transport.Clone intentionally omits
		// handlers installed through RegisterProtocol, and an escape hatch that
		// silently discards caller routing is not an escape hatch at all.
		return t.registry.RoundTrip(req)
	}

	if !t.inspectable {
		return nil, &externalTargetError{
			reason: "the caller's opaque transport cannot prove where a cross-registry request connects",
		}
	}
	if t.unverifiedDial {
		return nil, &externalTargetError{
			reason: "a caller dial hook can hide the destination of a cross-registry request",
		}
	}

	var proxyEndpoint *url.URL
	if t.proxy != nil {
		var err error
		proxyEndpoint, err = t.proxy(req)
		if err != nil {
			return nil, err
		}
		if proxyEndpoint != nil {
			return nil, &externalTargetError{
				reason: "a proxy hides the destination of a cross-registry request",
			}
		}
	}

	ctx := req.Context()
	if t.proxy != nil {
		ctx = context.WithValue(ctx, proxyDecisionKey{}, proxyDecision{endpoint: proxyEndpoint})
	}

	var check *connectionCheck
	if proxyEndpoint == nil {
		check = &connectionCheck{}
		ctx = httptrace.WithClientTrace(ctx, &httptrace.ClientTrace{GotConn: check.gotConn})
	}

	resp, err := t.next.RoundTrip(req.Clone(ctx))
	if check == nil {
		return resp, err
	}

	if policyErr := check.failure(); policyErr != nil {
		if resp != nil && resp.Body != nil {
			_ = resp.Body.Close()
		}

		return nil, policyErr
	}

	return resp, err
}

// gotConn validates the actual peer of a direct connection and closes it
// synchronously before the transport can write an HTTP request when refused.
func (c *connectionCheck) gotConn(info httptrace.GotConnInfo) {
	addr, ok := remoteIP(info.Conn.RemoteAddr())
	if !ok {
		c.record(&externalTargetError{reason: "the direct connection's remote address is not an IP address"})
		_ = info.Conn.Close()

		return
	}

	if restrictedIP(addr) {
		c.record(&externalTargetError{
			reason: "the direct connection landed on a local or private IP address",
		})
		_ = info.Conn.Close()
	}
}

// record keeps the first connection-policy failure.
func (c *connectionCheck) record(err *externalTargetError) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.err == nil {
		c.err = err
	}
}

// failure returns the connection-policy failure, if the transport observed one.
func (c *connectionCheck) failure() *externalTargetError {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.err
}

// remoteIP reads an IP address from a connection's remote endpoint.
func remoteIP(remote net.Addr) (netip.Addr, bool) {
	if tcp, ok := remote.(*net.TCPAddr); ok {
		addr, valid := netip.AddrFromSlice(tcp.IP)
		if !valid {
			return netip.Addr{}, false
		}

		return addr.Unmap(), true
	}

	host, _, err := net.SplitHostPort(remote.String())
	if err != nil {
		return netip.Addr{}, false
	}
	addr, err := netip.ParseAddr(host)
	if err != nil {
		return netip.Addr{}, false
	}

	return addr.Unmap(), true
}

// restrictedIPTarget reports whether hostname is a local or private IP
// literal that is not also the literal address of the registry itself. DNS
// names continue to the connection guard, which checks the peer actually used.
func restrictedIPTarget(hostname, registryHost string) bool {
	_, restricted := restrictedIPLiteral(hostname)

	return restricted && !sameEndpointHost(hostname, registryHost)
}

// sameEndpointHost reports whether hostname names the registry's host,
// comparing DNS names without case and IP literals by address value.
func sameEndpointHost(hostname, registryHost string) bool {
	registryName := (&url.URL{Host: registryHost}).Hostname()
	if strings.EqualFold(strings.TrimSuffix(hostname, "."), strings.TrimSuffix(registryName, ".")) {
		return true
	}

	target, targetErr := netip.ParseAddr(hostname)
	registry, registryErr := netip.ParseAddr(registryName)

	return targetErr == nil && registryErr == nil && target.Unmap() == registry.Unmap()
}

// restrictedIPLiteral parses hostname as an IP literal and reports the
// address classes a registry may not select on another host: loopback,
// private, link-local unicast or multicast, and unspecified. IPv4-mapped IPv6
// addresses are unmapped before classification so their spelling cannot
// bypass the IPv4 rule.
func restrictedIPLiteral(hostname string) (netip.Addr, bool) {
	addr, err := netip.ParseAddr(hostname)
	if err != nil {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()

	return addr, restrictedIP(addr)
}

// restrictedIP reports whether addr belongs to an address class a
// registry-selected cross-host endpoint may not use.
func restrictedIP(addr netip.Addr) bool {
	return addr.IsLoopback() || addr.IsPrivate() || addr.IsLinkLocalUnicast() ||
		addr.IsLinkLocalMulticast() || addr.IsUnspecified()
}
