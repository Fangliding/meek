// The code in this package provides a compatibility layer between an underlying
// packet-based connection and our polling-based domain-fronted HTTP carrier.
// The main interface is QueuePacketConn, which stores packets in buffered
// channels instead of sending/receiving them on some concrete network
// interface. Callers can inspect these channels.
package turbotunnel

import "errors"

// The size of receive and send queues.
const queueSize = 256

var errClosedPacketConn = errors.New("operation on closed connection")
var errNotImplemented = errors.New("not implemented")
