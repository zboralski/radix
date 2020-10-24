package radix

import (
	"context"
	"errors"
	"fmt"
	"net"
	"sync/atomic"
	"time"

	"github.com/mediocregopher/radix/v4/internal/proc"
	"github.com/mediocregopher/radix/v4/trace"
)

// wrapDefaultConnFunc is used to ensure that redis url options are
// automatically applied to future sentinel connections whose address doesn't
// have that information encoded.
func wrapDefaultConnFunc(addr string) ConnFunc {
	_, opts := parseRedisURL(addr)
	return func(ctx context.Context, network, addr string) (Conn, error) {
		return Dial(ctx, network, addr, opts...)
	}
}

type sentinelOpts struct {
	cf    ConnFunc
	pf    ClientFunc
	st    trace.SentinelTrace
	errCh chan<- error
}

// SentinelOpt is an optional behavior which can be applied to the NewSentinel
// function to effect a Sentinel's behavior.
type SentinelOpt func(*sentinelOpts)

// SentinelConnFunc tells the Sentinel to use the given ConnFunc when connecting
// to sentinel instances.
//
// NOTE that if SentinelConnFunc is not used then Sentinel will attempt to
// retrieve AUTH and SELECT information from the address provided to
// NewSentinel, and use that for dialing all Sentinels. If SentinelConnFunc is
// provided, however, those options must be given through
// DialAuthPass/DialSelectDB within the ConnFunc.
func SentinelConnFunc(cf ConnFunc) SentinelOpt {
	return func(so *sentinelOpts) {
		so.cf = cf
	}
}

// SentinelPoolFunc tells the Sentinel to use the given ClientFunc when creating
// a pool of connections to the sentinel's primary.
func SentinelPoolFunc(pf ClientFunc) SentinelOpt {
	return func(so *sentinelOpts) {
		so.pf = pf
	}
}

// SentinelErrCh takes a channel which asynchronous errors encountered by the
// Sentinel can be read off of. If the channel blocks the error will be dropped.
// The channel will be closed when the Sentinel is closed.
func SentinelErrCh(errCh chan<- error) SentinelOpt {
	return func(so *sentinelOpts) {
		so.errCh = errCh
	}
}

// SentinelWithTrace tells the Sentinel to trace itself with the given
// SentinelTrace. Note that SentinelTrace will block at every point which is set
// to trace.
func SentinelWithTrace(st trace.SentinelTrace) SentinelOpt {
	return func(so *sentinelOpts) {
		so.st = st
	}
}

// Sentinel is a Client which, in the background, connects to an available
// sentinel node and handles all of the following:
//
// * Creates a pool to the current primary instance, as advertised by the
// sentinel
//
// * Listens for events indicating the primary has changed, and automatically
// creates a new Client to the new primary
//
// * Keeps track of other sentinels in the cluster, and uses them if the
// currently connected one becomes unreachable
//
type Sentinel struct {
	proc      *proc.Proc
	opts      sentinelOpts
	initAddrs []string
	name      string

	// these fields are protected by proc's lock
	primAddr      string
	clients       map[string]Client
	sentinelAddrs map[string]bool // the known sentinel addresses

	// We use a persistent PubSubConn here, so we don't need to do much after
	// initialization. The pconn is only really kept around for closing
	pconn   PubSubConn
	pconnCh chan PubSubMessage

	// only used by tests to ensure certain actions have happened before
	// continuing on during the test
	testEventCh chan string

	// only used by tests to delay updates after event on pconnCh
	// contains time in milliseconds
	testSleepBeforeSwitch uint32
}

var _ MultiClient = new(Sentinel)

// NewSentinel creates and returns a *Sentinel instance. NewSentinel takes in a
// number of options which can overwrite its default behavior. The default
// options NewSentinel uses are:
//
//	SentinelConnFunc(DefaultConnFunc)
//	SentinelPoolFunc(DefaultClientFunc)
//
func NewSentinel(ctx context.Context, primaryName string, sentinelAddrs []string, opts ...SentinelOpt) (*Sentinel, error) {
	addrs := map[string]bool{}
	for _, addr := range sentinelAddrs {
		addrs[addr] = true
	}

	sc := &Sentinel{
		proc:          proc.New(),
		initAddrs:     sentinelAddrs,
		name:          primaryName,
		clients:       map[string]Client{},
		sentinelAddrs: addrs,
		pconnCh:       make(chan PubSubMessage, 1),
		testEventCh:   make(chan string, 1),
	}

	// If the given sentinelAddrs have AUTH/SELECT info encoded into them then
	// use that for all sentinel connections going forward (unless overwritten
	// by a SentinelConnFunc in opts).
	sc.opts.cf = wrapDefaultConnFunc(sentinelAddrs[0])
	defaultSentinelOpts := []SentinelOpt{
		SentinelPoolFunc(DefaultClientFunc),
	}

	for _, opt := range append(defaultSentinelOpts, opts...) {
		// the other args to NewSentinel used to be a ConnFunc and a ClientFunc,
		// which someone might have left as nil, in which case this now gives a
		// weird panic. Just handle it
		if opt != nil {
			opt(&(sc.opts))
		}
	}

	// first thing is to retrieve the state and create a pool using the first
	// connectable connection. This connection is only used during
	// initialization, it gets closed right after
	{
		conn, err := sc.dialSentinel(ctx)
		if err != nil {
			return nil, err
		}
		defer conn.Close()

		if err := sc.ensureSentinelAddrs(ctx, conn); err != nil {
			return nil, err
		} else if err := sc.ensureClients(ctx, conn); err != nil {
			return nil, err
		}
	}

	// because we're using persistent these can't _really_ fail
	var err error
	sc.pconn, err = NewPersistentPubSubConn(ctx, "", "", PersistentPubSubConnFunc(func(ctx context.Context, _, _ string) (Conn, error) {
		return sc.dialSentinel(ctx)
	}))
	if err != nil {
		sc.Close()
		return nil, err
	}

	sc.pconn.Subscribe(ctx, sc.pconnCh, "switch-master")
	sc.proc.Run(sc.spin)
	return sc, nil
}

func (sc *Sentinel) err(err error) {
	select {
	case sc.opts.errCh <- err:
	default:
	}
}

func (sc *Sentinel) testEvent(event string) {
	select {
	case sc.testEventCh <- event:
	default:
	}
}

func (sc *Sentinel) dialSentinel(ctx context.Context) (conn Conn, err error) {
	err = sc.proc.WithRLock(func() error {
		for addr := range sc.sentinelAddrs {
			if conn, err = sc.opts.cf(ctx, "tcp", addr); err == nil {
				return nil
			}
		}

		// try the initAddrs as a last ditch, but don't return their error if
		// this doesn't work
		for _, addr := range sc.initAddrs {
			var initErr error
			if conn, initErr = sc.opts.cf(ctx, "tcp", addr); initErr == nil {
				return nil
			}
		}
		return err
	})
	return
}

// Do implements the method for the Client interface. It will perform the given
// Action on the current primary.
func (sc *Sentinel) Do(ctx context.Context, a Action) error {
	return sc.proc.WithRLock(func() error {
		return sc.clients[sc.primAddr].Do(ctx, a)
	})
}

// DoSecondary implements the method for the Client interface. It will perform
// the given Action on a random secondary, or the primary if no secondary is
// available.
//
// For DoSecondary to work, replicas must be configured with replica-read-only
// enabled, otherwise calls to DoSecondary may by rejected by the replica.
func (sc *Sentinel) DoSecondary(ctx context.Context, a Action) error {
	c, err := sc.client(ctx, "")
	if err != nil {
		return err
	}
	return c.Do(ctx, a)
}

// Clients implements the method for the MultiClient interface. The returned map
// will only ever have one key/value pair.
func (sc *Sentinel) Clients() (map[string]ReplicaSet, error) {
	m := map[string]ReplicaSet{}
	err := sc.proc.WithRLock(func() error {
		var rs ReplicaSet
		for addr, client := range sc.clients {
			if addr == sc.primAddr {
				rs.Primary = client
			} else {
				rs.Secondaries = append(rs.Secondaries, client)
			}
		}
		m[sc.primAddr] = rs
		return nil
	})
	return m, err
}

// SentinelAddrs returns the addresses of all known sentinels.
func (sc *Sentinel) SentinelAddrs() ([]string, error) {
	var sentAddrs []string
	err := sc.proc.WithRLock(func() error {
		sentAddrs = make([]string, 0, len(sc.sentinelAddrs))
		for addr := range sc.sentinelAddrs {
			sentAddrs = append(sentAddrs, addr)
		}
		return nil
	})
	return sentAddrs, err
}

func (sc *Sentinel) client(ctx context.Context, addr string) (Client, error) {
	var client Client
	err := sc.proc.WithRLock(func() error {
		if addr == "" {
			for addr, client = range sc.clients {
				if addr != sc.primAddr {
					break
				}
			}
		}
		if client == nil {
			client = sc.clients[sc.primAddr]
		}
		return nil
	})
	if err != nil {
		return nil, err
	} else if client != nil {
		return client, nil
	} else if addr == "" {
		return nil, errors.New("no Clients available")
	}

	// if client was nil but ok was true it means the address is a secondary but
	// a Client for it has never been created. Create one now and store it into
	// clients.
	newClient, err := sc.opts.pf(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}

	// two routines might be requesting the same addr at the same time, and
	// both create the client. The second one needs to make sure it closes its
	// own pool when it sees the other got there first.
	err = sc.proc.WithLock(func() error {
		if client = sc.clients[addr]; client == nil {
			sc.clients[addr] = newClient
		}
		return nil
	})

	if client != nil || err != nil {
		newClient.Close()
		return client, err
	}

	return newClient, nil
}

// Close implements the method for the Client interface.
func (sc *Sentinel) Close() error {
	return sc.proc.Close(func() error {
		for _, client := range sc.clients {
			if client != nil {
				client.Close()
			}
		}
		if sc.opts.errCh != nil {
			close(sc.opts.errCh)
		}
		return nil
	})
}

// cmd should be the command called which generated m
func sentinelMtoAddr(m map[string]string, cmd string) (string, error) {
	if m["ip"] == "" || m["port"] == "" {
		return "", fmt.Errorf("malformed %q response: %#v", cmd, m)
	}
	return net.JoinHostPort(m["ip"], m["port"]), nil
}

// given a connection to a sentinel, ensures that the Clients currently being
// held agrees with what the sentinel thinks they should be
func (sc *Sentinel) ensureClients(ctx context.Context, conn Conn) error {
	var primM map[string]string
	var secMM []map[string]string
	p := NewPipeline()
	p.Append(Cmd(&primM, "SENTINEL", "MASTER", sc.name))
	p.Append(Cmd(&secMM, "SENTINEL", "SLAVES", sc.name))
	if err := conn.Do(ctx, p); err != nil {
		return err
	}

	newPrimAddr, err := sentinelMtoAddr(primM, "SENTINEL MASTER")
	if err != nil {
		return err
	}

	newClients := map[string]Client{newPrimAddr: nil}
	for _, secM := range secMM {
		newSecAddr, err := sentinelMtoAddr(secM, "SENTINEL SLAVES")
		if err != nil {
			return err
		}
		newClients[newSecAddr] = nil
	}

	// ensure all current clients exist
	newTraceNodes := map[string]trace.SentinelNodeInfo{}
	for addr := range newClients {
		client, err := sc.client(ctx, addr)
		if err != nil {
			return fmt.Errorf("error creating client for %q: %w", addr, err)
		}
		newClients[addr] = client
		newTraceNodes[addr] = trace.SentinelNodeInfo{
			Addr:      addr,
			IsPrimary: addr == newPrimAddr,
		}
	}

	var toClose []Client
	prevTraceNodes := map[string]trace.SentinelNodeInfo{}
	err = sc.proc.WithLock(func() error {

		// for each actual Client instance in sc.client, either move it over to
		// newClients (if the address is shared) or make sure it is closed
		for addr, client := range sc.clients {
			prevTraceNodes[addr] = trace.SentinelNodeInfo{
				Addr:      addr,
				IsPrimary: addr == sc.primAddr,
			}

			if _, ok := newClients[addr]; ok {
				newClients[addr] = client
			} else {
				toClose = append(toClose, client)
			}
		}

		sc.primAddr = newPrimAddr
		sc.clients = newClients

		return nil
	})
	if err != nil {
		return err
	}

	for _, client := range toClose {
		client.Close()
	}
	sc.traceTopoChanged(prevTraceNodes, newTraceNodes)
	return nil
}

func (sc *Sentinel) traceTopoChanged(prevTopo, newTopo map[string]trace.SentinelNodeInfo) {
	if sc.opts.st.TopoChanged == nil {
		return
	}

	var added, removed, changed []trace.SentinelNodeInfo
	for addr, prevNodeInfo := range prevTopo {
		if newNodeInfo, ok := newTopo[addr]; !ok {
			removed = append(removed, prevNodeInfo)
		} else if newNodeInfo != prevNodeInfo {
			changed = append(changed, newNodeInfo)
		}
	}
	for addr, newNodeInfo := range newTopo {
		if _, ok := prevTopo[addr]; !ok {
			added = append(added, newNodeInfo)
		}
	}

	if len(added)+len(removed)+len(changed) == 0 {
		return
	}
	sc.opts.st.TopoChanged(trace.SentinelTopoChanged{
		Added:   added,
		Removed: removed,
		Changed: changed,
	})
}

// annoyingly the SENTINEL SENTINELS <name> command doesn't return _this_
// sentinel instance, only the others it knows about for that primary
func (sc *Sentinel) ensureSentinelAddrs(ctx context.Context, conn Conn) error {
	var mm []map[string]string
	err := conn.Do(ctx, Cmd(&mm, "SENTINEL", "SENTINELS", sc.name))
	if err != nil {
		return err
	}

	addrs := map[string]bool{conn.Addr().String(): true}
	for _, m := range mm {
		addrs[net.JoinHostPort(m["ip"], m["port"])] = true
	}

	return sc.proc.WithLock(func() error {
		sc.sentinelAddrs = addrs
		return nil
	})
}

func (sc *Sentinel) spin(ctx context.Context) {
	defer sc.pconn.Close()
	for {
		err := sc.innerSpin(ctx)

		// This also gets checked within innerSpin to short-circuit that, but
		// we also must check in here to short-circuit this. The error returned
		// doesn't really matter if the whole Sentinel is closing.
		select {
		case <-ctx.Done():
			return
		default:
		}

		if err != nil {
			sc.err(err)
			// sleep a second so we don't end up in a tight loop
			time.Sleep(1 * time.Second)
		}
	}
}

// makes connection to an address in sc.addrs and handles
// the sentinel until that connection goes bad.
//
// Things this handles:
// * Listening for switch-master events (from pconn, which has reconnect logic
//   external to this package)
// * Periodically re-ensuring that the list of sentinel addresses is up-to-date
// * Periodically re-checking the current primary, in case the switch-master was
//   missed somehow
func (sc *Sentinel) innerSpin(ctx context.Context) error {
	conn, err := sc.dialSentinel(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()

	tick := time.NewTicker(5 * time.Second)
	defer tick.Stop()

	var switchMaster bool
	for {
		err := func() error {
			// putting this in an anonymous function is only slightly less ugly
			// than calling cancel in every if-error case.
			ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			defer cancel()
			if err := sc.ensureSentinelAddrs(ctx, conn); err != nil {
				return fmt.Errorf("retrieving addresses of sentinel instances: %w", err)
			} else if err := sc.ensureClients(ctx, conn); err != nil {
				return fmt.Errorf("creating clients based on sentinel addresses: %w", err)
			} else if err := sc.pconn.Ping(ctx); err != nil {
				return fmt.Errorf("calling PING on sentinel instance: %w", err)
			}
			return nil
		}()
		if err != nil {
			return err
		}

		// the tests want to know when the client state has been updated due to
		// a switch-master event
		if switchMaster {
			sc.testEvent("switch-master completed")
			switchMaster = false
		}

		select {
		case <-tick.C:
			// loop
		case <-sc.pconnCh:
			switchMaster = true
			if waitFor := atomic.SwapUint32(&sc.testSleepBeforeSwitch, 0); waitFor > 0 {
				time.Sleep(time.Duration(waitFor) * time.Millisecond)
			}
			// loop
		case <-ctx.Done():
			return nil
		}
	}
}

func (sc *Sentinel) forceMasterSwitch(waitFor time.Duration) {
	// can not use waitFor.Milliseconds() here since it was only introduced in Go 1.13 and we still support 1.12
	atomic.StoreUint32(&sc.testSleepBeforeSwitch, uint32(waitFor.Nanoseconds()/1e6))
	sc.pconnCh <- PubSubMessage{}
}
