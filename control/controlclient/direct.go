// Copyright (c) 2020 Tailscale Inc & AUTHORS All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package controlclient

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"net/http"
	"net/url"
	"os"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go4.org/mem"
	"golang.org/x/sync/singleflight"
	"inet.af/netaddr"
	"tailscale.com/control/controlknobs"
	"tailscale.com/envknob"
	"tailscale.com/health"
	"tailscale.com/hostinfo"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/log/logheap"
	"tailscale.com/net/dnscache"
	"tailscale.com/net/dnsfallback"
	"tailscale.com/net/interfaces"
	"tailscale.com/net/netns"
	"tailscale.com/net/netutil"
	"tailscale.com/net/tlsdial"
	"tailscale.com/net/tshttpproxy"
	"tailscale.com/tailcfg"
	"tailscale.com/types/key"
	"tailscale.com/types/logger"
	"tailscale.com/types/netmap"
	"tailscale.com/types/opt"
	"tailscale.com/types/persist"
	"tailscale.com/util/clientmetric"
	"tailscale.com/util/multierr"
	"tailscale.com/util/systemd"
	"tailscale.com/wgengine/monitor"
)

// Direct is the client that connects to a tailcontrol server for a node.
type Direct struct {
	httpc                  *http.Client // HTTP client used to talk to tailcontrol
	serverURL              string       // URL of the tailcontrol server
	timeNow                func() time.Time
	lastPrintMap           time.Time
	newDecompressor        func() (Decompressor, error)
	keepAlive              bool
	logf                   logger.Logf
	linkMon                *monitor.Mon // or nil
	discoPubKey            key.DiscoPublic
	getMachinePrivKey      func() (key.MachinePrivate, error)
	debugFlags             []string
	keepSharerAndUserSplit bool
	skipIPForwardingCheck  bool
	pinger                 Pinger
	popBrowser             func(url string) // or nil

	mu             sync.Mutex        // mutex guards the following fields
	serverKey      key.MachinePublic // original ("legacy") nacl crypto_box-based public key
	serverNoiseKey key.MachinePublic

	sfGroup     singleflight.Group // protects noiseClient creation.
	noiseClient *noiseClient

	persist      persist.Persist
	authKey      string
	tryingNewKey key.NodePrivate
	expiry       *time.Time
	// hostinfo is mutated in-place while mu is held.
	hostinfo      *tailcfg.Hostinfo // always non-nil
	endpoints     []tailcfg.Endpoint
	everEndpoints bool   // whether we've ever had non-empty endpoints
	localPort     uint16 // or zero to mean auto
	lastPingURL   string // last PingRequest.URL received, for dup suppression
}

type Options struct {
	Persist              persist.Persist                    // initial persistent data
	GetMachinePrivateKey func() (key.MachinePrivate, error) // returns the machine key to use
	ServerURL            string                             // URL of the tailcontrol server
	AuthKey              string                             // optional node auth key for auto registration
	TimeNow              func() time.Time                   // time.Now implementation used by Client
	Hostinfo             *tailcfg.Hostinfo                  // non-nil passes ownership, nil means to use default using os.Hostname, etc
	DiscoPublicKey       key.DiscoPublic
	NewDecompressor      func() (Decompressor, error)
	KeepAlive            bool
	Logf                 logger.Logf
	HTTPTestClient       *http.Client     // optional HTTP client to use (for tests only)
	DebugFlags           []string         // debug settings to send to control
	LinkMonitor          *monitor.Mon     // optional link monitor
	PopBrowserURL        func(url string) // optional func to open browser

	// KeepSharerAndUserSplit controls whether the client
	// understands Node.Sharer. If false, the Sharer is mapped to the User.
	KeepSharerAndUserSplit bool

	// SkipIPForwardingCheck declares that the host's IP
	// forwarding works and should not be double-checked by the
	// controlclient package.
	SkipIPForwardingCheck bool

	// Pinger optionally specifies the Pinger to use to satisfy
	// MapResponse.PingRequest queries from the control plane.
	// If nil, PingRequest queries are not answered.
	Pinger Pinger
}

// Pinger is a subset of the wgengine.Engine interface, containing just the Ping method.
type Pinger interface {
	// Ping is a request to start a discovery or TSMP ping with the peer handling
	// the given IP and then call cb with its ping latency & method.
	Ping(ip netaddr.IP, useTSMP bool, cb func(*ipnstate.PingResult))
}

type Decompressor interface {
	DecodeAll(input, dst []byte) ([]byte, error)
	Close()
}

// NewDirect returns a new Direct client.
func NewDirect(opts Options) (*Direct, error) {
	if opts.ServerURL == "" {
		return nil, errors.New("controlclient.New: no server URL specified")
	}
	if opts.GetMachinePrivateKey == nil {
		return nil, errors.New("controlclient.New: no GetMachinePrivateKey specified")
	}
	opts.ServerURL = strings.TrimRight(opts.ServerURL, "/")
	serverURL, err := url.Parse(opts.ServerURL)
	if err != nil {
		return nil, err
	}
	if opts.TimeNow == nil {
		opts.TimeNow = time.Now
	}
	if opts.Logf == nil {
		// TODO(apenwarr): remove this default and fail instead.
		// TODO(bradfitz): ... but then it shouldn't be in Options.
		opts.Logf = log.Printf
	}

	httpc := opts.HTTPTestClient
	if httpc == nil && runtime.GOOS == "js" {
		// In js/wasm, net/http.Transport (as of Go 1.18) will
		// only use the browser's Fetch API if you're using
		// the DefaultClient (or a client without dial hooks
		// etc set).
		httpc = http.DefaultClient
	}
	if httpc == nil {
		dnsCache := &dnscache.Resolver{
			Forward:          dnscache.Get().Forward, // use default cache's forwarder
			UseLastGood:      true,
			LookupIPFallback: dnsfallback.Lookup,
		}
		dialer := netns.NewDialer(opts.Logf)
		tr := http.DefaultTransport.(*http.Transport).Clone()
		tr.Proxy = tshttpproxy.ProxyFromEnvironment
		tshttpproxy.SetTransportGetProxyConnectHeader(tr)
		tr.TLSClientConfig = tlsdial.Config(serverURL.Hostname(), tr.TLSClientConfig)
		tr.DialContext = dnscache.Dialer(dialer.DialContext, dnsCache)
		tr.DialTLSContext = dnscache.TLSDialer(dialer.DialContext, dnsCache, tr.TLSClientConfig)
		tr.ForceAttemptHTTP2 = true
		// Disable implicit gzip compression; the various
		// handlers (register, map, set-dns, etc) do their own
		// zstd compression per naclbox.
		tr.DisableCompression = true
		httpc = &http.Client{Transport: tr}
	}

	c := &Direct{
		httpc:                  httpc,
		getMachinePrivKey:      opts.GetMachinePrivateKey,
		serverURL:              opts.ServerURL,
		timeNow:                opts.TimeNow,
		logf:                   opts.Logf,
		newDecompressor:        opts.NewDecompressor,
		keepAlive:              opts.KeepAlive,
		persist:                opts.Persist,
		authKey:                opts.AuthKey,
		discoPubKey:            opts.DiscoPublicKey,
		debugFlags:             opts.DebugFlags,
		keepSharerAndUserSplit: opts.KeepSharerAndUserSplit,
		linkMon:                opts.LinkMonitor,
		skipIPForwardingCheck:  opts.SkipIPForwardingCheck,
		pinger:                 opts.Pinger,
		popBrowser:             opts.PopBrowserURL,
	}
	if opts.Hostinfo == nil {
		c.SetHostinfo(hostinfo.New())
	} else {
		c.SetHostinfo(opts.Hostinfo)
	}
	return c, nil
}

// Close closes the underlying Noise connection(s).
func (c *Direct) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.noiseClient != nil {
		if err := c.noiseClient.Close(); err != nil {
			return err
		}
	}
	c.noiseClient = nil
	return nil
}

// SetHostinfo clones the provided Hostinfo and remembers it for the
// next update. It reports whether the Hostinfo has changed.
func (c *Direct) SetHostinfo(hi *tailcfg.Hostinfo) bool {
	if hi == nil {
		panic("nil Hostinfo")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if hi.Equal(c.hostinfo) {
		return false
	}
	c.hostinfo = hi.Clone()
	j, _ := json.Marshal(c.hostinfo)
	c.logf("[v1] HostInfo: %s", j)
	return true
}

// SetNetInfo clones the provided NetInfo and remembers it for the
// next update. It reports whether the NetInfo has changed.
func (c *Direct) SetNetInfo(ni *tailcfg.NetInfo) bool {
	if ni == nil {
		panic("nil NetInfo")
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.hostinfo == nil {
		c.logf("[unexpected] SetNetInfo called with no HostInfo; ignoring NetInfo update: %+v", ni)
		return false
	}
	if reflect.DeepEqual(ni, c.hostinfo.NetInfo) {
		return false
	}
	c.hostinfo.NetInfo = ni.Clone()
	return true
}

func (c *Direct) GetPersist() persist.Persist {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.persist
}

func (c *Direct) TryLogout(ctx context.Context) error {
	c.logf("[v1] direct.TryLogout()")

	mustRegen, newURL, err := c.doLogin(ctx, loginOpt{Logout: true})
	c.logf("[v1] TryLogout control response: mustRegen=%v, newURL=%v, err=%v", mustRegen, newURL, err)

	c.mu.Lock()
	c.persist = persist.Persist{}
	c.mu.Unlock()

	return err
}

func (c *Direct) TryLogin(ctx context.Context, t *tailcfg.Oauth2Token, flags LoginFlags) (url string, err error) {
	c.logf("[v1] direct.TryLogin(token=%v, flags=%v)", t != nil, flags)
	return c.doLoginOrRegen(ctx, loginOpt{Token: t, Flags: flags})
}

// WaitLoginURL sits in a long poll waiting for the user to authenticate at url.
//
// On success, newURL and err will both be nil.
func (c *Direct) WaitLoginURL(ctx context.Context, url string) (newURL string, err error) {
	c.logf("[v1] direct.WaitLoginURL")
	return c.doLoginOrRegen(ctx, loginOpt{URL: url})
}

func (c *Direct) doLoginOrRegen(ctx context.Context, opt loginOpt) (newURL string, err error) {
	mustRegen, url, err := c.doLogin(ctx, opt)
	if err != nil {
		return url, err
	}
	if mustRegen {
		opt.Regen = true
		_, url, err = c.doLogin(ctx, opt)
	}
	return url, err
}

// SetExpirySooner attempts to shorten the expiry to the specified time.
func (c *Direct) SetExpirySooner(ctx context.Context, expiry time.Time) error {
	c.logf("[v1] direct.SetExpirySooner()")

	newURL, err := c.doLoginOrRegen(ctx, loginOpt{Expiry: &expiry})
	c.logf("[v1] SetExpirySooner control response: newURL=%v, err=%v", newURL, err)

	return err
}

type loginOpt struct {
	Token  *tailcfg.Oauth2Token
	Flags  LoginFlags
	Regen  bool // generate a new nodekey, can be overridden in doLogin
	URL    string
	Logout bool // set the expiry to the far past, expiring the node
	// Expiry, if non-nil, attempts to set the node expiry to the
	// specified time and cannot be used to extend the expiry.
	// It is ignored if Logout is set since Logout works by setting a
	// expiry time in the far past.
	Expiry *time.Time
}

// httpClient provides a common interface for the noiseClient and
// the NaCl box http.Client.
type httpClient interface {
	Do(req *http.Request) (*http.Response, error)
}

func (c *Direct) doLogin(ctx context.Context, opt loginOpt) (mustRegen bool, newURL string, err error) {
	c.mu.Lock()
	persist := c.persist
	tryingNewKey := c.tryingNewKey
	serverKey := c.serverKey
	serverNoiseKey := c.serverNoiseKey
	authKey := c.authKey
	hi := c.hostinfo.Clone()
	backendLogID := hi.BackendLogID
	expired := c.expiry != nil && !c.expiry.IsZero() && c.expiry.Before(c.timeNow())
	c.mu.Unlock()

	machinePrivKey, err := c.getMachinePrivKey()
	if err != nil {
		return false, "", fmt.Errorf("getMachinePrivKey: %w", err)
	}
	if machinePrivKey.IsZero() {
		return false, "", errors.New("getMachinePrivKey returned zero key")
	}

	regen := opt.Regen
	if opt.Logout {
		c.logf("logging out...")
	} else {
		if expired {
			c.logf("Old key expired -> regen=true")
			systemd.Status("key expired; run 'tailscale up' to authenticate")
			regen = true
		}
		if (opt.Flags & LoginInteractive) != 0 {
			c.logf("LoginInteractive -> regen=true")
			regen = true
		}
	}

	c.logf("doLogin(regen=%v, hasUrl=%v)", regen, opt.URL != "")
	if serverKey.IsZero() {
		keys, err := loadServerPubKeys(ctx, c.httpc, c.serverURL)
		if err != nil {
			return regen, opt.URL, err
		}
		c.logf("control server key %s from %s", serverKey.ShortString(), c.serverURL)

		c.mu.Lock()
		c.serverKey = keys.LegacyPublicKey
		c.serverNoiseKey = keys.PublicKey
		c.mu.Unlock()
		serverKey = keys.LegacyPublicKey
		serverNoiseKey = keys.PublicKey

		// For servers supporting the Noise transport,
		// proactively shut down our TLS TCP connection.
		// We're not going to need it and it's nicer to the
		// server.
		if !serverNoiseKey.IsZero() {
			c.httpc.CloseIdleConnections()
		}
	}
	var oldNodeKey key.NodePublic
	switch {
	case opt.Logout:
		tryingNewKey = persist.PrivateNodeKey
	case opt.URL != "":
		// Nothing.
	case regen || persist.PrivateNodeKey.IsZero():
		c.logf("Generating a new nodekey.")
		persist.OldPrivateNodeKey = persist.PrivateNodeKey
		tryingNewKey = key.NewNode()
	default:
		// Try refreshing the current key first
		tryingNewKey = persist.PrivateNodeKey
	}
	if !persist.OldPrivateNodeKey.IsZero() {
		oldNodeKey = persist.OldPrivateNodeKey.Public()
	}

	if tryingNewKey.IsZero() {
		if opt.Logout {
			return false, "", errors.New("no nodekey to log out")
		}
		log.Fatalf("tryingNewKey is empty, give up")
	}
	if backendLogID == "" {
		err = errors.New("hostinfo: BackendLogID missing")
		return regen, opt.URL, err
	}
	now := time.Now().Round(time.Second)
	request := tailcfg.RegisterRequest{
		Version:    1,
		OldNodeKey: oldNodeKey,
		NodeKey:    tryingNewKey.Public(),
		Hostinfo:   hi,
		Followup:   opt.URL,
		Timestamp:  &now,
		Ephemeral:  (opt.Flags & LoginEphemeral) != 0,
	}
	if opt.Logout {
		request.Expiry = time.Unix(123, 0) // far in the past
	} else if opt.Expiry != nil {
		request.Expiry = *opt.Expiry
	}
	c.logf("RegisterReq: onode=%v node=%v fup=%v",
		request.OldNodeKey.ShortString(),
		request.NodeKey.ShortString(), opt.URL != "")
	request.Auth.Oauth2Token = opt.Token
	request.Auth.Provider = persist.Provider
	request.Auth.LoginName = persist.LoginName
	request.Auth.AuthKey = authKey
	err = signRegisterRequest(&request, c.serverURL, c.serverKey, machinePrivKey.Public())
	if err != nil {
		// If signing failed, clear all related fields
		request.SignatureType = tailcfg.SignatureNone
		request.Timestamp = nil
		request.DeviceCert = nil
		request.Signature = nil

		// Don't log the common error types. Signatures are not usually enabled,
		// so these are expected.
		if !errors.Is(err, errCertificateNotConfigured) && !errors.Is(err, errNoCertStore) {
			c.logf("RegisterReq sign error: %v", err)
		}
	}
	if debugRegister {
		j, _ := json.MarshalIndent(request, "", "\t")
		c.logf("RegisterRequest: %s", j)
	}

	// URL and httpc are protocol specific.
	var url string
	var httpc httpClient
	if serverNoiseKey.IsZero() {
		httpc = c.httpc
		url = fmt.Sprintf("%s/machine/%s", c.serverURL, machinePrivKey.Public().UntypedHexString())
	} else {
		request.Version = tailcfg.CurrentCapabilityVersion
		httpc, err = c.getNoiseClient()
		if err != nil {
			return regen, opt.URL, fmt.Errorf("getNoiseClient: %w", err)
		}
		url = fmt.Sprintf("%s/machine/register", c.serverURL)
		url = strings.Replace(url, "http:", "https:", 1)
	}
	bodyData, err := encode(request, serverKey, serverNoiseKey, machinePrivKey)
	if err != nil {
		return regen, opt.URL, err
	}
	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyData))
	if err != nil {
		return regen, opt.URL, err
	}
	res, err := httpc.Do(req)
	if err != nil {
		return regen, opt.URL, fmt.Errorf("register request: %w", err)
	}
	if res.StatusCode != 200 {
		msg, _ := ioutil.ReadAll(res.Body)
		res.Body.Close()
		return regen, opt.URL, fmt.Errorf("register request: http %d: %.200s",
			res.StatusCode, strings.TrimSpace(string(msg)))
	}
	resp := tailcfg.RegisterResponse{}
	if err := decode(res, &resp, serverKey, serverNoiseKey, machinePrivKey); err != nil {
		c.logf("error decoding RegisterResponse with server key %s and machine key %s: %v", serverKey, machinePrivKey.Public(), err)
		return regen, opt.URL, fmt.Errorf("register request: %v", err)
	}
	if debugRegister {
		j, _ := json.MarshalIndent(resp, "", "\t")
		c.logf("RegisterResponse: %s", j)
	}

	// Log without PII:
	c.logf("RegisterReq: got response; nodeKeyExpired=%v, machineAuthorized=%v; authURL=%v",
		resp.NodeKeyExpired, resp.MachineAuthorized, resp.AuthURL != "")

	if resp.Error != "" {
		return false, "", UserVisibleError(resp.Error)
	}
	if resp.NodeKeyExpired {
		if regen {
			return true, "", fmt.Errorf("weird: regen=true but server says NodeKeyExpired: %v", request.NodeKey)
		}
		c.logf("server reports new node key %v has expired",
			request.NodeKey.ShortString())
		return true, "", nil
	}
	if resp.Login.Provider != "" {
		persist.Provider = resp.Login.Provider
	}
	if resp.Login.LoginName != "" {
		persist.LoginName = resp.Login.LoginName
	}

	// TODO(crawshaw): RegisterResponse should be able to mechanically
	// communicate some extra instructions from the server:
	//	- new node key required
	//	- machine key no longer supported
	//	- user is disabled

	if resp.AuthURL != "" {
		c.logf("AuthURL is %v", resp.AuthURL)
	} else {
		c.logf("[v1] No AuthURL")
	}

	c.mu.Lock()
	if resp.AuthURL == "" {
		// key rotation is complete
		persist.PrivateNodeKey = tryingNewKey
	} else {
		// save it for the retry-with-URL
		c.tryingNewKey = tryingNewKey
	}
	c.persist = persist
	c.mu.Unlock()

	if err != nil {
		return regen, "", err
	}
	if ctx.Err() != nil {
		return regen, "", ctx.Err()
	}
	return false, resp.AuthURL, nil
}

func sameEndpoints(a, b []tailcfg.Endpoint) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// newEndpoints acquires c.mu and sets the local port and endpoints and reports
// whether they've changed.
//
// It does not retain the provided slice.
func (c *Direct) newEndpoints(localPort uint16, endpoints []tailcfg.Endpoint) (changed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Nothing new?
	if c.localPort == localPort && sameEndpoints(c.endpoints, endpoints) {
		return false // unchanged
	}
	var epStrs []string
	for _, ep := range endpoints {
		epStrs = append(epStrs, ep.Addr.String())
	}
	c.logf("[v2] client.newEndpoints(%v, %v)", localPort, epStrs)
	c.localPort = localPort
	c.endpoints = append(c.endpoints[:0], endpoints...)
	if len(endpoints) > 0 {
		c.everEndpoints = true
	}
	return true // changed
}

// SetEndpoints updates the list of locally advertised endpoints.
// It won't be replicated to the server until a *fresh* call to PollNetMap().
// You don't need to restart PollNetMap if we return changed==false.
func (c *Direct) SetEndpoints(localPort uint16, endpoints []tailcfg.Endpoint) (changed bool) {
	// (no log message on function entry, because it clutters the logs
	//  if endpoints haven't changed. newEndpoints() will log it.)
	return c.newEndpoints(localPort, endpoints)
}

func inTest() bool { return flag.Lookup("test.v") != nil }

// PollNetMap makes a /map request to download the network map, calling cb with
// each new netmap.
//
// maxPolls is how many network maps to download; common values are 1
// or -1 (to keep a long-poll query open to the server).
func (c *Direct) PollNetMap(ctx context.Context, maxPolls int, cb func(*netmap.NetworkMap)) error {
	return c.sendMapRequest(ctx, maxPolls, cb)
}

// SendLiteMapUpdate makes a /map request to update the server of our latest state,
// but does not fetch anything. It returns an error if the server did not return a
// successful 200 OK response.
func (c *Direct) SendLiteMapUpdate(ctx context.Context) error {
	return c.sendMapRequest(ctx, 1, nil)
}

// If we go more than pollTimeout without hearing from the server,
// end the long poll. We should be receiving a keep alive ping
// every minute.
const pollTimeout = 120 * time.Second

// cb nil means to omit peers.
func (c *Direct) sendMapRequest(ctx context.Context, maxPolls int, cb func(*netmap.NetworkMap)) error {
	metricMapRequests.Add(1)
	metricMapRequestsActive.Add(1)
	defer metricMapRequestsActive.Add(-1)
	if maxPolls == -1 {
		metricMapRequestsPoll.Add(1)
	} else {
		metricMapRequestsLite.Add(1)
	}

	c.mu.Lock()
	persist := c.persist
	serverURL := c.serverURL
	serverKey := c.serverKey
	serverNoiseKey := c.serverNoiseKey
	hi := c.hostinfo.Clone()
	backendLogID := hi.BackendLogID
	localPort := c.localPort
	var epStrs []string
	var epTypes []tailcfg.EndpointType
	for _, ep := range c.endpoints {
		epStrs = append(epStrs, ep.Addr.String())
		epTypes = append(epTypes, ep.Type)
	}
	everEndpoints := c.everEndpoints
	c.mu.Unlock()

	machinePrivKey, err := c.getMachinePrivKey()
	if err != nil {
		return fmt.Errorf("getMachinePrivKey: %w", err)
	}
	if machinePrivKey.IsZero() {
		return errors.New("getMachinePrivKey returned zero key")
	}

	if persist.PrivateNodeKey.IsZero() {
		return errors.New("privateNodeKey is zero")
	}
	if backendLogID == "" {
		return errors.New("hostinfo: BackendLogID missing")
	}

	allowStream := maxPolls != 1
	c.logf("[v1] PollNetMap: stream=%v :%v ep=%v", allowStream, localPort, epStrs)

	vlogf := logger.Discard
	if Debug.NetMap {
		// TODO(bradfitz): update this to use "[v2]" prefix perhaps? but we don't
		// want to upload it always.
		vlogf = c.logf
	}

	request := &tailcfg.MapRequest{
		Version:       tailcfg.CurrentCapabilityVersion,
		KeepAlive:     c.keepAlive,
		NodeKey:       persist.PrivateNodeKey.Public(),
		DiscoKey:      c.discoPubKey,
		Endpoints:     epStrs,
		EndpointTypes: epTypes,
		Stream:        allowStream,
		Hostinfo:      hi,
		DebugFlags:    c.debugFlags,
		OmitPeers:     cb == nil,
	}
	var extraDebugFlags []string
	if hi != nil && c.linkMon != nil && !c.skipIPForwardingCheck &&
		ipForwardingBroken(hi.RoutableIPs, c.linkMon.InterfaceState()) {
		extraDebugFlags = append(extraDebugFlags, "warn-ip-forwarding-off")
	}
	if health.RouterHealth() != nil {
		extraDebugFlags = append(extraDebugFlags, "warn-router-unhealthy")
	}
	if health.NetworkCategoryHealth() != nil {
		extraDebugFlags = append(extraDebugFlags, "warn-network-category-unhealthy")
	}
	if hostinfo.DisabledEtcAptSource() {
		extraDebugFlags = append(extraDebugFlags, "warn-etc-apt-source-disabled")
	}
	if len(extraDebugFlags) > 0 {
		old := request.DebugFlags
		request.DebugFlags = append(old[:len(old):len(old)], extraDebugFlags...)
	}
	if c.newDecompressor != nil {
		request.Compress = "zstd"
	}
	// On initial startup before we know our endpoints, set the ReadOnly flag
	// to tell the control server not to distribute out our (empty) endpoints to peers.
	// Presumably we'll learn our endpoints in a half second and do another post
	// with useful results. The first POST just gets us the DERP map which we
	// need to do the STUN queries to discover our endpoints.
	// TODO(bradfitz): we skip this optimization in tests, though,
	// because the e2e tests are currently hyperspecific about the
	// ordering of things. The e2e tests need love.
	if len(epStrs) == 0 && !everEndpoints && !inTest() {
		request.ReadOnly = true
	}

	bodyData, err := encode(request, serverKey, serverNoiseKey, machinePrivKey)
	if err != nil {
		vlogf("netmap: encode: %v", err)
		return err
	}

	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	machinePubKey := machinePrivKey.Public()
	t0 := time.Now()

	// Url and httpc are protocol specific.
	var url string
	var httpc httpClient
	if serverNoiseKey.IsZero() {
		httpc = c.httpc
		url = fmt.Sprintf("%s/machine/%s/map", serverURL, machinePubKey.UntypedHexString())
	} else {
		httpc, err = c.getNoiseClient()
		if err != nil {
			return fmt.Errorf("getNoiseClient: %w", err)
		}
		url = fmt.Sprintf("%s/machine/map", serverURL)
		url = strings.Replace(url, "http:", "https:", 1)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", url, bytes.NewReader(bodyData))
	if err != nil {
		return err
	}

	res, err := httpc.Do(req)
	if err != nil {
		vlogf("netmap: Do: %v", err)
		return err
	}
	vlogf("netmap: Do = %v after %v", res.StatusCode, time.Since(t0).Round(time.Millisecond))
	if res.StatusCode != 200 {
		msg, _ := ioutil.ReadAll(res.Body)
		res.Body.Close()
		return fmt.Errorf("initial fetch failed %d: %.200s",
			res.StatusCode, strings.TrimSpace(string(msg)))
	}
	defer res.Body.Close()

	health.NoteMapRequestHeard(request)

	if cb == nil {
		io.Copy(ioutil.Discard, res.Body)
		return nil
	}

	timeout := time.NewTimer(pollTimeout)
	timeoutReset := make(chan struct{})
	pollDone := make(chan struct{})
	defer close(pollDone)
	go func() {
		for {
			select {
			case <-pollDone:
				vlogf("netmap: ending timeout goroutine")
				return
			case <-timeout.C:
				c.logf("map response long-poll timed out!")
				cancel()
				return
			case <-timeoutReset:
				if !timeout.Stop() {
					select {
					case <-timeout.C:
					case <-pollDone:
						vlogf("netmap: ending timeout goroutine")
						return
					}
				}
				vlogf("netmap: reset timeout timer")
				timeout.Reset(pollTimeout)
			}
		}
	}()

	sess := newMapSession(persist.PrivateNodeKey)
	sess.logf = c.logf
	sess.vlogf = vlogf
	sess.machinePubKey = machinePubKey
	sess.keepSharerAndUserSplit = c.keepSharerAndUserSplit

	// If allowStream, then the server will use an HTTP long poll to
	// return incremental results. There is always one response right
	// away, followed by a delay, and eventually others.
	// If !allowStream, it'll still send the first result in exactly
	// the same format before just closing the connection.
	// We can use this same read loop either way.
	var msg []byte
	for i := 0; i < maxPolls || maxPolls < 0; i++ {
		vlogf("netmap: starting size read after %v (poll %v)", time.Since(t0).Round(time.Millisecond), i)
		var siz [4]byte
		if _, err := io.ReadFull(res.Body, siz[:]); err != nil {
			vlogf("netmap: size read error after %v: %v", time.Since(t0).Round(time.Millisecond), err)
			return err
		}
		size := binary.LittleEndian.Uint32(siz[:])
		vlogf("netmap: read size %v after %v", size, time.Since(t0).Round(time.Millisecond))
		msg = append(msg[:0], make([]byte, size)...)
		if _, err := io.ReadFull(res.Body, msg); err != nil {
			vlogf("netmap: body read error: %v", err)
			return err
		}
		vlogf("netmap: read body after %v", time.Since(t0).Round(time.Millisecond))

		var resp tailcfg.MapResponse
		if err := c.decodeMsg(msg, &resp, machinePrivKey); err != nil {
			vlogf("netmap: decode error: %v")
			return err
		}

		metricMapResponseMessages.Add(1)

		if allowStream {
			health.GotStreamedMapResponse()
		}

		if pr := resp.PingRequest; pr != nil && c.isUniquePingRequest(pr) {
			metricMapResponsePings.Add(1)
			go answerPing(c.logf, c.httpc, pr)
		}
		if u := resp.PopBrowserURL; u != "" && u != sess.lastPopBrowserURL {
			sess.lastPopBrowserURL = u
			if c.popBrowser != nil {
				c.logf("netmap: control says to open URL %v; opening...", u)
				c.popBrowser(u)
			} else {
				c.logf("netmap: control says to open URL %v; no popBrowser func", u)
			}
		}
		if resp.ControlTime != nil && !resp.ControlTime.IsZero() {
			c.logf.JSON(1, "controltime", resp.ControlTime.UTC())
		}
		if resp.KeepAlive {
			vlogf("netmap: got keep-alive")
		} else {
			vlogf("netmap: got new map")
		}

		select {
		case timeoutReset <- struct{}{}:
			vlogf("netmap: sent timer reset")
		case <-ctx.Done():
			c.logf("[v1] netmap: not resetting timer; context done: %v", ctx.Err())
			return ctx.Err()
		}
		if resp.KeepAlive {
			metricMapResponseKeepAlives.Add(1)
			continue
		}

		metricMapResponseMap.Add(1)
		if i > 0 {
			metricMapResponseMapDelta.Add(1)
		}

		hasDebug := resp.Debug != nil
		// being conservative here, if Debug not present set to False
		controlknobs.SetDisableUPnP(hasDebug && resp.Debug.DisableUPnP.EqualBool(true))
		if hasDebug {
			if code := resp.Debug.Exit; code != nil {
				c.logf("exiting process with status %v per controlplane", *code)
				os.Exit(*code)
			}
			if resp.Debug.LogHeapPprof {
				go logheap.LogHeap(resp.Debug.LogHeapURL)
			}
			if resp.Debug.GoroutineDumpURL != "" {
				go dumpGoroutinesToURL(c.httpc, resp.Debug.GoroutineDumpURL)
			}
			setControlAtomic(&controlUseDERPRoute, resp.Debug.DERPRoute)
			setControlAtomic(&controlTrimWGConfig, resp.Debug.TrimWGConfig)
			if sleep := time.Duration(resp.Debug.SleepSeconds * float64(time.Second)); sleep > 0 {
				if err := sleepAsRequested(ctx, c.logf, timeoutReset, sleep); err != nil {
					return err
				}
			}
		}

		nm := sess.netmapForResponse(&resp)
		if nm.SelfNode == nil {
			c.logf("MapResponse lacked node")
			return errors.New("MapResponse lacked node")
		}

		if Debug.StripEndpoints {
			for _, p := range resp.Peers {
				p.Endpoints = nil
			}
		}
		if Debug.StripCaps {
			nm.SelfNode.Capabilities = nil
		}

		// Get latest localPort. This might've changed if
		// a lite map update occurred meanwhile. This only affects
		// the end-to-end test.
		// TODO(bradfitz): remove the NetworkMap.LocalPort field entirely.
		c.mu.Lock()
		nm.LocalPort = c.localPort
		c.mu.Unlock()

		// Occasionally print the netmap header.
		// This is handy for debugging, and our logs processing
		// pipeline depends on it. (TODO: Remove this dependency.)
		// Code elsewhere prints netmap diffs every time they are received.
		now := c.timeNow()
		if now.Sub(c.lastPrintMap) >= 5*time.Minute {
			c.lastPrintMap = now
			c.logf("[v1] new network map[%d]:\n%s", i, nm.VeryConcise())
		}

		c.mu.Lock()
		c.expiry = &nm.Expiry
		c.mu.Unlock()

		cb(nm)
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return nil
}

// decode JSON decodes the res.Body into v. If serverNoiseKey is not specified,
// it uses the serverKey and mkey to decode the message from the NaCl-crypto-box.
func decode(res *http.Response, v any, serverKey, serverNoiseKey key.MachinePublic, mkey key.MachinePrivate) error {
	defer res.Body.Close()
	msg, err := ioutil.ReadAll(io.LimitReader(res.Body, 1<<20))
	if err != nil {
		return err
	}
	if res.StatusCode != 200 {
		return fmt.Errorf("%d: %v", res.StatusCode, string(msg))
	}
	if !serverNoiseKey.IsZero() {
		return json.Unmarshal(msg, v)
	}
	return decodeMsg(msg, v, serverKey, mkey)
}

var (
	debugMap      = envknob.Bool("TS_DEBUG_MAP")
	debugRegister = envknob.Bool("TS_DEBUG_REGISTER")
)

var jsonEscapedZero = []byte(`\u0000`)

// decodeMsg is responsible for uncompressing msg and unmarshaling into v.
// If c.serverNoiseKey is not specified, it uses the c.serverKey and mkey
// to first the decrypt msg from the NaCl-crypto-box.
func (c *Direct) decodeMsg(msg []byte, v any, mkey key.MachinePrivate) error {
	c.mu.Lock()
	serverKey := c.serverKey
	serverNoiseKey := c.serverNoiseKey
	c.mu.Unlock()

	var decrypted []byte
	if serverNoiseKey.IsZero() {
		var ok bool
		decrypted, ok = mkey.OpenFrom(serverKey, msg)
		if !ok {
			return errors.New("cannot decrypt response")
		}
	} else {
		decrypted = msg
	}
	var b []byte
	if c.newDecompressor == nil {
		b = decrypted
	} else {
		decoder, err := c.newDecompressor()
		if err != nil {
			return err
		}
		defer decoder.Close()
		b, err = decoder.DecodeAll(decrypted, nil)
		if err != nil {
			return err
		}
	}
	if debugMap {
		var buf bytes.Buffer
		json.Indent(&buf, b, "", "    ")
		log.Printf("MapResponse: %s", buf.Bytes())
	}

	if bytes.Contains(b, jsonEscapedZero) {
		log.Printf("[unexpected] zero byte in controlclient.Direct.decodeMsg into %T: %q", v, b)
	}
	if err := json.Unmarshal(b, v); err != nil {
		return fmt.Errorf("response: %v", err)
	}
	return nil

}

func decodeMsg(msg []byte, v any, serverKey key.MachinePublic, machinePrivKey key.MachinePrivate) error {
	decrypted, ok := machinePrivKey.OpenFrom(serverKey, msg)
	if !ok {
		return errors.New("cannot decrypt response")
	}
	if bytes.Contains(decrypted, jsonEscapedZero) {
		log.Printf("[unexpected] zero byte in controlclient decodeMsg into %T: %q", v, decrypted)
	}
	if err := json.Unmarshal(decrypted, v); err != nil {
		return fmt.Errorf("response: %v", err)
	}
	return nil
}

// encode JSON encodes v. If serverNoiseKey is not specified, it uses the serverKey and mkey to
// seal the message into a NaCl-crypto-box.
func encode(v any, serverKey, serverNoiseKey key.MachinePublic, mkey key.MachinePrivate) ([]byte, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	if debugMap {
		if _, ok := v.(*tailcfg.MapRequest); ok {
			log.Printf("MapRequest: %s", b)
		}
	}
	if !serverNoiseKey.IsZero() {
		return b, nil
	}
	return mkey.SealTo(serverKey, b), nil
}

func loadServerPubKeys(ctx context.Context, httpc *http.Client, serverURL string) (*tailcfg.OverTLSPublicKeyResponse, error) {
	keyURL := fmt.Sprintf("%v/key?v=%d", serverURL, tailcfg.CurrentCapabilityVersion)
	req, err := http.NewRequestWithContext(ctx, "GET", keyURL, nil)
	if err != nil {
		return nil, fmt.Errorf("create control key request: %v", err)
	}
	res, err := httpc.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetch control key: %v", err)
	}
	defer res.Body.Close()
	b, err := ioutil.ReadAll(io.LimitReader(res.Body, 64<<10))
	if err != nil {
		return nil, fmt.Errorf("fetch control key response: %v", err)
	}
	if res.StatusCode != 200 {
		return nil, fmt.Errorf("fetch control key: %d", res.StatusCode)
	}
	var out tailcfg.OverTLSPublicKeyResponse
	jsonErr := json.Unmarshal(b, &out)
	if jsonErr == nil {
		return &out, nil
	}

	// Some old control servers might not be updated to send the new format.
	// Accept the old pre-JSON format too.
	out = tailcfg.OverTLSPublicKeyResponse{}
	k, err := key.ParseMachinePublicUntyped(mem.B(b))
	if err != nil {
		return nil, multierr.New(jsonErr, err)
	}
	out.LegacyPublicKey = k
	return &out, nil
}

// Debug contains temporary internal-only debug knobs.
// They're unexported to not draw attention to them.
var Debug = initDebug()

type debug struct {
	NetMap         bool
	ProxyDNS       bool
	Disco          bool
	StripEndpoints bool // strip endpoints from control (only use disco messages)
	StripCaps      bool // strip all local node's control-provided capabilities
}

func initDebug() debug {
	return debug{
		NetMap:         envknob.Bool("TS_DEBUG_NETMAP"),
		ProxyDNS:       envknob.Bool("TS_DEBUG_PROXY_DNS"),
		StripEndpoints: envknob.Bool("TS_DEBUG_STRIP_ENDPOINTS"),
		StripCaps:      envknob.Bool("TS_DEBUG_STRIP_CAPS"),
		Disco:          envknob.BoolDefaultTrue("TS_DEBUG_USE_DISCO"),
	}
}

var clockNow = time.Now

// opt.Bool configs from control.
var (
	controlUseDERPRoute atomic.Value
	controlTrimWGConfig atomic.Value
)

func setControlAtomic(dst *atomic.Value, v opt.Bool) {
	old, ok := dst.Load().(opt.Bool)
	if !ok || old != v {
		dst.Store(v)
	}
}

// DERPRouteFlag reports the last reported value from control for whether
// DERP route optimization (Issue 150) should be enabled.
func DERPRouteFlag() opt.Bool {
	v, _ := controlUseDERPRoute.Load().(opt.Bool)
	return v
}

// TrimWGConfig reports the last reported value from control for whether
// we should do lazy wireguard configuration.
func TrimWGConfig() opt.Bool {
	v, _ := controlTrimWGConfig.Load().(opt.Bool)
	return v
}

// ipForwardingBroken reports whether the system's IP forwarding is disabled
// and will definitely not work for the routes provided.
//
// It should not return false positives.
//
// TODO(bradfitz): Change controlclient.Options.SkipIPForwardingCheck into a
// func([]netaddr.IPPrefix) error signature instead.
func ipForwardingBroken(routes []netaddr.IPPrefix, state *interfaces.State) bool {
	warn, err := netutil.CheckIPForwarding(routes, state)
	if err != nil {
		// Oh well, we tried. This is just for debugging.
		// We don't want false positives.
		// TODO: maybe we want a different warning for inability to check?
		return false
	}
	return warn != nil
}

// isUniquePingRequest reports whether pr contains a new PingRequest.URL
// not already handled, noting its value when returning true.
func (c *Direct) isUniquePingRequest(pr *tailcfg.PingRequest) bool {
	if pr == nil || pr.URL == "" {
		// Bogus.
		return false
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if pr.URL == c.lastPingURL {
		return false
	}
	c.lastPingURL = pr.URL
	return true
}

func answerPing(logf logger.Logf, c *http.Client, pr *tailcfg.PingRequest) {
	if pr.URL == "" {
		logf("invalid PingRequest with no URL")
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, "HEAD", pr.URL, nil)
	if err != nil {
		logf("http.NewRequestWithContext(%q): %v", pr.URL, err)
		return
	}
	if pr.Log {
		logf("answerPing: sending ping to %v ...", pr.URL)
	}
	t0 := time.Now()
	_, err = c.Do(req)
	d := time.Since(t0).Round(time.Millisecond)
	if err != nil {
		logf("answerPing error: %v to %v (after %v)", err, pr.URL, d)
	} else if pr.Log {
		logf("answerPing complete to %v (after %v)", pr.URL, d)
	}
}

func sleepAsRequested(ctx context.Context, logf logger.Logf, timeoutReset chan<- struct{}, d time.Duration) error {
	const maxSleep = 5 * time.Minute
	if d > maxSleep {
		logf("sleeping for %v, capped from server-requested %v ...", maxSleep, d)
		d = maxSleep
	} else {
		logf("sleeping for server-requested %v ...", d)
	}

	ticker := time.NewTicker(pollTimeout / 2)
	defer ticker.Stop()
	timer := time.NewTimer(d)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case <-ticker.C:
			select {
			case timeoutReset <- struct{}{}:
			case <-timer.C:
				return nil
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	}
}

// getNoiseClient returns the noise client, creating one if one doesn't exist.
func (c *Direct) getNoiseClient() (*noiseClient, error) {
	c.mu.Lock()
	serverNoiseKey := c.serverNoiseKey
	nc := c.noiseClient
	c.mu.Unlock()
	if serverNoiseKey.IsZero() {
		return nil, errors.New("zero serverNoiseKey")
	}
	if nc != nil {
		return nc, nil
	}
	np, err, _ := c.sfGroup.Do("noise", func() (any, error) {
		k, err := c.getMachinePrivKey()
		if err != nil {
			return nil, err
		}

		nc, err = newNoiseClient(k, serverNoiseKey, c.serverURL)
		if err != nil {
			return nil, err
		}
		c.mu.Lock()
		defer c.mu.Unlock()
		c.noiseClient = nc
		return nc, nil
	})
	if err != nil {
		return nil, err
	}
	return np.(*noiseClient), nil
}

// setDNSNoise sends the SetDNSRequest request to the control plane server over Noise,
// requesting a DNS record be created or updated.
func (c *Direct) setDNSNoise(ctx context.Context, req *tailcfg.SetDNSRequest) error {
	newReq := *req
	newReq.Version = tailcfg.CurrentCapabilityVersion
	np, err := c.getNoiseClient()
	if err != nil {
		return err
	}
	bodyData, err := json.Marshal(newReq)
	if err != nil {
		return err
	}
	res, err := np.Post(fmt.Sprintf("https://%v/%v", np.serverHost, "machine/set-dns"), "application/json", bytes.NewReader(bodyData))
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		msg, _ := ioutil.ReadAll(res.Body)
		return fmt.Errorf("set-dns response: %v, %.200s", res.Status, strings.TrimSpace(string(msg)))
	}
	var setDNSRes tailcfg.SetDNSResponse
	if err := json.NewDecoder(res.Body).Decode(&setDNSRes); err != nil {
		c.logf("error decoding SetDNSResponse: %v", err)
		return fmt.Errorf("set-dns-response: %w", err)
	}

	return nil
}

// noiseConfigured reports whether the client can communicate with Control
// over Noise.
func (c *Direct) noiseConfigured() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !c.serverNoiseKey.IsZero()
}

// SetDNS sends the SetDNSRequest request to the control plane server,
// requesting a DNS record be created or updated.
func (c *Direct) SetDNS(ctx context.Context, req *tailcfg.SetDNSRequest) (err error) {
	metricSetDNS.Add(1)
	defer func() {
		if err != nil {
			metricSetDNSError.Add(1)
		}
	}()
	if c.noiseConfigured() {
		return c.setDNSNoise(ctx, req)
	}
	c.mu.Lock()
	serverKey := c.serverKey
	c.mu.Unlock()

	if serverKey.IsZero() {
		return errors.New("zero serverKey")
	}
	machinePrivKey, err := c.getMachinePrivKey()
	if err != nil {
		return fmt.Errorf("getMachinePrivKey: %w", err)
	}
	if machinePrivKey.IsZero() {
		return errors.New("getMachinePrivKey returned zero key")
	}

	// TODO(maisem): dedupe this codepath from SetDNSNoise.
	var serverNoiseKey key.MachinePublic
	bodyData, err := encode(req, serverKey, serverNoiseKey, machinePrivKey)
	if err != nil {
		return err
	}
	body := bytes.NewReader(bodyData)

	u := fmt.Sprintf("%s/machine/%s/set-dns", c.serverURL, machinePrivKey.Public().UntypedHexString())
	hreq, err := http.NewRequestWithContext(ctx, "POST", u, body)
	if err != nil {
		return err
	}
	res, err := c.httpc.Do(hreq)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != 200 {
		msg, _ := ioutil.ReadAll(res.Body)
		return fmt.Errorf("set-dns response: %v, %.200s", res.Status, strings.TrimSpace(string(msg)))
	}
	var setDNSRes tailcfg.SetDNSResponse
	if err := decode(res, &setDNSRes, serverKey, serverNoiseKey, machinePrivKey); err != nil {
		c.logf("error decoding SetDNSResponse with server key %s and machine key %s: %v", serverKey, machinePrivKey.Public(), err)
		return fmt.Errorf("set-dns-response: %w", err)
	}

	return nil
}

func (c *Direct) DoNoiseRequest(req *http.Request) (*http.Response, error) {
	nc, err := c.getNoiseClient()
	if err != nil {
		return nil, err
	}
	return nc.Do(req)
}

// tsmpPing sends a Ping to pr.IP, and sends an http request back to pr.URL
// with ping response data.
func tsmpPing(logf logger.Logf, c *http.Client, pr *tailcfg.PingRequest, pinger Pinger) error {
	var err error
	if pr.URL == "" {
		return errors.New("invalid PingRequest with no URL")
	}
	if pr.IP.IsZero() {
		return errors.New("PingRequest without IP")
	}
	if !strings.Contains(pr.Types, "TSMP") {
		return fmt.Errorf("PingRequest with no TSMP in Types, got %q", pr.Types)
	}

	now := time.Now()
	pinger.Ping(pr.IP, true, func(res *ipnstate.PingResult) {
		// Currently does not check for error since we just return if it fails.
		err = postPingResult(now, logf, c, pr, res)
	})
	return err
}

func postPingResult(now time.Time, logf logger.Logf, c *http.Client, pr *tailcfg.PingRequest, res *ipnstate.PingResult) error {
	if res.Err != "" {
		return errors.New(res.Err)
	}
	duration := time.Since(now)
	if pr.Log {
		logf("TSMP ping to %v completed in %v seconds. pinger.Ping took %v seconds", pr.IP, res.LatencySeconds, duration.Seconds())
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	jsonPingRes, err := json.Marshal(res)
	if err != nil {
		return err
	}
	// Send the results of the Ping, back to control URL.
	req, err := http.NewRequestWithContext(ctx, "POST", pr.URL, bytes.NewBuffer(jsonPingRes))
	if err != nil {
		return fmt.Errorf("http.NewRequestWithContext(%q): %w", pr.URL, err)
	}
	if pr.Log {
		logf("tsmpPing: sending ping results to %v ...", pr.URL)
	}
	t0 := time.Now()
	_, err = c.Do(req)
	d := time.Since(t0).Round(time.Millisecond)
	if err != nil {
		return fmt.Errorf("tsmpPing error: %w to %v (after %v)", err, pr.URL, d)
	} else if pr.Log {
		logf("tsmpPing complete to %v (after %v)", pr.URL, d)
	}
	return nil
}

var (
	metricMapRequestsActive = clientmetric.NewGauge("controlclient_map_requests_active")

	metricMapRequests     = clientmetric.NewCounter("controlclient_map_requests")
	metricMapRequestsLite = clientmetric.NewCounter("controlclient_map_requests_lite")
	metricMapRequestsPoll = clientmetric.NewCounter("controlclient_map_requests_poll")

	metricMapResponseMessages   = clientmetric.NewCounter("controlclient_map_response_message") // any message type
	metricMapResponsePings      = clientmetric.NewCounter("controlclient_map_response_ping")
	metricMapResponseKeepAlives = clientmetric.NewCounter("controlclient_map_response_keepalive")
	metricMapResponseMap        = clientmetric.NewCounter("controlclient_map_response_map")       // any non-keepalive map response
	metricMapResponseMapDelta   = clientmetric.NewCounter("controlclient_map_response_map_delta") // 2nd+ non-keepalive map response

	metricSetDNS      = clientmetric.NewCounter("controlclient_setdns")
	metricSetDNSError = clientmetric.NewCounter("controlclient_setdns_error")
)
