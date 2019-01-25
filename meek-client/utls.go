// Support code for TLS camouflage using uTLS.
//
// The goal is: provide an http.RoundTripper abstraction that retains the
// features of http.Transport (e.g., persistent connections and HTTP/2 support),
// while making TLS connections using uTLS in place of crypto/tls. The challenge
// is: while http.Transport provides a DialTLS hook, setting it to non-nil
// disables automatic HTTP/2 support in the client. Most of the uTLS
// fingerprints contain an ALPN extension containing "h2"; i.e., they declare
// support for HTTP/2. If the server also supports HTTP/2, then uTLS may
// negotiate an HTTP/2 connection without the http.Transport knowing it, which
// leads to an HTTP/1.1 client speaking to an HTTP/2 server, a protocol error.
//
// The code here uses an idea adapted from meek_lite in obfs4proxy:
// https://gitlab.com/yawning/obfs4/commit/4d453dab2120082b00bf6e63ab4aaeeda6b8d8a3
// Instead of setting DialTLS on an http.Transport and exposing it directly, we
// expose a wrapper type, UTLSRoundTripper, that contains within it either an
// http.Transport or an http2.Transport. The first time a caller calls RoundTrip
// on the wrapper, we initiate a uTLS connection (bootstrapConn), then peek at
// the ALPN-negotiated protocol: if "h2", create an internal http2.Transport;
// otherwise, create an internal http.Transport. In either case, set DialTLS on
// the created Transport do a function that dials using uTLS As a special case,
// the first time the DialTLS callback is called, it reuses bootstrapConn (the
// one made to peek at the ALPN), rather than make a new connection.
//
// Subsequent calls to RoundTripper on the wrapper just pass the requests though
// the previously created http.Transport or http2.Transport. We assume that in
// future RoundTrips, the ALPN-negotiated protocol will remain the same as it
// was in the initial RoundTrip. At this point it is the http.Transport or
// http2.Transport calling DialTLS, not us, so we can't dynamically swap the
// underlying transport based on the ALPN.
//
// https://bugs.torproject.org/29077
// https://github.com/refraction-networking/utls/issues/16
package main

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strings"
	"sync"

	utls "github.com/refraction-networking/utls"
	"golang.org/x/net/http2"
)

// Extract a host:port address from a URL, suitable for passing to net.Dial.
func addrForDial(url *url.URL) (string, error) {
	host := url.Hostname()
	// net/http would use golang.org/x/net/idna here, to convert a possible
	// internationalized domain name to ASCII.
	port := url.Port()
	if port == "" {
		// No port? Use the default for the scheme. We only care about https.
		if url.Scheme == "https" {
			port = "https"
		} else {
			return "", fmt.Errorf("unsupported URL scheme %q", url.Scheme)
		}
	}
	return net.JoinHostPort(host, port), nil
}

// Analogous to tls.Dial. Connect to the given address and initiate a TLS
// handshake using the given ClientHelloID, returning the resulting connection.
func dialUTLS(network, addr string, cfg *utls.Config, clientHelloID *utls.ClientHelloID) (*utls.UConn, error) {
	if options.ProxyURL != nil {
		return nil, fmt.Errorf("no proxy allowed with uTLS")
	}

	conn, err := net.Dial(network, addr)
	if err != nil {
		return nil, err
	}
	uconn := utls.UClient(conn, cfg, *clientHelloID)
	err = uconn.Handshake()
	if err != nil {
		return nil, err
	}
	return uconn, nil
}

// A http.RoundTripper that uses uTLS (with a specified Client Hello ID) to make
// TLS connections.
//
// Can only be reused among servers which negotiate the same ALPN.
type UTLSRoundTripper struct {
	sync.Mutex

	clientHelloID *utls.ClientHelloID
	rt            http.RoundTripper
}

func (rt *UTLSRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	switch req.URL.Scheme {
	case "http":
		// If http, we don't invoke uTLS; just pass it to the global http.Transport.
		return httpRoundTripper.RoundTrip(req)
	case "https":
	default:
		return nil, fmt.Errorf("unsupported URL scheme %q", req.URL.Scheme)
	}

	rt.Lock()
	defer rt.Unlock()

	if rt.rt == nil {
		// On the first call, make an http.Transport or http2.Transport
		// as appropriate.
		var err error
		rt.rt, err = makeRoundTripper(req, rt.clientHelloID)
		if err != nil {
			return nil, err
		}
	}
	// Forward the request to the internal http.Transport or http2.Transport.
	return rt.rt.RoundTrip(req)
}

func makeRoundTripper(req *http.Request, clientHelloID *utls.ClientHelloID) (http.RoundTripper, error) {
	addr, err := addrForDial(req.URL)
	if err != nil {
		return nil, err
	}
	cfg := &utls.Config{ServerName: req.URL.Hostname()}
	bootstrapConn, err := dialUTLS("tcp", addr, cfg, clientHelloID)
	if err != nil {
		return nil, err
	}

	// Peek at what protocol we negotiated.
	protocol := bootstrapConn.ConnectionState().NegotiatedProtocol

	// Protects bootstrapConn.
	var lock sync.Mutex
	// This is the callback for future dials done by the internal
	// http.Transport or http2.Transport.
	dialTLS := func(network, addr string) (net.Conn, error) {
		lock.Lock()
		defer lock.Unlock()

		// On the first dial, reuse bootstrapConn.
		if bootstrapConn != nil {
			uconn := bootstrapConn
			bootstrapConn = nil
			return uconn, nil
		}

		// Later dials make a new connection.
		uconn, err := dialUTLS(network, addr, cfg, clientHelloID)
		if err != nil {
			return nil, err
		}
		if uconn.ConnectionState().NegotiatedProtocol != protocol {
			return nil, fmt.Errorf("unexpected switch from ALPN %q to %q",
				protocol, uconn.ConnectionState().NegotiatedProtocol)
		}

		return uconn, nil
	}

	// Construct an http.Transport or http2.Transport depending on ALPN.
	switch protocol {
	case http2.NextProtoTLS:
		return &http2.Transport{
			DialTLS: func(network, addr string, _ *tls.Config) (net.Conn, error) {
				// Ignore the *tls.Config parameter; use our
				// static cfg instead.
				return dialTLS(network, addr)
			},
		}, nil
	default:
		// TODO: copy public fields from httpRoundTripper?
		return &http.Transport{DialTLS: dialTLS}, nil
	}
}

var clientHelloIDMap = map[string]*utls.ClientHelloID{
	// No HelloCustom: not useful for external configuration.
	// No HelloGolang: just don't use uTLS.
	// No HelloRandomized: doesn't negotiate consistent ALPN.
	"hellorandomizedalpn":   &utls.HelloRandomizedALPN,
	"hellorandomizednoalpn": &utls.HelloRandomizedNoALPN,
	"hellofirefox_auto":     &utls.HelloFirefox_Auto,
	"hellofirefox_55":       &utls.HelloFirefox_55,
	"hellofirefox_56":       &utls.HelloFirefox_56,
	"hellofirefox_63":       &utls.HelloFirefox_63,
	"hellochrome_auto":      &utls.HelloChrome_Auto,
	"hellochrome_58":        &utls.HelloChrome_58,
	"hellochrome_62":        &utls.HelloChrome_62,
	"hellochrome_70":        &utls.HelloChrome_70,
	"helloios_auto":         &utls.HelloIOS_Auto,
	"helloios_11_1":         &utls.HelloIOS_11_1,
}

func NewUTLSRoundTripper(name string) (*UTLSRoundTripper, error) {
	// Lookup is case-insensitive.
	clientHelloID, ok := clientHelloIDMap[strings.ToLower(name)]
	if !ok {
		return nil, fmt.Errorf("no uTLS Client Hello ID named %q", name)
	}
	return &UTLSRoundTripper{
		clientHelloID: clientHelloID,
	}, nil
}