package sip

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"time"

	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
)

const (
	registerExpiry   = 300 // seconds; advertised Expires value
	reregisterBuffer = 30  // re-register this many seconds before expiry
)

type TrunkConfig struct {
	DisplayName string `yaml:"display_name" json:"display_name"`
	Host        string `yaml:"host" json:"host"`
	Port        int    `yaml:"port" json:"port"`
	Username    string `yaml:"username" json:"-"`
	Password    string `yaml:"password" json:"-"`
}

// trunks holds all configured trunks, keyed by name. Populated by Init.
var trunks = map[string]*Trunk{}

// Trunk maintains a persistent SIP REGISTER with a remote PBX/proxy.
// It provides outbound call capability via dialVia.
type Trunk struct {
	name   string
	config TrunkConfig

	cancel context.CancelFunc
}

// newTrunk starts the registration keep-alive loop and returns the Trunk.
func newTrunk(name string, config TrunkConfig) (*Trunk, error) {
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
	recipient := sip.Uri{Scheme: "sip", Host: t.config.Host}
	req := sip.NewRequest(sip.REGISTER, recipient)

	// From and To must be the same identity for REGISTER.
	identity := sip.Uri{User: t.config.Username, Host: t.config.Host}
	req.AppendHeader(&sip.FromHeader{DisplayName: t.name, Address: identity})
	req.AppendHeader(&sip.ToHeader{Address: identity})

	localIP := outboundIP(t.pbxAddr())
	req.AppendHeader(&sip.ContactHeader{
		Address: sip.Uri{User: t.config.Username, Host: localIP.String()},
	})

	tctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	res, err := sipClient.Do(tctx, req, sipgo.ClientRequestBuild)
	if err != nil {
		return err
	}

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
	return dialSIP(dialParams{
		callee:      callee,
		pbxHost:     t.config.Host,
		pbxPort:     t.config.Port,
		username:    t.config.Username,
		password:    t.config.Password,
		displayName: t.config.DisplayName,
	})
}
