package sip

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"
)

const (
	registerExpiry   = 300 // seconds; advertised Expires value
	reregisterBuffer = 30  // re-register this many seconds before expiry
)

// trunks holds all configured trunks, keyed by name. Populated by Init.
var trunks = map[string]*Trunk{}

// Trunk maintains a persistent SIP REGISTER with a remote PBX/proxy.
// It provides outbound call capability via dialVia.
type Trunk struct {
	name     string
	username string
	password string
	host     string
	port     int

	cancel context.CancelFunc
}

// newTrunk parses rawURL (sip:user:pass@host:port), starts the registration
// keep-alive loop, and returns the Trunk.
func newTrunk(name, rawURL string) (*Trunk, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse trunk URL %q: %w", rawURL, err)
	}

	host, portStr, splitErr := net.SplitHostPort(u.Host)
	if splitErr != nil {
		host = u.Host
		portStr = "5060"
	}
	port, _ := strconv.Atoi(portStr)
	if port == 0 {
		port = 5060
	}

	t := &Trunk{
		name:     name,
		username: u.User.Username(),
		host:     host,
		port:     port,
	}
	t.password, _ = u.User.Password()

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	go t.registerLoop(ctx)

	return t, nil
}

func (t *Trunk) stop() {
	t.cancel()
}

func (t *Trunk) pbxAddr() string {
	return net.JoinHostPort(t.host, strconv.Itoa(t.port))
}

// registerLoop sends REGISTER on startup and re-registers before the
// previous registration expires. On failure it retries every 30 s.
func (t *Trunk) registerLoop(ctx context.Context) {
	for {
		if err := t.register(ctx); err != nil {
			log.Error().Err(err).Str("trunk", t.name).Msg("[sip] REGISTER failed, retry in 30s")
			select {
			case <-ctx.Done():
				return
			case <-time.After(30 * time.Second):
			}
			continue
		}

		log.Info().Str("trunk", t.name).Str("server", t.pbxAddr()).Msg("[sip] trunk registered")

		select {
		case <-ctx.Done():
			return
		case <-time.After((registerExpiry - reregisterBuffer) * time.Second):
		}
	}
}

// register sends a single REGISTER transaction, handling digest auth if
// the server challenges with 401/407.
func (t *Trunk) register(ctx context.Context) error {
	recipient := sipmsg.Uri{
		Scheme: "sip",
		User:   t.username,
		Host:   t.host,
		Port:   t.port,
	}

	localIP := outboundIP(t.pbxAddr())

	req := sipmsg.NewRequest(sipmsg.REGISTER, recipient)
	req.AppendHeader(sipmsg.NewHeader(
		"Contact",
		fmt.Sprintf("<sip:%s@%s>", t.username, localIP.String()),
	))
	req.AppendHeader(sipmsg.NewHeader("Expires", strconv.Itoa(registerExpiry)))

	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	res, err := sipClient.Do(tctx, req, sipgo.ClientRequestRegisterBuild)
	if err != nil {
		return err
	}

	if res.StatusCode == 401 || res.StatusCode == 407 {
		res, err = sipClient.DoDigestAuth(tctx, req, res, sipgo.DigestAuth{
			Username: t.username,
			Password: t.password,
		})
		if err != nil {
			return err
		}
	}

	if res.StatusCode != 200 {
		return fmt.Errorf("REGISTER %d %s", res.StatusCode, res.Reason)
	}

	return nil
}

// dialVia places an outbound SIP INVITE to callee through this trunk.
// Credentials and PBX address are taken from the trunk configuration.
func (t *Trunk) dialVia(callee string) (*sipSession, error) {
	rawURL := fmt.Sprintf("sip://%s:%s@%s:%d/%s",
		url.PathEscape(t.username),
		url.PathEscape(t.password),
		t.host, t.port, url.PathEscape(callee),
	)
	return dialSIP(rawURL)
}
