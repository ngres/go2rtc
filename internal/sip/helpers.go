package sip

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/emiago/sipgo"
	"github.com/emiago/sipgo/sip"
	sipmsg "github.com/emiago/sipgo/sip"
	"github.com/pion/sdp/v3"
)

// ─── URL helpers ───────────────────────────────────────────────────────────────

// parseSIPURL splits a SIP URL into trunk name and callee.
//
//	sip:trunk/callee  →  ("trunk", "callee")
//	sip:callee        →  ("", "callee")
func parseSIPURL(rawURL string) (trunkName, callee string, err error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", "", fmt.Errorf("parse SIP URL: %w", err)
	}
	opaque := u.Opaque
	if opaque == "" {
		// Hierarchical form sip://host/path — not a trunk URL; pass through to dialSIP.
		return "", "", nil
	}
	if trunk, callee, ok := strings.Cut(opaque, "/"); ok {
		return trunk, callee, nil
	}
	return "", opaque, nil
}

// dialRegistered calls a phone that has registered with the internal SIP server.
// It looks up the phone's contact address from the registration table.
func dialRegistered(callee string) (*sipSession, error) {
	v, ok := registrations.Load(callee)
	if !ok {
		return nil, fmt.Errorf("SIP callee %q is not registered with the internal server", callee)
	}
	hostPort := v.(string)
	rawURL := fmt.Sprintf("sip://%s/%s", hostPort, callee)
	return dialSIP(rawURL, &ThrunkConfig{})
}

// ─── Shared dial helper ─────────────────────────────────────────────────────────

// sipSession holds the result of a successful outbound SIP call setup.
type sipSession struct {
	udpConn    *net.UDPConn
	remoteAddr *net.UDPAddr
	codec      *core.Codec
	dialog     *sipgo.DialogClientSession
}

// dialSIP parses rawURL, applies pbxConf defaults for any missing host or credentials,
// and performs the full SIP INVITE handshake. The caller owns the returned session
// and must send BYE / close the socket when done.
//
// Supported URL forms:
//
//	sip://user:pass@host:port/callee  – all fields explicit
//	sip:callee                        – host, port and credentials taken from pbxConf
func dialSIP(rawURL string, trunk *ThrunkConfig) (*sipSession, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse SIP URL: %w", err)
	}

	username := u.User.Username()
	password, _ := u.User.Password()
	pbxHost := u.Host

	pbxIP, pbxPortStr, err := net.SplitHostPort(pbxHost)
	if err != nil {
		pbxIP = pbxHost
		pbxPortStr = "5060"
	}
	pbxPort, _ := strconv.Atoi(pbxPortStr)

	// Callee is the URL path (full form) or opaque segment (short form "sip:callee").
	callee := strings.TrimPrefix(u.Path, "/")
	if callee == "" {
		callee = u.Opaque
	}
	if callee == "" {
		callee = username
	}
	if callee == "" {
		return nil, fmt.Errorf("SIP URL missing callee: %s", rawURL)
	}

	localIP := outboundIP(net.JoinHostPort(pbxIP, pbxPortStr))

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: localIP, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("bind RTP socket: %w", err)
	}

	localPort := udpConn.LocalAddr().(*net.UDPAddr).Port
	offerSDP := buildOfferSDP(localIP, localPort)

	recipient := sipmsg.Uri{
		Scheme: "sip",
		User:   callee,
		Host:   pbxIP,
		Port:   pbxPort,
	}
	ct := sipmsg.ContentTypeHeader("application/sdp")
	from := sipmsg.FromHeader{
		DisplayName: trunk.DisplayName,
		Address: sip.Uri{
			User: trunk.Username,
			Host: trunk.Host,
		},
	}

	// 30-second timeout so the caller's reconnect timer doesn't fire before we
	// know whether the remote party answered.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dialog, err := clientDialogs.Invite(ctx, recipient, offerSDP, &ct, &from)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("INVITE: %w", err)
	}

	if err := dialog.WaitAnswer(ctx, sipgo.AnswerOptions{
		Username: username,
		Password: password,
	}); err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("wait answer: %w", err)
	}

	if err := dialog.Ack(ctx); err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("ACK: %w", err)
	}

	answerBody := dialog.InviteResponse.Body()
	if len(answerBody) == 0 {
		_ = udpConn.Close()
		return nil, fmt.Errorf("empty answer SDP")
	}

	remoteAddr, codecs, err := parseAudioAddr(answerBody)
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("answer SDP: %w", err)
	}

	return &sipSession{
		udpConn:    udpConn,
		remoteAddr: remoteAddr,
		codec:      codecs[0],
		dialog:     dialog,
	}, nil
}

// ─── Shared SDP helpers ─────────────────────────────────────────────────────────

func buildOfferSDP(localIP net.IP, localPort int) []byte {
	ipStr := "0.0.0.0"
	if localIP != nil && !localIP.IsUnspecified() {
		ipStr = localIP.String()
	}

	sd := sdp.SessionDescription{
		Origin: sdp.Origin{
			Username:       "-",
			SessionID:      1,
			SessionVersion: 1,
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: ipStr,
		},
		SessionName: "go2rtc",
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: ipStr},
		},
		TimeDescriptions: []sdp.TimeDescription{{Timing: sdp.Timing{}}},
		MediaDescriptions: []*sdp.MediaDescription{{
			MediaName: sdp.MediaName{
				Media:   "audio",
				Port:    sdp.RangedPort{Value: localPort},
				Protos:  []string{"RTP", "AVP"},
				Formats: []string{"0", "8", "96"},
			},
			Attributes: []sdp.Attribute{
				{Key: "rtpmap", Value: "0 PCMU/8000"},
				{Key: "rtpmap", Value: "8 PCMA/8000"},
				{Key: "rtpmap", Value: "96 OPUS/48000/2"},
				{Key: core.DirectionSendRecv},
			},
		}},
	}

	b, _ := sd.Marshal()
	return b
}

// parseAudioAddr extracts the remote RTP address and all PCMA/PCMU/Opus codecs from an SDP body.
func parseAudioAddr(body []byte) (*net.UDPAddr, []*core.Codec, error) {
	var sd sdp.SessionDescription
	if err := sd.Unmarshal(body); err != nil {
		return nil, nil, fmt.Errorf("unmarshal: %w", err)
	}

	var audioMD *sdp.MediaDescription
	for _, md := range sd.MediaDescriptions {
		if md.MediaName.Media == "audio" {
			audioMD = md
			break
		}
	}
	if audioMD == nil {
		return nil, nil, fmt.Errorf("no audio media")
	}

	remoteIP := ""
	if sd.ConnectionInformation != nil && sd.ConnectionInformation.Address != nil {
		remoteIP = sd.ConnectionInformation.Address.Address
	}
	if audioMD.ConnectionInformation != nil && audioMD.ConnectionInformation.Address != nil {
		remoteIP = audioMD.ConnectionInformation.Address.Address
	}

	var codecs []*core.Codec
	for _, format := range audioMD.MediaName.Formats {
		c := core.UnmarshalCodec(audioMD, format)
		if c.Name == core.CodecPCMA || c.Name == core.CodecPCMU || c.Name == core.CodecOpus {
			codecs = append(codecs, c)
		}
	}
	if len(codecs) == 0 {
		return nil, nil, fmt.Errorf("no PCMA/PCMU/Opus codec (formats: %v)", audioMD.MediaName.Formats)
	}

	addr := &net.UDPAddr{
		IP:   net.ParseIP(remoteIP),
		Port: audioMD.MediaName.Port.Value,
	}
	return addr, codecs, nil
}

// ─── Helpers ───────────────────────────────────────────────────────────────────

// outboundIP returns the local IP the OS would use to reach remoteAddr.
func outboundIP(remoteAddr string) net.IP {
	host, _, _ := net.SplitHostPort(remoteAddr)
	if host == "" {
		return net.IPv4zero
	}
	conn, err := net.Dial("udp", net.JoinHostPort(host, "1"))
	if err != nil {
		return net.IPv4zero
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP
}
