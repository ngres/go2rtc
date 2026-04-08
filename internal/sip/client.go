package sip

import (
	"context"
	"net"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/emiago/sipgo"
	"github.com/pion/rtp"
)

// UACConn is a SIP media session initiated by go2rtc (UAC role).
//
// It serves dual roles depending on how it is registered with the streams engine:
//   - As a core.Producer (sip:// source): the remote party is the audio source;
//     GetTrack returns inbound RTP, AddTrack forwards outbound RTP (backchannel).
//   - As a core.Consumer (sip:// publish): stream audio is forwarded to the remote
//     party via AddTrack; GetTrack provides the backchannel back into the stream.
//
// The two roles differ only in the Media directions set by their respective handlers.
type UACConn struct {
	core.Connection

	udpConn    *net.UDPConn
	remoteAddr *net.UDPAddr
	codec      *core.Codec
	dialog     *sipgo.DialogClientSession

	// recvByPT maps RTP payload type → Receiver for routing inbound audio.
	recvByPT map[byte]*core.Receiver
}

// GetTrack implements core.Producer.
// Returns a Receiver fed by inbound RTP from the remote party. Starts the
// read loop on the first call.
func (c *UACConn) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
	if recv, ok := c.recvByPT[codec.PayloadType]; ok {
		return recv, nil
	}
	recv := core.NewReceiver(media, codec)
	c.recvByPT[codec.PayloadType] = recv
	c.Receivers = append(c.Receivers, recv)
	if len(c.recvByPT) == 1 {
		go c.readLoop()
	}
	return recv, nil
}

// AddTrack implements core.Consumer.
// Wraps a Sender that forwards RTP packets to the remote party.
func (c *UACConn) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	sender := core.NewSender(media, codec)
	sender.Handler = c.sendRTP
	sender.WithParent(track)
	sender.Start()
	c.Senders = append(c.Senders, sender)
	return nil
}

// Start implements core.Producer.
// Blocks until the dialog ends so the streams engine knows when to reconnect.
func (c *UACConn) Start() error {
	<-c.dialog.Context().Done()
	return context.Cause(c.dialog.Context())
}

// Stop sends a BYE to the remote party and releases local resources.
func (c *UACConn) Stop() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = c.dialog.Bye(ctx)
	return c.Connection.Stop()
}

func (c *UACConn) sendRTP(packet *core.Packet) {
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

func (c *UACConn) readLoop() {
	buf := make([]byte, 4096)
	for {
		n, _, err := c.udpConn.ReadFrom(buf)
		if err != nil {
			return // socket closed, exit cleanly
		}
		var pkt rtp.Packet
		if err := pkt.Unmarshal(buf[:n]); err != nil {
			continue
		}
		if recv, ok := c.recvByPT[pkt.PayloadType]; ok {
			c.Recv += n
			recv.Input(&pkt)
		}
	}
}

// newUACConn populates a UACConn from a completed sipSession.
// Callers must set Medias before use.
func newUACConn(sess *sipSession, rawURL string) *UACConn {
	conn := &UACConn{
		udpConn:    sess.udpConn,
		remoteAddr: sess.remoteAddr,
		codec:      sess.codec,
		dialog:     sess.dialog,
		recvByPT:   make(map[byte]*core.Receiver),
	}
	conn.ID = core.NewID()
	conn.FormatName = "sip"
	conn.Protocol = "udp"
	conn.Source = rawURL
	conn.RemoteAddr = sess.remoteAddr.String()
	conn.Transport = sess.udpConn
	return conn
}

// sipProducerHandler is called by the streams engine when a sip:// URL is used as a source.
//
// URL format:  sip://username:password@host:5060/callee
// Short form:  sip:callee  (uses sip.pbx config for host/port/credentials)
func sipProducerHandler(rawURL string) (core.Producer, error) {
	sess, err := dialSIP(rawURL)
	if err != nil {
		return nil, err
	}

	conn := newUACConn(sess, rawURL)
	conn.Medias = []*core.Media{
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{sess.codec}},
		{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{sess.codec}},
	}

	log.Info().
		Str("codec", sess.codec.Name).
		Str("remote_rtp", sess.remoteAddr.String()).
		Msg("[sip] outgoing call established")

	return conn, nil
}

// sipConsumerHandler is called by streams.Publish when a sip:// destination is configured.
// It dials the remote party and forwards stream audio to it, routing the reply audio
// back as a backchannel.
//
// URL format:  sip://username:password@host:5060/callee
// Short form:  sip:callee  (uses sip.pbx config for host/port/credentials)
func sipConsumerHandler(rawURL string) (core.Consumer, func(), error) {
	sess, err := dialSIP(rawURL)
	if err != nil {
		return nil, nil, err
	}

	conn := newUACConn(sess, rawURL)
	conn.Medias = []*core.Media{
		{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: []*core.Codec{sess.codec}},
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: []*core.Codec{sess.codec}},
	}

	log.Info().
		Str("codec", sess.codec.Name).
		Str("remote_rtp", sess.remoteAddr.String()).
		Msg("[sip] PBX consumer established")

	// run blocks until the dialog ends; streams.Publish calls RemoveConsumer → Stop after it returns.
	run := func() { <-conn.dialog.Context().Done() }

	return conn, run, nil
}
