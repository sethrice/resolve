// Copyright © by Jeff Foley 2017-2022. All rights reserved.
// Use of this source code is governed by Apache 2 LICENSE that can be found in the LICENSE file.
// SPDX-License-Identifier: Apache-2.0

package resolve

import (
	"context"
	"errors"
	"io/ioutil"
	"log"
	"net"
	"sync"
	"time"

	"github.com/caffix/queue"
	"github.com/miekg/dns"
	"go.uber.org/ratelimit"
)

// Resolvers is a pool of DNS resolvers managed for brute forcing using random selection.
type Resolvers struct {
	sync.Mutex
	done      chan struct{}
	log       *log.Logger
	pool      selector
	rmap      map[string]struct{}
	wildcards map[string]*wildcard
	queue     queue.Queue
	qps       int
	maxSet    bool
	rate      ratelimit.Limiter
	detector  *resolver
	timeout   time.Duration
	options   *ThresholdOptions
}

type resolver struct {
	done      chan struct{}
	xchgQueue queue.Queue
	xchgs     *xchgMgr
	address   string
	qps       int
	inc       time.Duration
	next      time.Time
	conn      *dns.Conn
	stats     *stats
}

// NewResolvers initializes a Resolvers that starts with the provided list of DNS resolver IP addresses.
func NewResolvers() *Resolvers {
	r := &Resolvers{
		done:      make(chan struct{}, 1),
		log:       log.New(ioutil.Discard, "", 0),
		pool:      new(randomSelector),
		rmap:      make(map[string]struct{}),
		wildcards: make(map[string]*wildcard),
		queue:     queue.NewQueue(),
		timeout:   DefaultTimeout,
		options:   new(ThresholdOptions),
	}

	go r.enforceMaxQPS()
	go r.sendQueries()
	go r.thresholdChecks()
	return r
}

// Len returns the number of resolvers that have been added to the pool.
func (r *Resolvers) Len() int {
	return r.pool.Len()
}

// SetLogger assigns a new logger to the resolver pool.
func (r *Resolvers) SetLogger(l *log.Logger) {
	r.log = l
}

// SetTimeout updates the amount of time this pool will wait for response messages.
func (r *Resolvers) SetTimeout(d time.Duration) {
	r.Lock()
	defer r.Unlock()

	r.timeout = d
	r.updateResolverTimeouts()
}

func (r *Resolvers) updateResolverTimeouts() {
	all := r.pool.AllResolvers()
	if r.detector != nil {
		all = append(all, r.detector)
	}

	for _, res := range all {
		select {
		case <-res.done:
		default:
			res.xchgs.setTimeout(r.timeout)
		}
	}
}

// QPS returns the maximum queries per second provided by the resolver pool.
func (r *Resolvers) QPS() int {
	r.Lock()
	defer r.Unlock()

	return r.qps
}

// SetMaxQPS allows a preferred maximum number of queries per second to be specified for the pool.
func (r *Resolvers) SetMaxQPS(qps int) {
	r.qps = qps
	if qps > 0 {
		r.maxSet = true
		r.rate = ratelimit.New(qps)
		return
	}
	r.maxSet = false
	r.rate = nil
}

// AddResolvers initializes and adds new resolvers to the pool of resolvers.
func (r *Resolvers) AddResolvers(qps int, addrs ...string) error {
	r.Lock()
	defer r.Unlock()

	if qps == 0 {
		return errors.New("failed to provide a maximum number of queries per second greater than zero")
	}

	for _, addr := range addrs {
		if _, found := r.rmap[addr]; found {
			continue
		}
		if res := r.initializeResolver(addr, qps); res != nil {
			r.rmap[addr] = struct{}{}
			r.pool.AddResolver(res)
			if !r.maxSet {
				r.qps += qps
			}
		}
	}
	return nil
}

// Stop will release resources for the resolver pool and all add resolvers.
func (r *Resolvers) Stop() {
	select {
	case <-r.done:
		return
	default:
	}

	close(r.done)

	all := r.pool.AllResolvers()
	if d := r.getDetectionResolver(); d != nil {
		all = append(all, d)
	}

	for _, res := range all {
		if !r.maxSet {
			r.qps -= res.qps
		}
		res.stop()
	}
	r.pool.Close()
}

// Query queues the provided DNS message and returns the response on the provided channel.
func (r *Resolvers) Query(ctx context.Context, msg *dns.Msg, ch chan *dns.Msg) {
	if msg == nil {
		ch <- msg
		return
	}

	select {
	case <-ctx.Done():
	case <-r.done:
	default:
		req := reqPool.Get().(*request)

		req.Ctx = ctx
		req.ID = msg.Id
		req.Name = RemoveLastDot(msg.Question[0].Name)
		req.Qtype = msg.Question[0].Qtype
		req.Msg = msg
		req.Result = ch
		r.queue.Append(req)
		return
	}

	msg.Rcode = RcodeNoResponse
	ch <- msg
}

// Query queues the provided DNS message and sends the response on the returned channel.
func (r *Resolvers) QueryChan(ctx context.Context, msg *dns.Msg) chan *dns.Msg {
	ch := make(chan *dns.Msg, 1)
	r.Query(ctx, msg, ch)
	return ch
}

// Query queues the provided DNS message and returns the associated response message.
func (r *Resolvers) QueryBlocking(ctx context.Context, msg *dns.Msg) (*dns.Msg, error) {
	select {
	case <-ctx.Done():
		return msg, errors.New("the context expired")
	default:
	}

	ch := r.QueryChan(ctx, msg)

	select {
	case <-ctx.Done():
		return msg, errors.New("the context expired")
	case resp := <-ch:
		var err error
		if resp == nil {
			err = errors.New("query failed")
		}
		return resp, err
	}
}

func (r *Resolvers) enforceMaxQPS() {
	for {
		select {
		case <-r.done:
			return
		case <-r.queue.Signal():
			if r.maxSet {
				r.rate.Take()
			}
			e, ok := r.queue.Next()
			if !ok {
				continue
			}
			if req, ok := e.(*request); ok {
				if res := r.pool.GetResolver(); res != nil {
					res.query(req)
					continue
				}
				req.errNoResponse()
				req.release()
			}
		}
	}
}

func (r *Resolvers) sendQueries() {
	for {
		select {
		case <-r.done:
			return
		default:
		}

		if !r.checkAllQueues() {
			time.Sleep(time.Millisecond)
		}
	}
}

func (r *Resolvers) checkAllQueues() bool {
	var sent bool
	cur := time.Now()

	all := r.pool.AllResolvers()
	if d := r.getDetectionResolver(); d != nil {
		all = append(all, d)
	}

	for _, res := range all {
		select {
		case <-res.done:
			continue
		default:
		}
		if res.next.After(cur) {
			continue
		}
		select {
		case <-res.xchgQueue.Signal():
			res.writeNextMsg()
			sent = true
		default:
		}
	}
	return sent
}

func (r *Resolvers) initializeResolver(addr string, qps int) *resolver {
	if _, _, err := net.SplitHostPort(addr); err != nil {
		// Add the default port number to the IP address
		addr = net.JoinHostPort(addr, "53")
	}

	var res *resolver
	c := dns.Client{UDPSize: dns.DefaultMsgSize}
	if conn, err := c.Dial(addr); err == nil {
		_ = conn.SetDeadline(time.Time{})
		res = &resolver{
			done:      make(chan struct{}, 1),
			xchgQueue: queue.NewQueue(),
			xchgs:     newXchgMgr(r.timeout),
			address:   addr,
			qps:       qps,
			inc:       time.Second / time.Duration(qps),
			next:      time.Now(),
			conn:      conn,
			stats:     new(stats),
		}
		go res.responses()
		go res.timeouts()
	}
	return res
}

func (r *resolver) stop() {
	select {
	case <-r.done:
		return
	default:
	}
	// Send the signal to shutdown and close the connection
	close(r.done)
	// Drains the xchgQueue of all requests
	r.xchgQueue.Process(func(e interface{}) {
		req := e.(*request)
		req.errNoResponse()
		req.release()
	})
	// Drain the xchgs of all messages and allow callers to return
	for _, req := range r.xchgs.removeAll() {
		req.errNoResponse()
		req.release()
	}
}

func (r *resolver) query(req *request) {
	select {
	case <-r.done:
	default:
		r.xchgQueue.Append(req)
		return
	}
	req.errNoResponse()
	req.release()
}

func (r *resolver) writeNextMsg() {
	select {
	case <-r.done:
		return
	default:
	}

	element, ok := r.xchgQueue.Next()
	if !ok {
		return
	}
	req := element.(*request)

	select {
	case <-req.Ctx.Done():
	default:
		if err := r.conn.WriteMsg(req.Msg); err == nil && r.xchgs.add(req) == nil {
			// Set the timestamp for message expiration
			r.xchgs.updateTimestamp(req.ID, req.Name)
			// Update the time for the next query to be sent
			r.next = time.Now().Add(r.inc)
			return
		}
	}
	req.errNoResponse()
	req.release()
}

func (r *resolver) responses() {
	defer r.conn.Close()

	for {
		select {
		case <-r.done:
			return
		default:
		}
		_ = r.conn.SetReadDeadline(time.Now().Add(5 * time.Second))
		if m, err := r.conn.ReadMsg(); err == nil && m != nil && len(m.Question) > 0 {
			if req := r.xchgs.remove(m.Id, m.Question[0].Name); req != nil {
				if m.Truncated {
					go r.tcpExchange(req)
					continue
				}
				req.Result <- m
				r.collectStats(m)
				req.release()
			}
		}
	}
}

func (r *resolver) tcpExchange(req *request) {
	client := dns.Client{
		Net:     "tcp",
		Timeout: time.Minute,
	}
	if m, _, err := client.Exchange(req.Msg, r.address); err == nil {
		req.Result <- m
		r.collectStats(m)
	} else {
		req.errNoResponse()
	}
	req.release()
}

func (r *resolver) timeouts() {
	t := time.NewTicker(100 * time.Millisecond)
	defer t.Stop()

	for {
		select {
		case <-r.done:
			return
		case <-t.C:
			for _, req := range r.xchgs.removeExpired() {
				req.errNoResponse()
				r.collectStats(req.Msg)
				req.release()
			}
		}
	}
}
