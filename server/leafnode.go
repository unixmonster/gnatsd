// Copyright 2019 The NATS Authors
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package server

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/nats-io/nkeys"
)

type leaf struct {
	// Used to suppress sub and unsub interest. Same as routes but our audience
	// here is tied to this leaf node. This will hold all subscriptions except this
	// leaf nodes. This represents all the interest we want to send to the other side.
	smap map[string]int32
	// We have any auth stuff here for solicited connections.
	remote *RemoteLeafOpts
}

func (c *client) isSolicitedLeafNode() bool {
	return c.kind == LEAF && c.leaf.remote != nil
}

// This will spin up go routines to solicit the remote leaf node connections.
func (s *Server) solicitLeafNodeRemotes(remotes []*RemoteLeafOpts) {
	for _, r := range remotes {
		remote := r
		s.startGoRoutine(func() { s.connectToRemoteLeafNode(remote) })
	}
}

func (s *Server) remoteLeafNodeStillValid(remote *RemoteLeafOpts) bool {
	for _, ri := range s.getOpts().LeafNode.Remotes {
		// FIXME(dlc) - What about auth changes?
		if urlsAreEqual(ri.URL, remote.URL) {
			return true
		}
	}
	return false
}

// Ensure that gateway is properly configured.
func validateLeafNode(o *Options) error {
	if o.LeafNode.Port == 0 {
		return nil
	}
	if o.Gateway.Name == "" && o.Gateway.Port == 0 {
		return nil
	}
	// If we are here we have both leaf nodes and gateways defined, make sure there
	// is a system account defined.
	if o.SystemAccount == "" {
		return fmt.Errorf("leaf nodes and gateways (both being defined) require a system account to also be configured")
	}
	return nil
}

func (s *Server) reConnectToRemoteLeafNode(remote *RemoteLeafOpts) {
	// TODO(dlc) - We will use route delay semantics for now.
	delay := DEFAULT_ROUTE_RECONNECT
	select {
	case <-time.After(delay):
	case <-s.quitCh:
		s.grWG.Done()
		return
	}
	s.connectToRemoteLeafNode(remote)
}

func (s *Server) connectToRemoteLeafNode(remote *RemoteLeafOpts) {
	defer s.grWG.Done()

	if remote == nil || remote.URL == nil {
		s.Debugf("Empty remote leaf node definition, nothing to connect")
		return
	}

	for s.isRunning() && s.remoteLeafNodeStillValid(remote) {
		rURL := remote.URL
		s.Debugf("Trying to connect as leaf node to remote server on %s", rURL.Host)
		conn, err := net.DialTimeout("tcp", rURL.Host, DEFAULT_ROUTE_DIAL) // Use same timeouts as routes for now.
		if err != nil {
			s.Errorf("Error trying to connect as leaf node to remote server: %v", err)
			select {
			case <-s.quitCh:
				return
			case <-time.After(DEFAULT_ROUTE_CONNECT):
				continue
			}
		}
		if !s.remoteLeafNodeStillValid(remote) {
			conn.Close()
			return
		}

		// We have a connection here to a remote server.
		// Go ahead and create our leaf node and return.
		s.createLeafNode(conn, remote)
		return
	}
}

// This is the leafnode's accept loop. This runs as a go-routine.
// The listen specification is resolved (if use of random port),
// then a listener is started. After that, this routine enters
// a loop (until the server is shutdown) accepting incoming
// leaf node connections from remote servers.
func (s *Server) leafNodeAcceptLoop(ch chan struct{}) {
	defer func() {
		if ch != nil {
			close(ch)
		}
	}()

	// Snapshot server options.
	opts := s.getOpts()

	port := opts.LeafNode.Port
	if port == -1 {
		port = 0
	}

	hp := net.JoinHostPort(opts.LeafNode.Host, strconv.Itoa(port))
	l, e := net.Listen("tcp", hp)
	if e != nil {
		s.Fatalf("Error listening on leafnode port: %d - %v", opts.LeafNode.Port, e)
		return
	}

	s.Noticef("Listening for leafnode connections on %s",
		net.JoinHostPort(opts.LeafNode.Host, strconv.Itoa(l.Addr().(*net.TCPAddr).Port)))

	s.mu.Lock()
	tlsReq := opts.LeafNode.TLSConfig != nil
	tlsVerify := tlsReq && opts.LeafNode.TLSConfig.ClientAuth == tls.RequireAndVerifyClientCert
	info := Info{
		ID:           s.info.ID,
		Version:      s.info.Version,
		GitCommit:    gitCommit,
		GoVersion:    runtime.Version(),
		AuthRequired: true,
		TLSRequired:  tlsReq,
		TLSVerify:    tlsVerify,
		MaxPayload:   s.info.MaxPayload, // TODO(dlc) - Allow override?
		Proto:        1,                 // Fixed for now.
	}
	// If we have selected a random port...
	if port == 0 {
		// Write resolved port back to options.
		opts.LeafNode.Port = l.Addr().(*net.TCPAddr).Port
	}

	s.leafNodeInfo = info
	// Possibly override Host/Port and set IP based on Cluster.Advertise
	if err := s.setLeafNodeInfoHostPortAndIP(); err != nil {
		s.Fatalf("Error setting leafnode INFO with LeafNode.Advertise value of %s, err=%v", s.opts.LeafNode.Advertise, err)
		l.Close()
		s.mu.Unlock()
		return
	}

	// Generate the INFO JSON.
	// FIXME(dlc) - If we always generate no need for this.
	b, _ := json.Marshal(s.leafNodeInfo)
	pcs := [][]byte{[]byte("INFO"), b, []byte(CR_LF)}
	s.leafNodeInfoJSON = bytes.Join(pcs, []byte(" "))

	// Setup state that can enable shutdown
	s.leafNodeListener = l
	s.mu.Unlock()

	// Let them know we are up
	close(ch)
	ch = nil

	tmpDelay := ACCEPT_MIN_SLEEP

	for s.isRunning() {
		conn, err := l.Accept()
		if err != nil {
			tmpDelay = s.acceptError("LeafNode", err, tmpDelay)
			continue
		}
		tmpDelay = ACCEPT_MIN_SLEEP
		s.startGoRoutine(func() {
			s.createLeafNode(conn, nil)
			s.grWG.Done()
		})
	}
	s.Debugf("Leafnode accept loop exiting..")
	s.done <- true
}

// RegEx to match a creds file with user JWT and Seed.
var credsRe = regexp.MustCompile(`\s*(?:(?:[-]{3,}[^\n]*[-]{3,}\n)(.+)(?:\n\s*[-]{3,}[^\n]*[-]{3,}\n))`)

// Lock should be held entering here.
func (c *client) sendLeafConnect(tlsRequired bool) {
	// We support basic user/pass and operator based user JWT with signatures.
	cinfo := leafConnectInfo{
		TLS:  tlsRequired,
		Name: c.srv.info.ID,
	}

	// Check for credentials first, that will take precedence..
	if creds := c.leaf.remote.Credentials; creds != "" {
		c.Debugf("Authenticating with credentials file %q", c.leaf.remote.Credentials)
		contents, err := ioutil.ReadFile(creds)
		if err != nil {
			c.Errorf("%v", err)
			return
		}
		defer wipeSlice(contents)
		items := credsRe.FindAllSubmatch(contents, -1)
		if len(items) < 2 {
			c.Errorf("Credentials file malformed")
			return
		}
		// First result should be the user JWT.
		// We copy here so that the file containing the seed will be wiped appropriately.
		raw := items[0][1]
		tmp := make([]byte, len(raw))
		copy(tmp, raw)
		// Seed is second item.
		kp, err := nkeys.FromSeed(items[1][1])
		if err != nil {
			c.Errorf("Credentials file has malformed seed")
			return
		}
		// Wipe our key on exit.
		defer kp.Wipe()

		sigraw, _ := kp.Sign(c.nonce)
		sig := base64.RawURLEncoding.EncodeToString(sigraw)
		cinfo.JWT = string(tmp)
		cinfo.Sig = sig
	} else if userInfo := c.leaf.remote.URL.User; userInfo != nil {
		cinfo.User = userInfo.Username()
		pass, _ := userInfo.Password()
		cinfo.Pass = pass
	}

	b, err := json.Marshal(cinfo)
	if err != nil {
		c.Errorf("Error marshaling CONNECT to route: %v\n", err)
		c.closeConnection(ProtocolViolation)
		return
	}
	c.sendProto([]byte(fmt.Sprintf(ConProto, b)), true)
}

// Called when an inbound leafnode connection is accepted or we create one for a solicited leafnode.
func (s *Server) createLeafNode(conn net.Conn, remote *RemoteLeafOpts) *client {
	// Snapshot server options.
	opts := s.getOpts()

	maxPay := int32(opts.MaxPayload)
	maxSubs := int32(opts.MaxSubs)
	// For system, maxSubs of 0 means unlimited, so re-adjust here.
	if maxSubs == 0 {
		maxSubs = -1
	}
	now := time.Now()

	c := &client{srv: s, nc: conn, kind: LEAF, opts: defaultOpts, mpay: maxPay, msubs: maxSubs, start: now, last: now}
	c.leaf = &leaf{smap: map[string]int32{}}

	// Determines if we are soliciting the connection or not.
	var solicited bool

	if remote != nil {
		solicited = true
		// Users can bind to any local account, if its empty
		// we will assume the $G account.
		if remote.LocalAccount == "" {
			remote.LocalAccount = globalAccountName
		}
		// FIXME(dlc) - Make this resolve at startup.
		c.acc, _ = s.LookupAccount(remote.LocalAccount)
		// Make sure we register with the account here.
		c.registerWithAccount(c.acc)
		c.leaf.remote = remote
	}

	// Grab server variables
	s.mu.Lock()
	info := s.leafNodeInfo
	s.mu.Unlock()

	// Grab lock
	c.mu.Lock()

	c.initClient()

	c.Debugf("Leafnode connection created")

	if solicited {
		// We need to wait here for the info, but not for too long.
		c.nc.SetReadDeadline(time.Now().Add(DEFAULT_LEAFNODE_INFO_WAIT))
		br := bufio.NewReaderSize(c.nc, MAX_CONTROL_LINE_SIZE)
		info, err := br.ReadString('\n')
		if err != nil {
			c.mu.Unlock()
			if err == io.EOF {
				c.closeConnection(ClientClosed)
			} else {
				c.closeConnection(ReadError)
			}
			return nil
		}
		c.nc.SetReadDeadline(time.Time{})

		c.mu.Unlock()
		// Error will be handled below, so ignore here.
		c.parse([]byte(info))
		c.mu.Lock()

		if !c.flags.isSet(infoReceived) {
			c.mu.Unlock()
			c.Debugf("Did not get the remote leafnode's INFO, timed-out")
			c.closeConnection(ReadError)
			return nil
		}

		// Do TLS here as needed.
		tlsRequired := c.leaf.remote.TLS || c.leaf.remote.TLSConfig != nil
		if tlsRequired {
			c.Debugf("Starting TLS leafnode client handshake")
			// Specify the ServerName we are expecting.
			var tlsConfig *tls.Config
			if c.leaf.remote.TLSConfig != nil {
				tlsConfig = c.leaf.remote.TLSConfig.Clone()
			} else {
				tlsConfig = &tls.Config{MinVersion: tls.VersionTLS12}
			}

			host, _, _ := net.SplitHostPort(c.leaf.remote.URL.Host)
			tlsConfig.ServerName = host

			c.nc = tls.Client(c.nc, tlsConfig)

			conn := c.nc.(*tls.Conn)

			// Setup the timeout
			var wait time.Duration
			if c.leaf.remote.TLSTimeout == 0 {
				wait = TLS_TIMEOUT
			} else {
				wait = secondsToDuration(c.leaf.remote.TLSTimeout)
			}
			time.AfterFunc(wait, func() { tlsTimeout(c, conn) })
			conn.SetReadDeadline(time.Now().Add(wait))

			// Force handshake
			c.mu.Unlock()
			if err := conn.Handshake(); err != nil {
				c.Errorf("TLS handshake error: %v", err)
				c.closeConnection(TLSHandshakeError)
				return nil
			}
			// Reset the read deadline
			conn.SetReadDeadline(time.Time{})

			// Re-Grab lock
			c.mu.Lock()
		}

		c.sendLeafConnect(tlsRequired)
		c.Debugf("Remote leaf node connect msg sent")
	} else {
		// Send our info to the other side.
		// Remember the nonce we sent here for signatures, etc.
		c.nonce = make([]byte, nonceLen)
		s.generateNonce(c.nonce)
		info.Nonce = string(c.nonce)
		info.CID = c.cid
		b, _ := json.Marshal(info)
		pcs := [][]byte{[]byte("INFO"), b, []byte(CR_LF)}
		c.sendInfo(bytes.Join(pcs, []byte(" ")))

		// Check to see if we need to spin up TLS.
		if info.TLSRequired {
			c.Debugf("Starting TLS leafnode server handshake")
			c.nc = tls.Server(c.nc, opts.LeafNode.TLSConfig)
			conn := c.nc.(*tls.Conn)

			// Setup the timeout
			ttl := secondsToDuration(opts.LeafNode.TLSTimeout)
			time.AfterFunc(ttl, func() { tlsTimeout(c, conn) })
			conn.SetReadDeadline(time.Now().Add(ttl))

			// Force handshake
			c.mu.Unlock()
			if err := conn.Handshake(); err != nil {
				c.Errorf("TLS handshake error: %v", err)
				c.closeConnection(TLSHandshakeError)
				return nil
			}
			// Reset the read deadline
			conn.SetReadDeadline(time.Time{})

			// Re-Grab lock
			c.mu.Lock()

			// Indicate that handshake is complete (used in monitoring)
			c.flags.set(handshakeComplete)
		}

		// Leaf nodes will always require a CONNECT to let us know
		// when we are properly bound to an account.
		// The connection may have been closed
		if c.nc != nil {
			c.setAuthTimer(secondsToDuration(opts.LeafNode.AuthTimeout))
		}
	}

	// Spin up the read loop.
	s.startGoRoutine(c.readLoop)

	// Spin up the write loop.
	s.startGoRoutine(c.writeLoop)

	// Set the Ping timer
	c.setPingTimer()

	c.mu.Unlock()

	// Update server's accounting here if we solicited.
	if solicited {
		s.addLeafNodeConnection(c)
	}

	return c
}

func (c *client) processLeafnodeInfo(info *Info) {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.leaf == nil || c.nc == nil {
		return
	}

	// Mark that the INFO protocol has been received.
	// Capture a nonce here.
	c.flags.set(infoReceived)
	c.nonce = []byte(info.Nonce)
	if info.TLSRequired && c.leaf.remote != nil {
		c.leaf.remote.TLS = true
	}
}

// Similar to setInfoHostPortAndGenerateJSON, but for leafNodeInfo.
func (s *Server) setLeafNodeInfoHostPortAndIP() error {
	if s.opts.LeafNode.Advertise != "" {
		advHost, advPort, err := parseHostPort(s.opts.LeafNode.Advertise, s.opts.LeafNode.Port)
		if err != nil {
			return err
		}
		s.leafNodeInfo.Host = advHost
		s.leafNodeInfo.Port = advPort
		s.leafNodeInfo.IP = fmt.Sprintf("nats-leafnode://%s/", net.JoinHostPort(advHost, strconv.Itoa(advPort)))
	} else {
		s.leafNodeInfo.Host = s.opts.LeafNode.Host
		s.leafNodeInfo.Port = s.opts.LeafNode.Port
		s.leafNodeInfo.IP = ""
	}
	return nil
}

func (s *Server) addLeafNodeConnection(c *client) {
	c.mu.Lock()
	cid := c.cid
	c.mu.Unlock()
	s.mu.Lock()
	s.leafs[cid] = c
	s.mu.Unlock()
}

func (s *Server) removeLeafNodeConnection(c *client) {
	c.mu.Lock()
	cid := c.cid
	c.mu.Unlock()
	s.mu.Lock()
	delete(s.leafs, cid)
	s.mu.Unlock()
}

type leafConnectInfo struct {
	JWT  string `json:"jwt,omitempty"`
	Sig  string `json:"sig,omitempty"`
	User string `json:"user,omitempty"`
	Pass string `json:"pass,omitempty"`
	TLS  bool   `json:"tls_required"`
	Comp bool   `json:"compression,omitempty"`
	Name string `json:"name,omitempty"`

	// Just used to detect wrong connection attempts.
	Gateway string `json:"gateway,omitempty"`
}

// processLeafNodeConnect will process the inbound connect args.
// Once we are here we are bound to an account, so can send any interest that
// we would have to the other side.
func (c *client) processLeafNodeConnect(s *Server, arg []byte, lang string) error {
	// Way to detect clients that incorrectly connect to the route listen
	// port. Client provided "lang" in the CONNECT protocol while LEAFNODEs don't.

	if lang != "" {
		c.sendErrAndErr(ErrClientConnectedToLeafNodePort.Error())
		c.closeConnection(WrongPort)
		return ErrClientConnectedToLeafNodePort
	}

	// Unmarshal as a leaf node connect protocol
	proto := &leafConnectInfo{}
	if err := json.Unmarshal(arg, proto); err != nil {
		return err
	}

	// Reject if this has Gateway which means that it would be from a gateway
	// connection that incorrectly connects to the leafnode port.
	if proto.Gateway != "" {
		errTxt := fmt.Sprintf("Rejecting connection from gateway %q on the leafnode port", proto.Gateway)
		c.Errorf(errTxt)
		c.sendErr(errTxt)
		c.closeConnection(WrongGateway)
		return ErrWrongGateway
	}

	// Leaf Nodes do not do echo or verbose or pedantic.
	c.opts.Verbose = false
	c.opts.Echo = false
	c.opts.Pedantic = false

	// Create and initialize the smap since we know our bound account now.
	c.initLeafNodeSmap()

	// We are good to go, send over all the bound account subscriptions.
	s.startGoRoutine(func() {
		c.sendAllAccountSubs()
		s.grWG.Done()
	})

	// Add in the leafnode here since we passed through auth at this point.
	s.addLeafNodeConnection(c)

	// Announce the account connect event for a leaf node.
	// This will no-op as needed.
	s.sendLeafNodeConnect(c.acc)

	return nil
}

// Snapshot the current subscriptions from the sublist into our smap which
// we will keep updated from now on.
func (c *client) initLeafNodeSmap() {
	acc := c.acc
	if acc == nil {
		c.Debugf("Leaf node does not have an account bound")
		return
	}
	// Collect all subs here.
	_subs := [256]*subscription{}
	subs := _subs[:0]
	ims := []string{}
	acc.mu.RLock()
	acc.sl.All(&subs)
	// Since leaf nodes only send on interest, if the bound
	// account has import services we need to send those over.
	for isubj := range acc.imports.services {
		ims = append(ims, isubj)
	}
	acc.mu.RUnlock()

	// Now walk the results and add them to our smap
	c.mu.Lock()
	for _, sub := range subs {
		// We ignore ourselves here.
		if c != sub.client {
			c.leaf.smap[keyFromSub(sub)]++
		}
	}
	// FIXME(dlc) - We need to update appropriately on an account claims update.
	for _, isubj := range ims {
		c.leaf.smap[isubj]++
	}
	c.mu.Unlock()
}

// updateInterestForAccountOnGateway called from gateway code when processing RS+ and RS-.
func (s *Server) updateInterestForAccountOnGateway(accName string, sub *subscription, delta int32) {
	acc, err := s.LookupAccount(accName)
	if acc == nil || err != nil {
		s.Debugf("No or bad account for %q, failed to update interest from gateway", accName)
		return
	}
	s.updateLeafNodes(acc, sub, delta)
}

// updateLeafNodes will make sure to update the smap for the subscription. Will
// also forward to all leaf nodes as needed.
func (s *Server) updateLeafNodes(acc *Account, sub *subscription, delta int32) {
	if acc == nil || sub == nil {
		return
	}

	_l := [256]*client{}
	leafs := _l[:0]

	// Grab all leaf nodes. Ignore leafnode if sub's client is a leafnode and matches.
	acc.mu.RLock()
	for _, ln := range acc.clients {
		if ln.kind == LEAF && ln != sub.client {
			leafs = append(leafs, ln)
		}
	}
	acc.mu.RUnlock()

	for _, ln := range leafs {
		ln.updateSmap(sub, delta)
	}
}

// This will make an update to our internal smap and determine if we should send out
// and interest update to the remote side.
func (c *client) updateSmap(sub *subscription, delta int32) {
	key := keyFromSub(sub)

	c.mu.Lock()
	n := c.leaf.smap[key]
	// We will update if its a queue, if count is zero (or negative), or we were 0 and are N > 0.
	update := sub.queue != nil || n == 0 || n+delta <= 0
	n += delta
	if n > 0 {
		c.leaf.smap[key] = n
	} else {
		delete(c.leaf.smap, key)
	}
	if update {
		c.sendLeafNodeSubUpdate(key, n)
	}
	c.mu.Unlock()
}

// Send the subscription interest change to the other side.
// Lock should be held.
func (c *client) sendLeafNodeSubUpdate(key string, n int32) {
	_b := [64]byte{}
	b := bytes.NewBuffer(_b[:0])
	writeLeafSub(b, key, n)
	c.sendProto(b.Bytes(), false)
}

// Helper function to build the key.
func keyFromSub(sub *subscription) string {
	var _rkey [1024]byte
	var key []byte

	if sub.queue != nil {
		// Just make the key subject spc group, e.g. 'foo bar'
		key = _rkey[:0]
		key = append(key, sub.subject...)
		key = append(key, byte(' '))
		key = append(key, sub.queue...)
	} else {
		key = sub.subject
	}
	return string(key)
}

// Send all subscriptions for this account that include local
// and all subscriptions besides our own.
func (c *client) sendAllAccountSubs() {
	c.mu.Lock()
	defer c.mu.Unlock()

	// Hold all at once for now.
	var b bytes.Buffer

	for key, n := range c.leaf.smap {
		writeLeafSub(&b, key, n)
	}

	// We will make sure we don't overflow here due to an max_pending.
	chunks := protoChunks(b.Bytes(), MAX_PAYLOAD_SIZE)

	for _, chunk := range chunks {
		c.queueOutbound(chunk)
		c.flushOutbound()
	}
}

func writeLeafSub(w *bytes.Buffer, key string, n int32) {
	if key == "" {
		return
	}
	if n > 0 {
		w.WriteString("LS+ " + key)
		// Check for queue semantics, if found write n.
		if strings.Contains(key, " ") {
			w.WriteString(" ")
			var b [12]byte
			var i = len(b)
			for l := n; l > 0; l /= 10 {
				i--
				b[i] = digits[l%10]
			}
			w.Write(b[i:])
		}
	} else {
		w.WriteString("LS- " + key)
	}
	w.WriteString(CR_LF)
}

// processLeafSub will process an inbound sub request for the remote leaf node.
func (c *client) processLeafSub(argo []byte) (err error) {
	c.traceInOp("LS+", argo)

	// Indicate activity.
	c.in.subs++

	srv := c.srv
	if srv == nil {
		return nil
	}

	// Copy so we do not reference a potentially large buffer
	arg := make([]byte, len(argo))
	copy(arg, argo)

	args := splitArg(arg)
	sub := &subscription{client: c}

	switch len(args) {
	case 1:
		sub.queue = nil
	case 3:
		sub.queue = args[1]
		sub.qw = int32(parseSize(args[2]))
	default:
		return fmt.Errorf("processLeafSub Parse Error: '%s'", arg)
	}
	sub.subject = args[0]

	c.mu.Lock()
	if c.nc == nil {
		c.mu.Unlock()
		return nil
	}

	// Check permissions if applicable.
	if !c.canExport(string(sub.subject)) {
		c.mu.Unlock()
		c.Debugf("Can not export %q, ignoring remote subscription request", sub.subject)
		return nil
	}

	// Check if we have a maximum on the number of subscriptions.
	if c.subsAtLimit() {
		c.mu.Unlock()
		c.maxSubsExceeded()
		return nil
	}

	// Like Routes, we store local subs by account and subject and optionally queue name.
	// If we have a queue it will have a trailing weight which we do not want.
	if sub.queue != nil {
		sub.sid = arg[:len(arg)-len(args[2])-1]
	} else {
		sub.sid = arg
	}
	acc := c.acc
	key := string(sub.sid)
	osub := c.subs[key]
	updateGWs := false
	if osub == nil {
		c.subs[key] = sub
		// Now place into the account sl.
		if err = acc.sl.Insert(sub); err != nil {
			delete(c.subs, key)
			c.mu.Unlock()
			c.Errorf("Could not insert subscription: %v", err)
			c.sendErr("Invalid Subscription")
			return nil
		}
		updateGWs = srv.gateway.enabled
	} else if sub.queue != nil {
		// For a queue we need to update the weight.
		atomic.StoreInt32(&osub.qw, sub.qw)
		acc.sl.UpdateRemoteQSub(osub)
	}

	c.mu.Unlock()

	// Treat leaf node subscriptions similar to a client subscription, meaning we
	// send them to both routes and gateways and other leaf nodes. We also do
	// the shadow subscriptions.
	if err := c.addShadowSubscriptions(acc, sub); err != nil {
		c.Errorf(err.Error())
	}
	// If we are routing add to the route map for the associated account.
	srv.updateRouteSubscriptionMap(acc, sub, 1)
	if updateGWs {
		srv.gatewayUpdateSubInterest(acc.Name, sub, 1)
	}
	// Now check on leafnode updates for other leaf nodes.
	srv.updateLeafNodes(acc, sub, 1)

	return nil
}

// processLeafUnsub will process an inbound unsub request for the remote leaf node.
func (c *client) processLeafUnsub(arg []byte) error {
	c.traceInOp("LS-", arg)

	// Indicate any activity, so pub and sub or unsubs.
	c.in.subs++

	acc := c.acc
	srv := c.srv

	c.mu.Lock()
	if c.nc == nil {
		c.mu.Unlock()
		return nil
	}

	updateGWs := false
	// We store local subs by account and subject and optionally queue name.
	// LS- will have the arg exactly as the key.
	sub, ok := c.subs[string(arg)]
	c.mu.Unlock()

	if ok {
		c.unsubscribe(acc, sub, true)
		updateGWs = srv.gateway.enabled
	}

	// Treat leaf node subscriptions similar to a client subscription, meaning we
	// send them to both routes and gateways and other leaf nodes. We also do
	// the shadow subscriptions.
	if err := c.addShadowSubscriptions(acc, sub); err != nil {
		c.Errorf(err.Error())
	}
	// If we are routing add to the route map for the associated account.
	srv.updateRouteSubscriptionMap(acc, sub, -1)
	// Gateways
	if updateGWs {
		srv.gatewayUpdateSubInterest(acc.Name, sub, -1)
	}
	// Now check on leafnode updates for other leaf nodes.
	srv.updateLeafNodes(acc, sub, -1)
	return nil
}

func (c *client) processLeafMsgArgs(trace bool, arg []byte) error {
	if trace {
		c.traceInOp("LMSG", arg)
	}

	// Unroll splitArgs to avoid runtime/heap issues
	a := [MAX_MSG_ARGS][]byte{}
	args := a[:0]
	start := -1
	for i, b := range arg {
		switch b {
		case ' ', '\t', '\r', '\n':
			if start >= 0 {
				args = append(args, arg[start:i])
				start = -1
			}
		default:
			if start < 0 {
				start = i
			}
		}
	}
	if start >= 0 {
		args = append(args, arg[start:])
	}

	c.pa.arg = arg
	switch len(args) {
	case 0, 1:
		return fmt.Errorf("processLeafMsgArgs Parse Error: '%s'", args)
	case 2:
		c.pa.reply = nil
		c.pa.queues = nil
		c.pa.szb = args[1]
		c.pa.size = parseSize(args[1])
	case 3:
		c.pa.reply = args[1]
		c.pa.queues = nil
		c.pa.szb = args[2]
		c.pa.size = parseSize(args[2])
	default:
		// args[1] is our reply indicator. Should be + or | normally.
		if len(args[1]) != 1 {
			return fmt.Errorf("processLeafMsgArgs Bad or Missing Reply Indicator: '%s'", args[1])
		}
		switch args[1][0] {
		case '+':
			c.pa.reply = args[2]
		case '|':
			c.pa.reply = nil
		default:
			return fmt.Errorf("processLeafMsgArgs Bad or Missing Reply Indicator: '%s'", args[1])
		}
		// Grab size.
		c.pa.szb = args[len(args)-1]
		c.pa.size = parseSize(c.pa.szb)

		// Grab queue names.
		if c.pa.reply != nil {
			c.pa.queues = args[3 : len(args)-1]
		} else {
			c.pa.queues = args[2 : len(args)-1]
		}
	}
	if c.pa.size < 0 {
		return fmt.Errorf("processRoutedMsgArgs Bad or Missing Size: '%s'", args)
	}

	// Common ones processed after check for arg length
	c.pa.subject = args[0]

	return nil
}

// processInboundLeafMsg is called to process an inbound msg from a leaf node.
func (c *client) processInboundLeafMsg(msg []byte) {
	// Update statistics
	c.in.msgs++
	// The msg includes the CR_LF, so pull back out for accounting.
	c.in.bytes += int32(len(msg) - LEN_CR_LF)

	if c.trace {
		c.traceMsg(msg)
	}

	// Check pub permissions
	if c.perms != nil && (c.perms.pub.allow != nil || c.perms.pub.deny != nil) && !c.pubAllowed(string(c.pa.subject)) {
		c.pubPermissionViolation(c.pa.subject)
		return
	}

	srv := c.srv
	acc := c.acc

	// Mostly under testing scenarios.
	if srv == nil || acc == nil {
		return
	}

	// Match the subscriptions. We will use our own L1 map if
	// it's still valid, avoiding contention on the shared sublist.
	var r *SublistResult
	var ok bool

	genid := atomic.LoadUint64(&c.acc.sl.genid)
	if genid == c.in.genid && c.in.results != nil {
		r, ok = c.in.results[string(c.pa.subject)]
	} else {
		// Reset our L1 completely.
		c.in.results = make(map[string]*SublistResult)
		c.in.genid = genid
	}

	// Go back to the sublist data structure.
	if !ok {
		r = c.acc.sl.Match(string(c.pa.subject))
		c.in.results[string(c.pa.subject)] = r
		// Prune the results cache. Keeps us from unbounded growth. Random delete.
		if len(c.in.results) > maxResultCacheSize {
			n := 0
			for subject := range c.in.results {
				delete(c.in.results, subject)
				if n++; n > pruneSize {
					break
				}
			}
		}
	}

	// Check to see if we need to map/route to another account.
	if acc.imports.services != nil {
		c.checkForImportServices(acc, msg)
	}

	// Check to see if we have a routed message with a service reply.
	if isServiceReply(c.pa.reply) && acc != nil {
		// TODO(dlc) - Figure out what this means.
		panic("Not Implemented")
	}

	var qnames [][]byte

	// Check for no interest, short circuit if so.
	// This is the fanout scale.
	if len(r.psubs)+len(r.qsubs) > 0 {
		var collect bool
		// If we have queue subs in this cluster, then if we run in gateway
		// mode and the remote gateways have queue subs, then we need to
		// collect the queue groups this message was sent to so that we
		// exclude them when sending to gateways.
		if len(r.qsubs) > 0 && c.srv.gateway.enabled &&
			atomic.LoadInt64(&c.srv.gateway.totalQSubs) > 0 {
			collect = true
		}
		qnames = c.processMsgResults(acc, r, msg, c.pa.subject, c.pa.reply, collect)
	}

	// Now deal with gateways
	if c.srv.gateway.enabled {
		c.sendMsgToGateways(c.acc, msg, c.pa.subject, c.pa.reply, qnames)
	}
}

// This functional will take a larger buffer and break it into
// chunks that are protocol correct. Reason being is that we are
// doing this in the first place to get things in smaller sizes
// out the door but we may allow someone to get in between us as
// we do.
// NOTE - currently this does not process MSG protos.
func protoChunks(b []byte, csz int) [][]byte {
	if b == nil {
		return nil
	}
	if len(b) <= csz {
		return [][]byte{b}
	}
	var (
		chunks [][]byte
		start  int
	)
	for i := csz; i < len(b); {
		// Walk forward to find a CR_LF
		delim := bytes.Index(b[i:], []byte(CR_LF))
		if delim < 0 {
			chunks = append(chunks, b[start:])
			break
		}
		end := delim + LEN_CR_LF + i
		chunks = append(chunks, b[start:end])
		start = end
		i = end + csz
	}
	return chunks
}
