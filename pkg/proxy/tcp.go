package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

var ErrStopped = errors.New("tcp proxy stopped")

const (
	upstreamRetryInterval = 100 * time.Millisecond
	upstreamRetryTimeout  = 30 * time.Second
)

type TCP struct {
	laddr, raddr  *net.TCPAddr
	mu            sync.Mutex
	listener      *net.TCPListener
	stopped       bool
	runOnce       bool
	onAccept      func(*net.TCPConn) bool
	retryUpstream bool
	dialContext   context.Context
	cancelDials   context.CancelFunc
}

func NewTCPProxy(localAddr, remoteAddr string, failOnClose bool, onAccept ...func(*net.TCPConn) bool) (*TCP, error) {
	laddr, err := net.ResolveTCPAddr("tcp", localAddr)
	if err != nil {
		return nil, err
	}
	raddr, err := net.ResolveTCPAddr("tcp", remoteAddr)
	if err != nil {
		return nil, err
	}

	dialContext, cancelDials := context.WithCancel(context.Background())
	proxy := &TCP{
		laddr:       laddr,
		raddr:       raddr,
		runOnce:     failOnClose,
		dialContext: dialContext,
		cancelDials: cancelDials,
	}
	if len(onAccept) > 0 {
		proxy.onAccept = onAccept[0]
		proxy.retryUpstream = proxy.onAccept != nil
	}
	return proxy, nil
}

func (p *TCP) Start() error {
	log.Infof("starting tcp proxy at %v", p.laddr)

	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return ErrStopped
	}
	listener, err := net.ListenTCP("tcp", p.laddr)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	p.listener = listener
	p.mu.Unlock()

	defer func() {
		_ = listener.Close()
		p.mu.Lock()
		if p.listener == listener {
			p.listener = nil
		}
		p.mu.Unlock()
	}()

	for {
		lconn, err := listener.AcceptTCP()
		if err != nil {
			p.mu.Lock()
			stopped := p.stopped
			p.mu.Unlock()
			if stopped {
				return ErrStopped
			}
			if p.runOnce {
				return nil
			}
			return fmt.Errorf("failed to accept connection: %v", err)
		}

		if p.onAccept != nil && !p.onAccept(lconn) {
			log.WithField("remote", lconn.RemoteAddr()).Warn("rejected untrusted TCP proxy peer")
			_ = lconn.Close()
			continue
		}

		log.WithFields(log.Fields{
			"laddr": p.laddr,
			"raddr": p.raddr,
		}).Info("handling TCP connection")

		go func(runListener *net.TCPListener, conn *net.TCPConn) {
			if handleErr := p.handle(conn); handleErr != nil {
				if !errors.Is(handleErr, ErrStopped) {
					log.Errorf("failed to handle connection: %v", handleErr)
				}
				return
			}
			if p.runOnce {
				_ = runListener.Close()
			}
		}(listener, lconn)
	}
}

func (p *TCP) Stop() error {
	p.mu.Lock()
	p.stopped = true
	p.cancelDials()
	listener := p.listener
	p.mu.Unlock()
	if listener == nil {
		return nil
	}
	return listener.Close()
}

func (p *TCP) dialUpstream() (*net.TCPConn, error) {
	deadline := time.Now().Add(upstreamRetryTimeout)
	for {
		p.mu.Lock()
		stopped := p.stopped
		p.mu.Unlock()
		if stopped {
			return nil, ErrStopped
		}
		conn, err := (&net.Dialer{}).DialContext(p.dialContext, "tcp", p.raddr.String())
		if err == nil {
			rconn, ok := conn.(*net.TCPConn)
			if !ok {
				_ = conn.Close()
				return nil, fmt.Errorf("upstream connection is %T, want *net.TCPConn", conn)
			}
			return rconn, nil
		}
		if errors.Is(err, context.Canceled) {
			return nil, ErrStopped
		}
		if !p.retryUpstream || time.Now().Add(upstreamRetryInterval).After(deadline) {
			return nil, fmt.Errorf("failed to dial upstream: %v", err)
		}
		time.Sleep(upstreamRetryInterval)
	}
}

func (p *TCP) handle(lconn *net.TCPConn) error {
	rconn, err := p.dialUpstream()
	if err != nil {
		lconn.Close()
		return err
	}

	// Use sync.Once to ensure connections are closed exactly once
	var lconnClose, rconnClose sync.Once
	closeLconn := func() { lconnClose.Do(func() { lconn.Close() }) }
	closeRconn := func() { rconnClose.Do(func() { rconn.Close() }) }

	wg := sync.WaitGroup{}
	wg.Add(2)

	go func() {
		defer wg.Done()
		defer closeLconn()
		defer closeRconn()
		if _, err := io.Copy(rconn, lconn); err != nil {
			log.WithFields(log.Fields{
				"laddr": p.laddr,
				"raddr": p.raddr,
			}).Tracef("error copying from %v: %v", lconn.RemoteAddr(), err)
		}
		log.WithFields(log.Fields{
			"laddr": p.laddr,
			"raddr": p.raddr,
		}).Tracef("finished copying from %v", lconn.RemoteAddr())
	}()

	go func() {
		defer wg.Done()
		defer closeLconn()
		defer closeRconn()
		if _, err := io.Copy(lconn, rconn); err != nil {
			log.WithFields(log.Fields{
				"laddr": p.laddr,
				"raddr": p.raddr,
			}).Tracef("error copying to %v: %v", lconn.RemoteAddr(), err)
		}
		log.WithFields(log.Fields{
			"laddr": p.laddr,
			"raddr": p.raddr,
		}).Tracef("finished copying to %v", lconn.RemoteAddr())
	}()

	wg.Wait()
	return nil
}
