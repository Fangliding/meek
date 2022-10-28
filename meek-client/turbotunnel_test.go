package main

import (
	"bytes"
	"errors"
	"io"
	"io/ioutil"
	"testing"
	"time"
)

type emptyAddr struct{}

func (_ emptyAddr) Network() string { return "empty" }
func (_ emptyAddr) String() string  { return "empty" }

type funcPoller struct {
	poll func(out io.Reader) (in io.ReadCloser, err error)
}

func (fp funcPoller) Poll(out io.Reader) (in io.ReadCloser, err error) {
	return fp.poll(out)
}

// TestCloseHaltsRequestLoop tests that requestLoop terminates and stops calling
// its Poller after Close is called.
func TestCloseHaltsRequestLoop(t *testing.T) {
	closedCh := make(chan struct{})
	resultCh := make(chan error)
	// The poller returns immediately with a nil error as long as closedCh
	// is not closed. When closedCh is closed, the poller returns
	// immediately with a non-nil error.
	poller := funcPoller{poll: func(_ io.Reader) (io.ReadCloser, error) {
		select {
		case <-closedCh:
			resultCh <- errors.New("poll called after close")
		default:
		}
		return ioutil.NopCloser(bytes.NewReader(nil)), nil
	}}
	pconn := NewPollingPacketConn(emptyAddr{}, poller)
	// Close the connection.
	err := pconn.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Tell the poll function to return an error if it is called after this
	// point.
	close(closedCh)
	// Wait a few seconds to see if the poll function is called after the
	// conn is closed.
	select {
	case err := <-resultCh:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
	}
}
