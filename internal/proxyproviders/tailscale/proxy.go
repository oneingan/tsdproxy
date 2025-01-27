// SPDX-FileCopyrightText: 2024 Paulo Almeida <almeidapaulopt@gmail.com>
// SPDX-License-Identifier: MIT
package tailscale

import (
	"context"
	"errors"
	"net"
	"strings"
	"sync"

	"github.com/almeidapaulopt/tsdproxy/internal/proxyconfig"
	"github.com/almeidapaulopt/tsdproxy/internal/proxyproviders"

	"github.com/rs/zerolog"
	"tailscale.com/client/tailscale"
	"tailscale.com/ipn"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"
)

// Proxy struct implements proxyconfig.Proxy.
type Proxy struct {
	log      zerolog.Logger
	config   *proxyconfig.Config
	tsServer *tsnet.Server
	status   *ipnstate.Status
	lc       *tailscale.LocalClient
	ctx      context.Context

	events chan proxyproviders.ProxyEvent

	authURL string
	state   proxyconfig.ProxyState

	mu sync.Mutex
}

// Start method implements proxyconfig.Proxy Start method.
func (p *Proxy) Start(ctx context.Context) error {
	var (
		err error
		lc  *tailscale.LocalClient
	)

	if err = p.tsServer.Start(); err != nil {
		return err
	}

	if lc, err = p.tsServer.LocalClient(); err != nil {
		return err
	}

	p.mu.Lock()
	p.ctx = ctx
	p.lc = lc
	p.mu.Unlock()

	go p.watchStatus()

	return nil
}

func (p *Proxy) GetURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.status == nil {
		return ""
	}

	// TODO: should be configurable and not force to https
	return "https://" + strings.TrimRight(p.status.Self.DNSName, ".")
}

func (p *Proxy) watchStatus() {
	watcher, err := p.lc.WatchIPNBus(p.ctx, ipn.NotifyInitialState)
	if err != nil {
		p.log.Error().Err(err).Msg("tailscale.watchStatus")
		return
	}
	defer watcher.Close()

	for {
		n, err := watcher.Next()
		if err != nil {
			if !errors.Is(err, context.Canceled) {
				p.log.Error().Err(err).Msg("tailscale.watchStatus: Next")
			}
			return
		}

		if n.ErrMessage != nil {
			p.log.Error().Err(err).Msg("tailscale.watchStatus: backend")
			return
		}

		switch {
		case n.BrowseToURL != nil:
			p.mu.Lock()
			p.state = proxyconfig.ProxyStateAuthenticating
			p.authURL = *n.BrowseToURL
			p.mu.Unlock()

			p.events <- proxyproviders.ProxyEvent{
				AuthURL: *n.BrowseToURL,
				State:   proxyconfig.ProxyStateAuthenticating,
			}

		case n.LoginFinished != nil:
			p.mu.Lock()
			p.state = proxyconfig.ProxyStateStarting
			p.mu.Unlock()

			p.events <- proxyproviders.ProxyEvent{
				State: proxyconfig.ProxyStateStarting,
			}
		}

		if s := n.State; s != nil {
			status, err := p.lc.Status(p.ctx)
			if err != nil && !errors.Is(err, net.ErrClosed) {
				p.log.Error().Err(err).Msg("tailscale.watchStatus: status")
				return
			}

			p.status = status
		}
	}
}

// Close method implements proxyconfig.Proxy Close method.
func (p *Proxy) Close() error {
	if p.tsServer != nil {
		return p.tsServer.Close()
	}

	return nil
}

// NewListener method implements proxyconfig.Proxy NewListener method.
func (p *Proxy) NewListener(network, addr string) (net.Listener, error) {
	return p.tsServer.Listen(network, addr)
}

// NewTLSListener method implements proxyconfig.Proxy NewTLSListener method.
func (p *Proxy) NewTLSListener(network, addr string) (net.Listener, error) {
	if p.config.Tailscale.Funnel {
		return p.tsServer.ListenFunnel(network, addr)
	}

	return p.tsServer.ListenTLS(network, addr)
}

func (p *Proxy) WatchEvents() chan proxyproviders.ProxyEvent {
	return p.events
}

func (p *Proxy) GetAuthURL() string {
	return p.authURL
}

func (p *Proxy) getTLSCertificates() error {
	p.log.Info().Msg("Generating TLS certificate")
	certDomains := p.tsServer.CertDomains()
	if _, _, err := p.lc.CertPair(p.ctx, certDomains[0]); err != nil {
		return err
	}
	p.log.Info().Msg("TLS certificate generated")
	return nil
}
