package sip

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const (
	registerExpiry   = 300 // seconds; advertised Expires value
	reregisterBuffer = 30  // re-register this many seconds before expiry
)

type ThrunkConfig struct {
	Host     string `yaml:"host" json:"host"`
	Port     int    `yaml:"port" json:"port"`
	Username string `yaml:"username" json:"-"`
	Password string `yaml:"password" json:"-"`
}

// trunks holds all configured trunks, keyed by name. Populated by Init.
var trunks = map[string]*Trunk{}

// Trunk maintains a persistent SIP REGISTER with a remote PBX/proxy.
// It provides outbound call capability via dialVia.
type Trunk struct {
	name   string
	config ThrunkConfig

	cancel context.CancelFunc
}

// newTrunk parses rawURL (sip:user:pass@host:port), starts the registration
// keep-alive loop, and returns the Trunk.
func newTrunk(name string, config ThrunkConfig) (*Trunk, error) {

	t := &Trunk{
		name:   name,
		config: config,
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.cancel = cancel

	go t.registerLoop(ctx)

	return t, nil
}

func (t *Trunk) stop() {
	t.cancel()
}

func (t *Trunk) pbxAddr() string {
	return net.JoinHostPort(t.config.Host, strconv.Itoa(t.config.Port))
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
	// 1. Define the Registrar URI
	recipient := sip.Uri{
		Scheme: "sip",
		Host:   t.config.Host,
		// Port is usually handled by the transport/client,
		// but can be set if using a non-standard port.
	}

	req := sip.NewRequest(sip.REGISTER, recipient)

	// 2. Define the Identity (Used for both From and To)
	// Most registrars require From and To to be the same for REGISTER
	identity := sip.Uri{
		User: t.config.Username,
		Host: t.config.Host,
	}

	req.AppendHeader(&sip.FromHeader{
		DisplayName: "Easybell",
		Address:     identity,
	})

	req.AppendHeader(&sip.ToHeader{
		Address: identity,
	})

	// 3. Structured Contact Header
	localIP := outboundIP(t.pbxAddr())
	req.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{
			User: t.config.Username,
			Host: localIP.String(),
			// Port: if you want to force the listening port
		},
	})

	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	// 5. Initial Request (using ClientRequestBuild to handle CSeq and Call-ID)
	res, err := sipClient.Do(tctx, req, sipgo.ClientRequestBuild)
	if err != nil {
		return err
	}

	// 6. Handle Challenge
	if res.StatusCode == 401 || res.StatusCode == 407 {
		res, err = sipClient.DoDigestAuth(tctx, req, res, sipgo.DigestAuth{
			Username: t.config.Username,
			Password: t.config.Password,
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
		url.PathEscape(t.config.Username),
		url.PathEscape(t.config.Password),
		t.config.Host, t.config.Port, url.PathEscape(callee),
	)
	return dialSIP(rawURL)
}
