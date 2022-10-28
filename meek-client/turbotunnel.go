// The code in this file provides a compatibility layer between an underlying
// packet-based connection and our polling-based domain-fronted HTTP carrier.
// The main interface is PollingPacketConn, which abstracts over a polling
// medium like HTTP. Each request consists of an 8-byte Client ID, followed by a
// sequence of packets and padding encapsulated according to the rules of the
// common/encapsulation package. The downstream direction is the same, except
// that this is no Client ID prefix.

package main

import (
	"bytes"
	"context"
	"io"
	"net"
	"time"

	"git.torproject.org/pluggable-transports/meek.git/common/encapsulation"
	"git.torproject.org/pluggable-transports/meek.git/common/turbotunnel"
)

const (
	// The size of the largest bundle of packets we will send in a poll.
	// (Actually it's not quite a maximum, we will quit bundling as soon as
	// it is exceeded.)
	maxSendBundleLength = 0x10000

	// We must poll the server to see if it has anything to send; there is
	// no way for the server to push data back to us until we poll it. When
	// a timer expires, we send a request even if it has an empty body. The
	// interval starts at this value and then grows.
	initPollDelay = 500 * time.Millisecond
	// Maximum polling interval.
	maxPollDelay = 10 * time.Second
	// Geometric increase in the polling interval each time we fail to read
	// data.
	pollDelayMultiplier = 2.0
)

// Poller is an abstract interface over an operation that writes a stream of
// bytes and reads a stream of bytes in return, like an HTTP request.
type Poller interface {
	Poll(out io.Reader) (in io.ReadCloser, err error)
}

// PollingPacketConn implements the net.PacketConn interface over a carrier of
// HTTP requests and responses. Packets passed to WriteTo are batched,
// encapsulated, and sent in the bodies of HTTP POST requests. Downstream
// packets are unencapsulated and unbatched from HTTP response bodies and put in
// a queue to be returned by ReadFrom.
//
// HTTP interaction is done using an abstract Poller function whose Poll method
// takes a byte slice (an HTTP request body) and returns an io.ReadCloser (an
// HTTP response body). The Poller can apply HTTP request customizations such as
// domain fronting.
type PollingPacketConn struct {
	clientID   turbotunnel.ClientID
	remoteAddr net.Addr
	poller     Poller
	ctx        context.Context
	cancel     context.CancelFunc
	// QueuePacketConn is the direct receiver of ReadFrom and WriteTo calls.
	// requestLoop, via send, removes messages from the outgoing queue that
	// were placed there by WriteTo, and inserts messages into the incoming
	// queue to be returned from ReadFrom.
	*turbotunnel.QueuePacketConn
}

// NewPollingPacketConn creates a PollingPacketConn with a random ClientID as
// the local address. remoteAddr is used only in errors, the return value of
// ReadFrom, and the RemoteAddr method; is is poller that really controls the
// effective remote address.
func NewPollingPacketConn(remoteAddr net.Addr, poller Poller) *PollingPacketConn {
	clientID := turbotunnel.NewClientID()
	ctx, cancel := context.WithCancel(context.Background())
	c := &PollingPacketConn{
		clientID:        clientID,
		remoteAddr:      remoteAddr,
		poller:          poller,
		ctx:             ctx,
		cancel:          cancel,
		QueuePacketConn: turbotunnel.NewQueuePacketConn(clientID, 0),
	}
	go c.requestLoop()
	return c
}

// Close cancels any in-progress polls and closes the underlying
// QueuePacketConn.
func (c *PollingPacketConn) Close() error {
	c.cancel()
	return c.QueuePacketConn.Close()
}

// RemoteAddr returns the remoteAddr value that was passed to
// NewPollingPacketConn.
func (c *PollingPacketConn) RemoteAddr() net.Addr { return c.remoteAddr }

func (c *PollingPacketConn) requestLoop() {
	pollDelay := initPollDelay
	pollTimer := time.NewTimer(pollDelay)
	for {
		var body bytes.Buffer
		body.Write(c.clientID[:])

		var p []byte
		unstash := c.QueuePacketConn.Unstash(c.remoteAddr)
		outgoing := c.QueuePacketConn.OutgoingQueue(c.remoteAddr)
		pollTimerExpired := false
		// Block, waiting for one packet or a demand to poll. Prioritize
		// taking a packet from the stash, then taking one from the
		// outgoing queue, then finally also consider polls.
		select {
		case <-c.ctx.Done():
			return
		case p = <-unstash:
		default:
			select {
			case <-c.ctx.Done():
				return
			case p = <-unstash:
			case p = <-outgoing:
			default:
				select {
				case <-c.ctx.Done():
					return
				case p = <-unstash:
				case p = <-outgoing:
				case <-pollTimer.C:
					pollTimerExpired = true
				}
			}
		}

		if pollTimerExpired {
			// We're polling because it's been a while since we last
			// polled. Increase the poll delay.
			pollDelay = time.Duration(float64(pollDelay) * pollDelayMultiplier)
			if pollDelay > maxPollDelay {
				pollDelay = maxPollDelay
			}
		} else {
			// We're sending an actual data packet. Reset the poll
			// delay to initial.
			if !pollTimer.Stop() {
				<-pollTimer.C
			}
			pollDelay = initPollDelay
		}
		pollTimer.Reset(pollDelay)

		// Grab as many more packets as are immediately available and
		// fit in maxSendBundleLength. Always include the first packet,
		// even if it doesn't fit.
		first := true
		for len(p) > 0 && (first || body.Len()+len(p) <= maxSendBundleLength) {
			first = false

			// Encapsulate the packet into the request body.
			encapsulation.WriteData(&body, p)

			select {
			case p = <-outgoing:
			default:
				p = nil
			}
		}
		if len(p) > 0 {
			// We read an actual packet, but it didn't fit under the
			// limit. Stash it so that it will be first in line for
			// the next poll.
			c.QueuePacketConn.Stash(p, c.remoteAddr)
		}

		go func() {
			resp, err := c.poller.Poll(&body)
			if err != nil {
				c.Close()
				return
			}
			defer resp.Close()
			for {
				p, err := encapsulation.ReadData(resp)
				if err == io.EOF {
					break
				} else if err != nil {
					c.Close()
					return
				}
				c.QueuePacketConn.QueueIncoming(p, c.remoteAddr)
			}
		}()
	}
}
