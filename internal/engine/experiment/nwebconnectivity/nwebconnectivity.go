package nwebconnectivity

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/apex/log"
	"github.com/lucas-clemente/quic-go"
	"github.com/lucas-clemente/quic-go/http3"
	"github.com/ooni/probe-cli/v3/internal/engine/geolocate"
	"github.com/ooni/probe-cli/v3/internal/engine/httpheader"
	"github.com/ooni/probe-cli/v3/internal/engine/model"
	"github.com/ooni/probe-cli/v3/internal/engine/netx/archival"
	"github.com/ooni/probe-cli/v3/internal/engine/netx/trace"
	"github.com/ooni/probe-cli/v3/internal/errorsx"
	"github.com/ooni/probe-cli/v3/internal/netxlite"
	"github.com/ooni/probe-cli/v3/internal/runtimex"
	"github.com/ooni/psiphon/oopsi/golang.org/x/net/idna"
	utls "gitlab.com/yawning/utls.git"
	"golang.org/x/net/http2"
)

// Conig contains the experiment config.
type Config struct {
	ClientHello string `ooni:"Use ClientHello of specific client for parroting."`
}

// Measurer performs the measurement.
type Measurer struct {
	Config     Config
	dialer     netxlite.Dialer
	handshaker netxlite.TLSHandshaker
	logger     netxlite.Logger
	quicDialer netxlite.QUICContextDialer
}

// TestKeys contains webconnectivity test keys.
type TestKeys struct {
	sync.Mutex
	Agent          string `json:"agent"`
	ClientResolver string `json:"client_resolver"`

	// For now mostly TCP/TLS "connect" experiment but we are
	// considering adding more events. An open question is
	// currently how to properly tag these events so that it
	// is rather obvious where they come from.
	//
	// See https://github.com/ooni/probe/issues/1413.
	NetworkEvents []archival.NetworkEvent `json:"network_events"`
	TLSHandshakes []archival.TLSHandshake `json:"tls_handshakes"`

	// DNS experiment
	Queries              []archival.DNSQueryEntry `json:"queries"`
	DNSExperimentFailure *string                  `json:"dns_experiment_failure"`

	// Control experiment
	ControlFailure *string `json:"control_failure"`

	// TCP/TLS "connect" experiment
	TCPConnect          []archival.TCPConnectEntry `json:"tcp_connect"`
	TCPConnectSuccesses int                        `json:"-"`
	TCPConnectAttempts  int                        `json:"-"`

	// HTTP experiment
	Requests              []archival.RequestEntry `json:"requests"`
	HTTPExperimentFailure *string                 `json:"http_experiment_failure"`
}

// Tags describing the section of this experiment in which
// the data has been collected.
const (
	// TCPTLSExperimentTag is a tag indicating the TCP connect experiment.
	TCPTLSExperimentTag = "tcptls_experiment"

	// QUICTLSExperimentTag is a tag indicating the QUIC handshake experiment.
	QUICTLSExperimentTag = "quictls_experiment"
)

// NewExperimentMeasurer creates a new ExperimentMeasurer.
func NewExperimentMeasurer(config Config) model.ExperimentMeasurer {
	logger := log.Log
	return &Measurer{
		Config:     config,
		dialer:     newDialer(logger),
		handshaker: newHandshaker(config),
		logger:     logger,
		quicDialer: newQUICDialer(logger),
	}
}

func getClientHelloID(stringHelloID string) (utlsID *utls.ClientHelloID) {
	switch strings.ToLower(stringHelloID) {
	case "firefox":
		return &utls.HelloFirefox_Auto
	case "chrome":
		return &utls.HelloChrome_Auto
	case "ios":
		return &utls.HelloIOS_Auto
	case "golang":
		return &utls.HelloGolang
	}
	return nil
}

func newHandshaker(config Config) netxlite.TLSHandshaker {
	var h netxlite.TLSHandshaker
	h = &netxlite.TLSHandshakerConfigurable{}
	typedClientHello := getClientHelloID(config.ClientHello)
	if typedClientHello != nil {
		h.(*netxlite.TLSHandshakerConfigurable).NewConn = netxlite.NewConnUTLS(typedClientHello)
	}
	h = &errorsx.ErrorWrapperTLSHandshaker{TLSHandshaker: h}
	return h
}

func newDialer(logger netxlite.Logger) netxlite.Dialer {
	var d netxlite.Dialer
	d = &errorsx.ErrorWrapperDialer{Dialer: netxlite.DefaultDialer}
	d = &netxlite.DialerLogger{Dialer: d, Logger: logger}
	return d
}

func newQUICDialer(logger netxlite.Logger) netxlite.QUICContextDialer {
	ql := &errorsx.ErrorWrapperQUICListener{QUICListener: &netxlite.QUICListenerStdlib{}}
	var d netxlite.QUICContextDialer = &netxlite.QUICDialerQUICGo{QUICListener: ql}
	d = &errorsx.ErrorWrapperQUICDialer{Dialer: d}
	return d
}

// ExperimentName implements ExperimentMeasurer.ExperExperimentName.
func (m *Measurer) ExperimentName() string {
	return "new_webconnectivity"
}

// ExperimentVersion implements ExperimentMeasurer.ExperExperimentVersion.
func (m *Measurer) ExperimentVersion() string {
	return "0.1.0"
}

var (
	// ErrNoAvailableTestHelpers is emitted when there are no available test helpers.
	ErrNoAvailableTestHelpers = errors.New("no available helpers")

	// ErrNoInput indicates that no input was provided
	ErrNoInput = errors.New("no input provided")

	// ErrInputIsNotAnURL indicates that the input is not an URL.
	ErrInputIsNotAnURL = errors.New("input is not an URL")

	// ErrUnsupportedInput indicates that the input URL scheme is unsupported.
	ErrUnsupportedInput = errors.New("unsupported input scheme")
)

type MeasurementSession struct {
	experimentSession model.ExperimentSession
	jar               *cookiejar.Jar
	measurement       *model.Measurement
	redirectedReq     *http.Request
	URL               *url.URL
}

type redirectInfo struct {
	location *url.URL
	req      *http.Request
}

// Run implements ExperimentMeasurer.Run.
func (m *Measurer) Run(
	ctx context.Context,
	sess model.ExperimentSession,
	measurement *model.Measurement,
	callbacks model.ExperimentCallbacks,
) error {
	tk := new(TestKeys)
	measurement.TestKeys = tk
	// TODO(kelmenhorst): what is the specification of the TestKeys Agent? do we need to use "agent" hier?
	tk.Agent = "redirect"
	tk.ClientResolver = sess.ResolverIP()
	URL, err := url.Parse(string(measurement.Input))
	if err != nil {
		return ErrInputIsNotAnURL
	}
	// create session
	jar, err := cookiejar.New(nil)
	runtimex.PanicOnError(err, "cookiejar.New failed")
	session := &MeasurementSession{
		experimentSession: sess,
		jar:               jar,
		measurement:       measurement,
		URL:               URL,
	}
	return m.runWithRedirect(session, ctx, 0)
}

func (m *Measurer) runWithRedirect(
	sess *MeasurementSession,
	ctx context.Context,
	nRedirects int,
) error {
	ctx, cancel := context.WithTimeout(ctx, 60*time.Second)
	defer cancel()

	if sess.URL.Scheme != "http" && sess.URL.Scheme != "https" {
		return ErrUnsupportedInput
	}

	// perform DNS lookup
	addresses := m.dnsLookup(sess, ctx)
	if len(addresses) == 0 {
		return nil
	}
	epnts := m.getEndpoints(addresses, sess.URL.Scheme)
	// TODO: perform dns lookup on testhelper and create union of the returned ip addresses

	var wg sync.WaitGroup
	// at most we should get a redirect response from each endpoints
	redirects := make(chan *redirectInfo, len(epnts)+1)

	// for each IP address
	for _, ip := range epnts {
		wg.Add(1)
		go m.measure(sess, ctx, ip, &wg, redirects)
		// TODO: perform the control measurement
	}
	wg.Wait()
	redirects <- nil

	rdrct := <-redirects
	// we only follow one redirect request here, assuming that we get the same redirect location from every endpoint that belongs to the domain
	// we assume this so that the number of requests does not exponentially grow with every redirect
	if rdrct != nil {
		if nRedirects == 10 {
			// we stop after 10 redirects
			return errors.New("stopped after 10 redirects")
		}
		session := &MeasurementSession{
			experimentSession: sess.experimentSession,
			jar:               sess.jar,
			measurement:       sess.measurement,
			redirectedReq:     rdrct.req,
			URL:               rdrct.location,
		}
		return m.runWithRedirect(session, ctx, nRedirects+1)
	}
	return nil

}

// dnsLookup finds the IP address(es) associated with a domain name
func (m *Measurer) dnsLookup(sess *MeasurementSession, ctx context.Context) []string {
	tk := sess.measurement.TestKeys.(*TestKeys)
	resolver := &errorsx.ErrorWrapperResolver{Resolver: &netxlite.ResolverSystem{}}
	hostname := sess.URL.Hostname()
	idnaHost, err := idna.ToASCII(hostname)
	if err != nil {
		tk.DNSExperimentFailure = archival.NewFailure(err)
		return nil
	}
	addrs, err := resolver.LookupHost(ctx, idnaHost)
	stop := time.Now()
	for _, qtype := range []dnsQueryType{"A", "AAAA"} {
		entry := archival.DNSQueryEntry{
			Engine:          resolver.Network(),
			Failure:         archival.NewFailure(err),
			Hostname:        hostname,
			QueryType:       string(qtype),
			ResolverAddress: resolver.Address(),
			T:               stop.Sub(sess.measurement.MeasurementStartTimeSaved).Seconds(),
		}
		for _, addr := range addrs {
			if qtype.ipoftype(addr) {
				entry.Answers = append(entry.Answers, qtype.makeanswerentry(addr))
			}
		}
		if len(entry.Answers) <= 0 && err == nil {
			continue
		}
		tk.Lock()
		tk.Queries = append(tk.Queries, entry)
		tk.Unlock()
	}
	tk.DNSExperimentFailure = archival.NewFailure(err)
	return addrs
}

func (m *Measurer) measure(
	sess *MeasurementSession,
	ctx context.Context,
	addr string,
	wg *sync.WaitGroup,
	redirects chan *redirectInfo,
) error {
	defer wg.Done()
	// connect
	conn := m.connect(sess.measurement, ctx, addr)
	if conn == nil {
		return nil
	}
	var transport http.RoundTripper
	switch sess.URL.Scheme {
	case "http":
		transport = netxlite.NewHTTPTransport(&singleDialerHTTP1{conn: &conn}, nil, nil)
	case "https":
		// Handshake
		transport = m.tlsHandshake(sess, ctx, conn)
	}
	if transport == nil {
		return nil
	}

	// HTTP roundtrip
	m.httpRoundtrip(sess, ctx, transport, redirects)

	// QUIC handshake
	transport = m.quicHandshake(sess, ctx, addr)
	if transport == nil {
		return nil
	}
	// HTTP/3 roundtrip
	m.httpRoundtrip(sess, ctx, transport, redirects)

	return nil
}

// connect performs the TCP three way handshake
func (m *Measurer) connect(measurement *model.Measurement, ctx context.Context, addr string) net.Conn {
	conn, err := m.dialer.DialContext(ctx, "tcp", addr)
	stop := time.Now()

	a, sport, _ := net.SplitHostPort(addr)
	iport, _ := strconv.Atoi(sport)
	entry := archival.TCPConnectEntry{
		IP:   a,
		Port: iport,
		Status: archival.TCPConnectStatus{
			Failure: archival.NewFailure(err),
			Success: err == nil,
		},
		T: stop.Sub(measurement.MeasurementStartTimeSaved).Seconds(),
	}
	tk := measurement.TestKeys.(*TestKeys)
	tk.Lock()
	tk.TCPConnect = append(tk.TCPConnect, entry)
	tk.Unlock()
	return conn
}

// quicHandshake performs the QUIC handshake
func (m *Measurer) quicHandshake(sess *MeasurementSession, ctx context.Context, addr string) http.RoundTripper {
	tlscfg := &tls.Config{
		ServerName: sess.URL.Hostname(),
		NextProtos: []string{"h3"},
	}
	qcfg := &quic.Config{}
	qsess, err := m.quicDialer.DialContext(ctx, "udp", addr, tlscfg, qcfg)
	stop := time.Now()
	if err != nil {
		entry := archival.TLSHandshake{
			Failure:     archival.NewFailure(err),
			NoTLSVerify: tlscfg.InsecureSkipVerify,
			ServerName:  tlscfg.ServerName,
			T:           stop.Sub(sess.measurement.MeasurementStartTimeSaved).Seconds(),
			Tags:        []string{QUICTLSExperimentTag},
		}
		tk := sess.measurement.TestKeys.(*TestKeys)
		tk.Lock()
		tk.TLSHandshakes = append(tk.TLSHandshakes, entry)
		tk.Unlock()
		return nil
	}
	state := qsess.ConnectionState().TLS.ConnectionState
	entry := archival.TLSHandshake{
		CipherSuite:        netxlite.TLSCipherSuiteString(state.CipherSuite),
		Failure:            archival.NewFailure(err),
		NegotiatedProtocol: state.NegotiatedProtocol,
		NoTLSVerify:        tlscfg.InsecureSkipVerify,
		PeerCertificates:   makePeerCerts(trace.PeerCerts(state, err)),
		ServerName:         tlscfg.ServerName,
		TLSVersion:         netxlite.TLSVersionString(state.Version),
		T:                  stop.Sub(sess.measurement.MeasurementStartTimeSaved).Seconds(),
		Tags:               []string{QUICTLSExperimentTag},
	}
	tk := sess.measurement.TestKeys.(*TestKeys)
	tk.Lock()
	tk.TLSHandshakes = append(tk.TLSHandshakes, entry)
	tk.Unlock()
	return m.getHTTP3Transport(qsess, tlscfg, &quic.Config{})
}

// tlsHandshake performs the TLS handshake
func (m *Measurer) tlsHandshake(sess *MeasurementSession, ctx context.Context, conn net.Conn) http.RoundTripper {
	config := &tls.Config{
		ServerName: sess.URL.Hostname(),
		NextProtos: []string{"h2", "http/1.1"},
	}
	tlsconn, state, err := m.handshaker.Handshake(ctx, conn, config)
	stop := time.Now()

	if err != nil {
		entry := archival.TLSHandshake{
			Failure:     archival.NewFailure(err),
			NoTLSVerify: config.InsecureSkipVerify,
			ServerName:  config.ServerName,
			T:           stop.Sub(sess.measurement.MeasurementStartTimeSaved).Seconds(),
			Tags:        []string{TCPTLSExperimentTag},
		}
		tk := sess.measurement.TestKeys.(*TestKeys)
		tk.Lock()
		tk.TLSHandshakes = append(tk.TLSHandshakes, entry)
		tk.Unlock()
		return nil
	}
	entry := archival.TLSHandshake{
		CipherSuite:        netxlite.TLSCipherSuiteString(state.CipherSuite),
		Failure:            archival.NewFailure(err),
		NegotiatedProtocol: state.NegotiatedProtocol,
		NoTLSVerify:        config.InsecureSkipVerify,
		PeerCertificates:   makePeerCerts(trace.PeerCerts(state, err)),
		ServerName:         config.ServerName,
		TLSVersion:         netxlite.TLSVersionString(state.Version),
		T:                  stop.Sub(sess.measurement.MeasurementStartTimeSaved).Seconds(),
		Tags:               []string{TCPTLSExperimentTag},
	}
	tk := sess.measurement.TestKeys.(*TestKeys)
	tk.Lock()
	tk.TLSHandshakes = append(tk.TLSHandshakes, entry)
	tk.Unlock()
	return m.getTransport(state, tlsconn, config)
}

// httpRoundtrip constructs the HTTP request and HTTP client and performs the HTTP Roundtrip with the given transport
func (m *Measurer) httpRoundtrip(sess *MeasurementSession, ctx context.Context, transport http.RoundTripper, redirects chan *redirectInfo) {
	req := sess.redirectedReq
	if req == nil {
		req = m.getRequest(ctx, sess.URL, "GET", nil)
	}
	httpClient := &http.Client{
		Jar:       sess.jar,
		Transport: transport,
	}
	httpClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	defer httpClient.CloseIdleConnections()
	resp, _ := httpClient.Do(req)
	if resp == nil {
		return
	}
	shouldRedirect, includeBody, location := m.redirectBehavior(resp, req)
	if shouldRedirect {
		var reqBody io.ReadCloser = nil
		reqMethod := "GET"
		if includeBody {
			// we created the request with http.NewRequest so we know that the GetBody function will not return an error
			reqBody, _ = req.GetBody()
			reqMethod = req.Method
		}
		redReq := m.getRequest(ctx, location, reqMethod, reqBody)
		redirects <- &redirectInfo{location: location, req: redReq}
	}
}

func (m *Measurer) redirectBehavior(resp *http.Response, req *http.Request) (shouldRedirect, includeBody bool, location *url.URL) {
	switch resp.StatusCode {
	case 301, 302, 303:
		shouldRedirect = true
		includeBody = false
		location, _ = resp.Location()
	case 307, 308:
		shouldRedirect = true
		includeBody = true
		var err error
		// 308s have been observed in the wild being served
		// without Location headers.
		location, err = resp.Location()
		if err != nil {
			shouldRedirect = false
			break
		}
		if req.GetBody == nil {
			shouldRedirect = false
		}
	}
	return shouldRedirect, includeBody, location
}

// getEndpoints connects IP addresses with the port associated with the URL scheme
func (m *Measurer) getEndpoints(addrs []string, scheme string) []string {
	out := []string{}
	if scheme != "http" && scheme != "https" {
		panic("passed an unexpected scheme")
	}
	for _, a := range addrs {
		var port string
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
		endpoint := net.JoinHostPort(a, port)
		out = append(out, endpoint)
	}
	return out
}

// getTransport determines the appropriate HTTP Transport from the ALPN
func (m *Measurer) getTransport(state tls.ConnectionState, conn net.Conn, config *tls.Config) http.RoundTripper {
	// ALPN ?
	switch state.NegotiatedProtocol {
	case "h2":
		// HTTP 2 + TLS.
		return m.getHTTP2Transport(conn, config)
	default:
		// assume HTTP 1.x + TLS.
		dialer := &singleDialerHTTP1{conn: &conn}
		return netxlite.NewHTTPTransport(dialer, config, m.handshaker)
	}
}

// getHTTP3Transport creates am http2.Transport
func (m *Measurer) getHTTP2Transport(conn net.Conn, config *tls.Config) (transport http.RoundTripper) {
	transport = &http2.Transport{
		DialTLS:            (&singleDialerH2{conn: &conn}).DialTLS,
		TLSClientConfig:    config,
		DisableCompression: true,
	}
	transport = &netxlite.HTTPTransportLogger{Logger: log.Log, HTTPTransport: transport.(*http2.Transport)}
	return transport
}

// getHTTP3Transport creates am http3.RoundTripper
func (m *Measurer) getHTTP3Transport(qsess quic.EarlySession, tlscfg *tls.Config, qcfg *quic.Config) *http3.RoundTripper {
	transport := &http3.RoundTripper{
		DisableCompression: true,
		TLSClientConfig:    tlscfg,
		QuicConfig:         qcfg,
		Dial:               (&singleDialerH3{qsess: &qsess}).Dial,
	}
	return transport
}

// getRequest gives us a new HTTP GET request
func (m *Measurer) getRequest(ctx context.Context, URL *url.URL, method string, body io.ReadCloser) *http.Request {
	req, err := http.NewRequest(method, URL.String(), nil)
	runtimex.PanicOnError(err, "http.NewRequest failed")
	req = req.WithContext(ctx)
	req.Header.Set("Accept", httpheader.Accept())
	req.Header.Set("Accept-Language", httpheader.AcceptLanguage())
	req.Host = URL.Hostname()
	if body != nil {
		req.Body = body
	}
	return req
}

// TODO(kelmenhorst): this part is stolen from archival.
// decide: make archival functions public or repeat ourselves?
type dnsQueryType string

func (qtype dnsQueryType) ipoftype(addr string) bool {
	switch qtype {
	case "A":
		return !strings.Contains(addr, ":")
	case "AAAA":
		return strings.Contains(addr, ":")
	}
	return false
}

func (qtype dnsQueryType) makeanswerentry(addr string) archival.DNSAnswerEntry {
	answer := archival.DNSAnswerEntry{AnswerType: string(qtype)}
	asn, org, _ := geolocate.LookupASN(addr)
	answer.ASN = int64(asn)
	answer.ASOrgName = org
	switch qtype {
	case "A":
		answer.IPv4 = addr
	case "AAAA":
		answer.IPv6 = addr
	}
	return answer
}

func makePeerCerts(in []*x509.Certificate) (out []archival.MaybeBinaryValue) {
	for _, e := range in {
		out = append(out, archival.MaybeBinaryValue{Value: string(e.Raw)})
	}
	return
}

// SummaryKeys contains summary keys for this experiment.
//
// Note that this structure is part of the ABI contract with probe-cli
// therefore we should be careful when changing it.
type SummaryKeys struct {
	Accessible bool   `json:"accessible"`
	Blocking   string `json:"blocking"`
	IsAnomaly  bool   `json:"-"`
}

// GetSummaryKeys implements model.ExperimentMeasurer.GetSummaryKeys.
func (m *Measurer) GetSummaryKeys(measurement *model.Measurement) (interface{}, error) {
	sk := SummaryKeys{IsAnomaly: false}
	return sk, nil
}