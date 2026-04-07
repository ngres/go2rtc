package sip

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"sync"

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

// callEntry tracks an active SIP call for BYE handling.
type callEntry struct {
	stream *streams.Stream
	conn   *Conn
}

// calls maps SIP Call-ID -> *callEntry.
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

	if conf.Mod.Listen == "" {
		return
	}

	go runServer(conf.Mod.Listen)
}

func runServer(listen string) {
	ua, err := sipgo.NewUA(sipgo.WithUserAgent(app.UserAgent))
	if err != nil {
		log.Error().Err(err).Msg("[sip] new UA")
		return
	}

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
	// Send 100 Trying immediately to stop retransmissions.
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

	// Determine our local IP towards the caller for RTP binding.
	localIP := outboundIP(req.Source())

	conn, err := newConn(offerBody, localIP)
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

	callID := ""
	if cid := req.CallID(); cid != nil {
		callID = string(*cid)
	}
	calls.Store(callID, &callEntry{stream: stream, conn: conn})

	answerSDP, err := conn.answerSDP()
	if err != nil {
		log.Error().Err(err).Msg("[sip] answer SDP")
		stream.RemoveConsumer(conn)
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

	log.Info().Str("call_id", callID).Str("stream", streamName).Msg("[sip] call started")
}

func handleBye(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	callID := ""
	if cid := req.CallID(); cid != nil {
		callID = string(*cid)
	}

	if v, ok := calls.LoadAndDelete(callID); ok {
		entry := v.(*callEntry)
		entry.stream.RemoveConsumer(entry.conn)
		log.Info().Str("call_id", callID).Msg("[sip] call ended (BYE)")
	}

	_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 200, "OK", nil))
}

// outboundIP returns the local IP that would be used to reach remoteAddr.
// This is needed to advertise the correct IP in the SDP answer.
func outboundIP(remoteAddr string) net.IP {
	host, _, _ := net.SplitHostPort(remoteAddr)
	if host == "" {
		return net.IPv4zero
	}
	// Dial UDP (no packets sent) to discover the outbound interface.
	conn, err := net.Dial("udp", net.JoinHostPort(host, "1"))
	if err != nil {
		return net.IPv4zero
	}
	defer conn.Close()
	return conn.LocalAddr().(*net.UDPAddr).IP
}

// Conn is a SIP media session bridging the SIP peer and a go2rtc stream.
//
// It implements core.Consumer so go2rtc pushes the stream's audio out to
// the SIP peer via RTP.  When two-way audio is negotiated it also implements
// core.Producer so RTP arriving from the SIP peer is injected back into the
// stream as backchannel audio (e.g. to the doorbell speaker).
type Conn struct {
	core.Connection

	udpConn    *net.UDPConn
	remoteAddr *net.UDPAddr
	codec      *core.Codec
	backRecv   *core.Receiver
}

func newConn(offerSDP []byte, localIP net.IP) (*Conn, error) {
	var sd sdp.SessionDescription
	if err := sd.Unmarshal(offerSDP); err != nil {
		return nil, fmt.Errorf("unmarshal offer SDP: %w", err)
	}

	// Locate the audio media section.
	var audioMD *sdp.MediaDescription
	for _, md := range sd.MediaDescriptions {
		if md.MediaName.Media == "audio" {
			audioMD = md
			break
		}
	}
	if audioMD == nil {
		return nil, fmt.Errorf("no audio media in SDP offer")
	}

	// Remote IP: media-level c= overrides session-level c=.
	remoteIP := ""
	if sd.ConnectionInformation != nil && sd.ConnectionInformation.Address != nil {
		remoteIP = sd.ConnectionInformation.Address.Address
	}
	if audioMD.ConnectionInformation != nil && audioMD.ConnectionInformation.Address != nil {
		remoteIP = audioMD.ConnectionInformation.Address.Address
	}

	remotePort := audioMD.MediaName.Port.Value

	// Select first PCMA or PCMU codec offered (telephony standard codecs).
	var codec *core.Codec
	for _, format := range audioMD.MediaName.Formats {
		c := core.UnmarshalCodec(audioMD, format)
		if c.Name == core.CodecPCMA || c.Name == core.CodecPCMU {
			codec = c
			break
		}
	}
	if codec == nil {
		return nil, fmt.Errorf("no PCMA/PCMU codec in SDP offer (got %v)", audioMD.MediaName.Formats)
	}

	remoteAddr := &net.UDPAddr{
		IP:   net.ParseIP(remoteIP),
		Port: remotePort,
	}

	// Bind a local UDP port for RTP exchange on the outbound interface.
	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: localIP, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("bind RTP socket: %w", err)
	}

	conn := &Conn{
		udpConn:    udpConn,
		remoteAddr: remoteAddr,
		codec:      codec,
	}
	conn.ID = core.NewID()
	conn.FormatName = "sip"
	conn.Protocol = "udp"
	// Setting Transport lets core.Connection.Stop() close the UDP socket.
	conn.Transport = udpConn

	// Advertise to go2rtc what this session can do:
	//   sendonly – we want audio from the producer to forward to the PBX.
	//   recvonly – we can provide audio received from the PBX (backchannel).
	conn.Medias = []*core.Media{
		{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{codec}},
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{codec}},
	}

	return conn, nil
}

// GetMedias implements core.Consumer.
func (c *Conn) GetMedias() []*core.Media {
	return c.Medias
}

// AddTrack implements core.Consumer.
// Called by the streams engine when a matching producer track is found.
// We wrap the incoming receiver in a Sender that writes RTP to the PBX.
func (c *Conn) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	sender := core.NewSender(media, codec)
	sender.Handler = c.sendRTP
	sender.WithParent(track)
	sender.Start()
	c.Senders = append(c.Senders, sender)
	return nil
}

// sendRTP serialises a single RTP packet and sends it to the SIP peer.
func (c *Conn) sendRTP(packet *core.Packet) {
	// Shallow-copy so we can rewrite the payload type without affecting
	// other consumers reading the same packet.
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

// GetTrack implements core.Producer (backchannel).
// Called by the streams engine when the upstream producer (e.g. Ring doorbell)
// has a sendonly backchannel.  We return a Receiver fed by incoming UDP packets.
func (c *Conn) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	// Deduplicate: return existing receiver if one already exists for this codec.
	for _, recv := range c.Receivers {
		if recv.Codec == codec {
			return recv, nil
		}
	}

	recv := core.NewReceiver(media, codec)
	c.backRecv = recv
	c.Receivers = append(c.Receivers, recv)

	// Start reading inbound RTP from the SIP peer.
	go c.readLoop()

	return recv, nil
}

// Start implements core.Producer.
// The read loop is launched in GetTrack; nothing more to do here.
func (c *Conn) Start() error {
	return nil
}

// answerSDP builds the SDP body for the 200 OK response.
// It advertises our local UDP port and the single negotiated codec.
func (c *Conn) answerSDP() ([]byte, error) {
	localUDP := c.udpConn.LocalAddr().(*net.UDPAddr)

	ipStr := "0.0.0.0"
	if localUDP.IP != nil && !localUDP.IP.IsUnspecified() {
		ipStr = localUDP.IP.String()
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
		TimeDescriptions: []sdp.TimeDescription{
			{Timing: sdp.Timing{}},
		},
		MediaDescriptions: []*sdp.MediaDescription{
			{
				MediaName: sdp.MediaName{
					Media:   "audio",
					Port:    sdp.RangedPort{Value: localUDP.Port},
					Protos:  []string{"RTP", "AVP"},
					Formats: []string{strconv.Itoa(int(c.codec.PayloadType))},
				},
				Attributes: []sdp.Attribute{
					{
						Key:   "rtpmap",
						Value: fmt.Sprintf("%d %s/%d", c.codec.PayloadType, c.codec.Name, c.codec.ClockRate),
					},
					{Key: "sendrecv"},
				},
			},
		},
	}

	return sd.Marshal()
}

// readLoop receives UDP datagrams on the RTP socket and feeds them into the
// backchannel receiver so the upstream producer (doorbell) can play them.
// It exits when the UDP socket is closed (i.e. Stop() is called).
func (c *Conn) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, _, err := c.udpConn.ReadFrom(buf)
		if err != nil {
			return // socket closed, exit cleanly
		}
		if c.backRecv == nil {
			continue
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		c.Recv += n
		c.backRecv.Input(&pkt)
	}
}
