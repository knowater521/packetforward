// packetforward provides a mechanism for forwarding IP packets from a client
// to a NAT server, which in turn proxies them to their final destination.
//
// - Clients are uniquely identified by a random UUID.
// - Clients connect to the server using a configurable dial function.
// - In the event of a disconnect, clients can reconnect with the same client ID
// - Interrupted and resumed client connections do not disconnect the clients' TCP connections to the origin
// - Currently, packetforward supports only TCP and UDP
//
package packetforward

import (
	"context"
	"io"
	"math"
	"net"
	"time"

	"github.com/getlantern/errors"
	"github.com/getlantern/framed"
	"github.com/getlantern/golog"
	"github.com/getlantern/gonat"
	"github.com/getlantern/idletiming"
	"github.com/getlantern/ops"
	"github.com/getlantern/uuid"
)

var log = golog.LoggerFor("packetforward")

const (
	maxDialDelay = 1 * time.Second
)

// DialFunc is a function that dials a server, preferrably respecting any timeout
// in the provided Context.
type DialFunc func(ctx context.Context) (net.Conn, error)

type forwarder struct {
	id                    string
	downstream            io.Writer
	idleTimeout           time.Duration
	dialServer            DialFunc
	upstreamConn          net.Conn
	upstream              io.ReadWriteCloser
	copyToDownstreamError chan error
}

// Client creates a new packetforward client and returns a WriteCloser. Consumers of packetforward
// should write whole IP packets to this WriteCloser. The packetforward client will write response
// packets to the specified downstream Writer. idleTimeout specifies a timeout for idle clients.
// When the client to server connection remains idle for longer than idleTimeout, it is automatically
// closed. dialServer configures how to connect to the packetforward server. When packetforwarding is
// no longer needed, consumers should Close the returned WriteCloser to clean up any outstanding resources.
func Client(downstream io.Writer, idleTimeout time.Duration, dialServer DialFunc) io.WriteCloser {
	id := uuid.New().String()
	return &forwarder{
		id:                    id,
		downstream:            downstream,
		idleTimeout:           idleTimeout,
		dialServer:            dialServer,
		copyToDownstreamError: make(chan error, 1),
	}
}

func (f *forwarder) Write(b []byte) (int, error) {
	writeErr := f.writeToUpstream(b)
	if writeErr != nil {
		return 0, writeErr
	}
	return len(b), nil
}

func (f *forwarder) writeToUpstream(b []byte) error {
	// Keep trying to transmit the client packet
	priorAttempts := float64(-1)
	sleepTime := 50 * time.Millisecond
	maxSleepTime := f.idleTimeout

	firstDial := true
	for {
		if priorAttempts > -1 {
			sleepTime := time.Duration(math.Pow(2, priorAttempts)) * sleepTime
			if sleepTime > maxSleepTime {
				sleepTime = maxSleepTime
			}
			time.Sleep(sleepTime)
		}
		priorAttempts++

		if f.upstreamConn == nil {
			if !firstDial {
				// wait for copying to downstream to finish
				<-f.copyToDownstreamError
			}
			if err := f.dialUpstream(); err != nil {
				log.Error(err)
				continue
			}
			firstDial = false
		}

		priorAttempts = -1

		_, writeErr := f.upstream.Write(b)
		if writeErr != nil {
			f.closeUpstream()
			log.Errorf("Unexpected error writing to upstream: %v", writeErr)
			continue
		}

		return nil
	}
}

func (f *forwarder) dialUpstream() error {
	log.Debug("Dialing upstream")
	ctx, cancel := context.WithTimeout(context.Background(), f.idleTimeout)
	upstreamConn, dialErr := f.dialServer(ctx)
	cancel()
	if dialErr != nil {
		return errors.New("Error dialing upstream, will retry: %v", dialErr)
	}
	upstreamConn = idletiming.Conn(upstreamConn, f.idleTimeout, nil)
	rwc := framed.NewReadWriteCloser(upstreamConn)
	rwc.EnableBigFrames()
	rwc.EnableBuffering(gonat.MaximumIPPacketSize)
	rwc.DisableThreadSafety()
	upstream := rwc
	if _, err := upstream.Write([]byte(f.id)); err != nil {
		return errors.New("Error sending client ID to upstream, will retry: %v", err)
	}
	f.upstreamConn, f.upstream = upstreamConn, upstream
	ops.Go(func() {
		f.copyToDownstream(upstreamConn, upstream)
	})
	return nil
}

func (f *forwarder) copyToDownstream(upstreamConn net.Conn, upstream io.ReadWriteCloser) {
	b := make([]byte, gonat.MaximumIPPacketSize)
	for {
		n, readErr := upstream.Read(b)
		if n > 0 {
			_, writeErr := f.downstream.Write(b[:n])
			if writeErr != nil {
				upstream.Close()
				f.copyToDownstreamError <- writeErr
				return
			}
		}
		if readErr != nil {
			upstream.Close()
			f.copyToDownstreamError <- readErr
			return
		}
	}
}

func (f *forwarder) closeUpstream() {
	if f.upstream != nil {
		f.upstream.Close()
		f.upstream = nil
		f.upstreamConn = nil
	}
}

func (f *forwarder) Close() error {
	f.closeUpstream()
	<-f.copyToDownstreamError
	return nil
}
