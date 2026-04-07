package sip

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/rs/zerolog"
)

var log zerolog.Logger

// Module-level SIP stack shared by server and client.
var (
	ua            *sipgo.UserAgent
	sipClient     *sipgo.Client
	clientDialogs *sipgo.DialogClientCache
)

// callEntry tracks an active server-side SIP session (phone calling us).
type callEntry struct {
	stream *streams.Stream
	conn   *Conn
}

// calls maps SIP Call-ID → *callEntry for server-side sessions.
var calls sync.Map

func Init() {
	var conf struct {
		Mod struct {
			Listen   string `yaml:"listen" json:"listen"`
			Username string `yaml:"username" json:"-"`
			Password string `yaml:"password" json:"-"`
		} `yaml:"sip"`
	}

	conf.Mod.Listen = ":5060"

	app.LoadConfig(&conf)
	app.Info["sip"] = conf.Mod

	log = app.GetLogger("sip")

	// Create the shared User Agent used for both server and client.
	var err error
	ua, err = sipgo.NewUA(sipgo.WithUserAgent(app.UserAgent))
	if err != nil {
		log.Error().Err(err).Msg("[sip] new UA")
		return
	}

	sipClient, err = sipgo.NewClient(ua)
	if err != nil {
		log.Error().Err(err).Msg("[sip] new client")
		return
	}

	// The Contact host is intentionally left empty; sipgo fills it in from
	// the Via header at send time so we always advertise the correct address.
	contactHDR := sipmsg.ContactHeader{
		Address: sipmsg.Uri{Scheme: "sip"},
	}
	clientDialogs = sipgo.NewDialogClientCache(sipClient, contactHDR)

	// Register handler so streams can use  sip://user:pass@pbx/callee  as a source.
	streams.HandleFunc("sip", sipProducerHandler)

	if conf.Mod.Listen == "" {
		return
	}

	go runServer(conf.Mod.Listen)
}

// ─── Server (UAS) ──────────────────────────────────────────────────────────────

func runServer(listen string) {
	srv, err := sipgo.NewServer(ua)
	if err != nil {
		log.Error().Err(err).Msg("[sip] new server")
		return
	}

	srv.OnInvite(handleInvite)
	srv.OnBye(handleBye)
	srv.OnAck(func(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
		// ACK completes the 3-way handshake; media is already flowing.
	})
	srv.OnOptions(func(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
		_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 200, "OK", nil))
	})

	log.Info().Str("addr", listen).Msg("[sip] listen")

	if err := srv.ListenAndServe(context.Background(), "udp", listen); err != nil {
		log.Error().Err(err).Msg("[sip] serve")
	}
}

func handleInvite(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 100, "Trying", nil))

	// Derive stream name from the request URI user part, e.g. sip:doorbell@host → "doorbell".
	streamName := req.Recipient.User
	if streamName == "" {
		streamName = "sip"
	}

	stream := streams.Get(streamName)
	if stream == nil {
		log.Warn().Str("stream", streamName).Msg("[sip] stream not found")
		_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 404, "Not Found", nil))
		return
	}

	offerBody := req.Body()
	if len(offerBody) == 0 {
		_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
		return
	}

	localIP := outboundIP(req.Source())

	conn, err := newServerConn(offerBody, localIP)
	if err != nil {
		log.Error().Err(err).Msg("[sip] create conn")
		_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 500, "Internal Server Error", nil))
		return
	}
	conn.RemoteAddr = req.Source()

	if err := stream.AddConsumer(conn); err != nil {
		log.Warn().Err(err).Msg("[sip] add consumer")
		_ = conn.Stop()
		_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 488, "Not Acceptable Here", nil))
		return
	}

	stream.AddProducer(conn)

	callID := ""
	if cid := req.CallID(); cid != nil {
		callID = string(*cid)
	}
	conn.Source = callID
	calls.Store(callID, &callEntry{stream: stream, conn: conn})

	answerSDP, err := conn.answerSDP()
	if err != nil {
		log.Error().Err(err).Msg("[sip] answer SDP")
		stream.RemoveConsumer(conn)
		stream.RemoveProducer(conn)
		calls.Delete(callID)
		_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 500, "Internal Server Error", nil))
		return
	}

	ct := sipmsg.ContentTypeHeader("application/sdp")
	res := sipmsg.NewResponseFromRequest(req, 200, "OK", answerSDP)
	res.AppendHeader(&ct)
	if err := tx.Respond(res); err != nil {
		log.Error().Err(err).Msg("[sip] respond 200 OK")
	}

	log.Info().Str("call_id", callID).Str("stream", streamName).Msg("[sip] incoming call")
}

func handleBye(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	// First, check if this BYE belongs to an outgoing (client) dialog.
	if clientDialogs != nil {
		if err := clientDialogs.ReadBye(req, tx); err == nil {
			log.Info().Str("call_id", byeCallID(req)).Msg("[sip] outgoing call ended (BYE)")
			return
		}
	}

	// Otherwise, handle as incoming (server) dialog.
	callID := byeCallID(req)
	if v, ok := calls.LoadAndDelete(callID); ok {
		entry := v.(*callEntry)
		entry.stream.RemoveConsumer(entry.conn)
		entry.stream.RemoveProducer(entry.conn)
		log.Info().Str("call_id", callID).Msg("[sip] incoming call ended (BYE)")
	}

	_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 200, "OK", nil))
}

func byeCallID(req *sipmsg.Request) string {
	if cid := req.CallID(); cid != nil {
		return string(*cid)
	}
	return ""
}

// ─── Client (UAC) ──────────────────────────────────────────────────────────────

// sipProducerHandler is called by the streams engine when a sip:// URL is used
// as a source.
//
// URL format:  sip://username:password@pbx-host:5060/callee
//
//   - username / password  are credentials for digest authentication with the PBX.
//   - pbx-host:port        is the PBX address (port defaults to 5060).
//   - callee               is the extension being called (path after the host).
//     If omitted, username is used as the callee.
func sipProducerHandler(rawURL string) (core.Producer, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, fmt.Errorf("parse SIP URL: %w", err)
	}

	username := u.User.Username()
	password, _ := u.User.Password()

	pbxHost := u.Host
	if pbxHost == "" {
		return nil, fmt.Errorf("SIP URL missing host: %s", rawURL)
	}

	// Resolve host and port separately; default to 5060.
	pbxIP, pbxPortStr, err := net.SplitHostPort(pbxHost)
	if err != nil {
		pbxIP = pbxHost
		pbxPortStr = "5060"
	}
	pbxPort, _ := strconv.Atoi(pbxPortStr)

	callee := strings.TrimPrefix(u.Path, "/")
	if callee == "" {
		callee = username
	}
	if callee == "" {
		return nil, fmt.Errorf("SIP URL missing callee (path): %s", rawURL)
	}

	// Determine which local IP faces the PBX.
	localIP := outboundIP(net.JoinHostPort(pbxIP, pbxPortStr))

	// Bind a UDP socket now so we know our RTP port before sending the offer.
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

	// Dial with a 30-second timeout so the caller's reconnect timer doesn't fire
	// before we know whether the PBX answered.
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	dialog, err := clientDialogs.Invite(ctx, recipient, offerSDP, &ct)
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

	// Parse the answer SDP to learn the PBX's chosen codec and RTP address.
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

	codec := codecs[0]

	conn := &ClientConn{
		udpConn:    udpConn,
		remoteAddr: remoteAddr,
		codec:      codec,
		dialog:     dialog,
	}
	conn.ID = core.NewID()
	conn.FormatName = "sip"
	conn.Protocol = "udp"
	conn.Source = rawURL
	conn.RemoteAddr = remoteAddr.String()
	// Transport lets core.Connection.Stop() close the socket.
	conn.Transport = udpConn

	// From the producer perspective:
	//   recvonly – PBX sends us audio; we provide it to downstream consumers.
	//   sendonly – we accept audio from consumers and forward it to the PBX.
	conn.Medias = []*core.Media{
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}},
		{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}},
	}

	log.Info().
		Str("callee", callee).
		Str("codec", codec.Name).
		Str("remote_rtp", remoteAddr.String()).
		Msg("[sip] outgoing call established")

	return conn, nil
}

// buildOfferSDP creates an SDP offer advertising both PCMU (PT 0) and PCMA (PT 8).
// The PBX picks one; we handle either in the answer.
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
				Media:  "audio",
				Port:   sdp.RangedPort{Value: localPort},
				Protos: []string{"RTP", "AVP"},
				// 0: PCMU, 8: PCMA, 14: MP3 (Static)
				// 96-111: Dynamic assignments for Opus, AAC, L16, etc.
				Formats: []string{"0", "8", "96"},
			},
			Attributes: []sdp.Attribute{
				{Key: "rtpmap", Value: "0 PCMU/8000"},
				{Key: "rtpmap", Value: "8 PCMA/8000"},
				{Key: "rtpmap", Value: "96 OPUS/48000/2"},
				{Key: "sendrecv"},
			},
		}},
	}

	b, _ := sd.Marshal()
	return b
}

// parseAudioAddr extracts the remote RTP address and all PCMA/PCMU codecs from an SDP body.
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

// ClientConn is a SIP media session initiated by go2rtc (UAC role).
// It implements core.Producer: the PBX call is the source of audio.
// It also implements core.Consumer.AddTrack for the sendonly backchannel.
type ClientConn struct {
	core.Connection

	udpConn     *net.UDPConn
	remoteAddr  *net.UDPAddr
	codec       *core.Codec
	recvFromPBX *core.Receiver // fed by readLoop, given to downstream consumers
	dialog      *sipgo.DialogClientSession
}

// GetMedias implements core.Producer.
func (c *ClientConn) GetMedias() []*core.Media {
	return c.Medias
}

// GetTrack implements core.Producer.
// Called for the recvonly media: returns a Receiver fed by inbound RTP from the PBX.
func (c *ClientConn) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	for _, recv := range c.Receivers {
		if recv.Codec == codec {
			return recv, nil
		}
	}
	recv := core.NewReceiver(media, codec)
	c.recvFromPBX = recv
	c.Receivers = append(c.Receivers, recv)
	go c.readLoop()
	return recv, nil
}

// AddTrack implements core.Consumer (backchannel).
// Called for the sendonly media: wraps a Sender that forwards RTP to the PBX.
func (c *ClientConn) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	sender := core.NewSender(media, codec)
	sender.Handler = c.sendRTP
	sender.WithParent(track)
	sender.Start()
	c.Senders = append(c.Senders, sender)
	return nil
}

// Start implements core.Producer.
// Blocks until the dialog ends so the streams engine knows when to reconnect.
func (c *ClientConn) Start() error {
	<-c.dialog.Context().Done()
	return context.Cause(c.dialog.Context())
}

// Stop sends a BYE to the PBX (no-op if the PBX already hung up) then
// closes the UDP socket and cleans up tracks.
func (c *ClientConn) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.dialog.Bye(ctx)
	return c.Connection.Stop()
}

func (c *ClientConn) sendRTP(packet *core.Packet) {
	pkt := *packet
	pkt.Header.PayloadType = c.codec.PayloadType
	buf, err := pkt.Marshal()
	if err != nil {
		return
	}
	if n, err := c.udpConn.WriteTo(buf, c.remoteAddr); err == nil {
		c.Send += n
	}
}

func (c *ClientConn) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, _, err := c.udpConn.ReadFrom(buf)
		if err != nil {
			return // socket closed, exit cleanly
		}
		if c.recvFromPBX == nil {
			continue
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		c.Recv += n
		c.recvFromPBX.Input(&pkt)
	}
}

// ─── Server-side Conn (UAS) ────────────────────────────────────────────────────

// Conn is a server-side SIP media session (phone called us, UAS role).
// It implements core.Consumer so go2rtc pushes stream audio to the SIP peer.
// When two-way audio is negotiated it also acts as core.Producer (backchannel).
type Conn struct {
	core.Connection

	udpConn    *net.UDPConn
	remoteAddr *net.UDPAddr
	codecs     []*core.Codec
	codec      *core.Codec // negotiated codec

	receivers map[byte]*core.Receiver
}

func newServerConn(offerSDP []byte, localIP net.IP) (*Conn, error) {
	remoteAddr, codecs, err := parseAudioAddr(offerSDP)
	if err != nil {
		return nil, err
	}

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: localIP, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("bind RTP socket: %w", err)
	}

	conn := &Conn{
		udpConn:    udpConn,
		remoteAddr: remoteAddr,
		codecs:     codecs,
		receivers:  make(map[byte]*core.Receiver),
	}
	conn.ID = core.NewID()
	conn.FormatName = "sip"
	conn.Protocol = "udp"
	conn.Transport = udpConn

	// Consumer perspective:
	//   sendonly – consume stream audio and forward to the SIP peer.
	//   recvonly – accept audio from the SIP peer for the backchannel.
	conn.Medias = []*core.Media{
		{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: codecs},
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: codecs},
	}

	return conn, nil
}

// GetMedias implements core.Consumer.
func (c *Conn) GetMedias() []*core.Media {
	return c.Medias
}

// AddTrack implements core.Consumer — outgoing audio to the SIP peer.
func (c *Conn) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	c.codec = codec // use this codec for answer and outgoing RTP
	sender := core.NewSender(media, codec)
	sender.Handler = c.sendRTP
	sender.WithParent(track)
	sender.Start()
	c.Senders = append(c.Senders, sender)
	return nil
}

func (c *Conn) sendRTP(packet *core.Packet) {
	if c.codec == nil {
		return
	}
	pkt := *packet
	pkt.Header.PayloadType = c.codec.PayloadType
	buf, err := pkt.Marshal()
	if err != nil {
		return
	}
	if n, err := c.udpConn.WriteTo(buf, c.remoteAddr); err == nil {
		c.Send += n
	}
}

// GetTrack implements core.Producer (backchannel) — inbound audio from the SIP peer.
func (c *Conn) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	if recv, ok := c.receivers[codec.PayloadType]; ok {
		return recv, nil
	}

	recv := core.NewReceiver(media, codec)
	c.receivers[codec.PayloadType] = recv
	c.Receivers = append(c.Receivers, recv)

	if len(c.receivers) == 1 {
		go c.readLoop()
	}

	return recv, nil
}

// Start implements core.Producer — no-op; readLoop started inside GetTrack.
func (c *Conn) Start() error {
	return nil
}

// answerSDP builds the SDP for the 200 OK response.
func (c *Conn) answerSDP() ([]byte, error) {
	localUDP := c.udpConn.LocalAddr().(*net.UDPAddr)

	ipStr := "0.0.0.0"
	if localUDP.IP != nil && !localUDP.IP.IsUnspecified() {
		ipStr = localUDP.IP.String()
	}

	codec := c.codec
	if codec == nil {
		codec = c.codecs[0]
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
				Port:    sdp.RangedPort{Value: localUDP.Port},
				Protos:  []string{"RTP", "AVP"},
				Formats: []string{strconv.Itoa(int(codec.PayloadType))},
			},
			Attributes: []sdp.Attribute{
				{
					Key:   "rtpmap",
					Value: codecRTPMap(*codec),
				},
				{Key: "sendrecv"},
			},
		}},
	}

	return sd.Marshal()
}

func codecRTPMap(codec core.Codec) string {
	if codec.Channels > 1 {
		return fmt.Sprintf("%d %s/%d/%d", codec.PayloadType, codec.Name, codec.ClockRate, codec.Channels)
	}
	return fmt.Sprintf("%d %s/%d", codec.PayloadType, codec.Name, codec.ClockRate)
}

func (c *Conn) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, _, err := c.udpConn.ReadFrom(buf)
		if err != nil {
			return
		}

		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}

		if recv, ok := c.receivers[pkt.PayloadType]; ok {
			c.Recv += n
			recv.Input(&pkt)
		}
	}
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
