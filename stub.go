package radix

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"net"
	"sync"
	"time"

	"github.com/mediocregopher/radix/v4/resp"
	"github.com/mediocregopher/radix/v4/resp/resp3"
)

type buffer struct {
	remoteAddr net.Addr

	bufL   *sync.Cond
	buf    *bytes.Buffer
	bufbr  *bufio.Reader
	closed bool
}

func newBuffer(remoteNetwork, remoteAddr string) *buffer {
	buf := new(bytes.Buffer)
	return &buffer{
		remoteAddr: rawAddr{network: remoteNetwork, addr: remoteAddr},
		bufL:       sync.NewCond(new(sync.Mutex)),
		buf:        buf,
		bufbr:      bufio.NewReader(buf),
	}
}

func (b *buffer) Encode(m interface{}) error {
	b.bufL.L.Lock()
	var err error
	if b.closed {
		err = b.err("write", errClosed)
	} else {
		err = resp3.Marshal(b.buf, m, resp.NewOpts())
	}
	b.bufL.L.Unlock()
	if err != nil {
		return err
	}

	b.bufL.Broadcast()
	return nil
}

func (b *buffer) Decode(ctx context.Context, u interface{}) error {
	b.bufL.L.Lock()
	defer b.bufL.L.Unlock()

	wakeupTicker := time.NewTicker(250 * time.Millisecond)
	defer wakeupTicker.Stop()

	for b.buf.Len() == 0 && b.bufbr.Buffered() == 0 {
		if b.closed {
			return b.err("read", errClosed)
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
		}

		// we have to periodically wakeup to check if the context is done
		go func() {
			<-wakeupTicker.C
			b.bufL.Broadcast()
		}()

		b.bufL.Wait()
	}

	return resp3.Unmarshal(b.bufbr, u, resp.NewOpts())
}

func (b *buffer) Close() error {
	b.bufL.L.Lock()
	defer b.bufL.L.Unlock()
	if b.closed {
		return b.err("close", errClosed)
	}
	b.closed = true
	b.bufL.Broadcast()
	return nil
}

func (b *buffer) err(op string, err error) error {
	return &net.OpError{
		Op:     op,
		Net:    "tcp",
		Source: nil,
		Addr:   b.remoteAddr,
		Err:    err,
	}
}

var errClosed = errors.New("use of closed network connection")

////////////////////////////////////////////////////////////////////////////////

type stub struct {
	network, addr string
	*buffer
	fn func([]string) interface{}
}

// NewStubConn returns a (fake) Conn which pretends it is a Conn to a real redis
// instance, but is instead using the given callback to service requests. It is
// primarily useful for writing tests.
//
// When EncodeDecode is called the value to be marshaled is converted into a
// []string and passed to the callback. The return from the callback is then
// marshaled into an internal buffer. The value to be decoded is unmarshaled
// into using the internal buffer. If the internal buffer is empty at
// this step then the call will block.
//
// remoteNetwork and remoteAddr can be empty, but if given will be used as the
// return from the RemoteAddr method.
//
func NewStubConn(remoteNetwork, remoteAddr string, fn func([]string) interface{}) Conn {
	return &stub{
		network: remoteNetwork, addr: remoteAddr,
		buffer: newBuffer(remoteNetwork, remoteAddr),
		fn:     fn,
	}
}

func (s *stub) Do(ctx context.Context, a Action) error {
	return a.Perform(ctx, s)
}

func (s *stub) EncodeDecode(ctx context.Context, m, u interface{}) error {
	if m != nil {
		buf := new(bytes.Buffer)
		if err := resp3.Marshal(buf, m, resp.NewOpts()); err != nil {
			return err
		}
		br := bufio.NewReader(buf)

		for {
			var ss []string
			if buf.Len() == 0 && br.Buffered() == 0 {
				break
			} else if err := resp3.Unmarshal(br, &ss, resp.NewOpts()); err != nil {
				return err
			}

			// get return from callback. Results implementing resp.Marshaler are
			// assumed to be wanting to be written in all cases, otherwise if
			// the result is an error it is assumed to want to be returned
			// directly.
			ret := s.fn(ss)
			if m, ok := ret.(resp.Marshaler); ok {
				if err := s.buffer.Encode(m); err != nil {
					return err
				}
			} else if err, _ := ret.(error); err != nil {
				return err
			} else if err = s.buffer.Encode(ret); err != nil {
				return err
			}
		}
	}

	if u != nil {
		if err := s.buffer.Decode(ctx, u); err != nil {
			return err
		}
	}

	return nil
}

func (s *stub) Addr() net.Addr {
	return rawAddr{network: s.network, addr: s.addr}
}
