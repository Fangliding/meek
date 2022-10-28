package main

import (
	"bytes"
	"context"
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
	poll func(ctx context.Context, out io.Reader) (in io.ReadCloser, err error)
}

func (fp funcPoller) Poll(ctx context.Context, out io.Reader) (in io.ReadCloser, err error) {
	return fp.poll(ctx, out)
}

// TestCloseCancelsPoll tests that calling Close cancels the context passed to
// the poller.
func TestCloseCancelsPoll(t *testing.T) {
	beginCh := make(chan struct{})
	resultCh := make(chan error)
	// The poller returns immediately with a nil error when its context is
	// canceled. It returns after a delay with a non-nil error if its
	// context is not canceled.
	poller := funcPoller{poll: func(ctx context.Context, _ io.Reader) (io.ReadCloser, error) {
		defer close(resultCh)
		beginCh <- struct{}{}
		select {
		case <-ctx.Done():
			resultCh <- nil
		case <-time.After(5 * time.Second):
			resultCh <- errors.New("poll was not canceled")
		}
		return ioutil.NopCloser(bytes.NewReader(nil)), nil
	}}
	pconn := NewPollingPacketConn(emptyAddr{}, poller)
	// Wait until the poll function has been called.
	<-beginCh
	// Close the connection.
	err := pconn.Close()
	if err != nil {
		t.Fatal(err)
	}
	// Observe what happened inside the poll function. Closing the
	// connection should have canceled the context.
	err = <-resultCh
	if err != nil {
		t.Fatal(err)
	}
}

// TestCloseHaltsRequestLoop tests that requestLoop terminates and stops calling
// its Poller after Close is called.
func TestCloseHaltsRequestLoop(t *testing.T) {
	resultCh := make(chan error)
	// The poller returns immediately with a nil error as long as closedCh
	// is not closed. When closedCh is closed, the poller returns
	// immediately with a non-nil error.
	poller := funcPoller{poll: func(ctx context.Context, _ io.Reader) (io.ReadCloser, error) {
		select {
		case <-ctx.Done():
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
	// Wait a few seconds to see if the poll function is called after the
	// conn is closed.
	select {
	case err := <-resultCh:
		t.Fatal(err)
	case <-time.After(3 * time.Second):
	}
}
