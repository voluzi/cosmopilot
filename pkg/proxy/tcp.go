package proxy

import (
	"fmt"
	"io"
	"net"
	"sync"
	"time"

	log "github.com/sirupsen/logrus"
)

const (
	upstreamRetryInterval = 100 * time.Millisecond
	upstreamRetryTimeout  = 30 * time.Second
)

type TCP struct {
	laddr, raddr  *net.TCPAddr
	listener      *net.TCPListener
	runOnce       bool
	onAccept      func()
	retryUpstream bool
}

func NewTCPProxy(localAddr, remoteAddr string, failOnClose bool, onAccept ...func()) (*TCP, error) {
	laddr, err := net.ResolveTCPAddr("tcp", localAddr)
	if err != nil {
		return nil, err
	}
	raddr, err := net.ResolveTCPAddr("tcp", remoteAddr)
	if err != nil {
		return nil, err
	}

	proxy := &TCP{
		laddr:   laddr,
		raddr:   raddr,
		runOnce: failOnClose,
	}
	if len(onAccept) > 0 {
		proxy.onAccept = onAccept[0]
		proxy.retryUpstream = true
	}
	return proxy, nil
}

func (p *TCP) Start() error {
	var err error
	log.Infof("starting tcp proxy at %v", p.laddr)
	p.listener, err = net.ListenTCP("tcp", p.laddr)
	if err != nil {
		return err
	}
	defer p.listener.Close()

	for {
		lconn, err := p.listener.AcceptTCP()
		if err != nil {
			log.Errorf("failed to accept connection: %v", err)
			continue
		}

		if p.onAccept != nil {
			p.onAccept()
		}

		log.WithFields(log.Fields{
			"laddr": p.laddr,
			"raddr": p.raddr,
		}).Info("handling TCP connection")

		if err = p.handle(lconn); err != nil {
			log.Errorf("failed to handle connection: %v", err)
			continue
		}
		if p.runOnce {
			return err
		}
	}
}

func (p *TCP) Stop() error {
	return p.listener.Close()
}

func (p *TCP) dialUpstream() (*net.TCPConn, error) {
	deadline := time.Now().Add(upstreamRetryTimeout)
	for {
		rconn, err := net.DialTCP("tcp", nil, p.raddr)
		if err == nil {
			return rconn, nil
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
