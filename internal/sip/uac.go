package sip

import (
	"context"
	"fmt"
	"time"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/emiago/sipgo"
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
	SIPConn
	dialog *sipgo.DialogClientSession
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
	return c.SIPConn.Stop()
}

// newUACConn populates a UACConn from a completed sipSession.
// Callers must set Medias before use.
func newUACConn(sess *sipSession, rawURL string) *UACConn {
	conn := &UACConn{
		SIPConn: SIPConn{
			udpConn:    sess.udpConn,
			remoteAddr: sess.remoteAddr,
			codec:      sess.codec,
		},
		dialog: sess.dialog,
	}
	conn.init(rawURL)
	conn.RemoteAddr = sess.remoteAddr.String()
	return conn
}

// sipProducerHandler is called by the streams engine when a sip: URL is used as a source.
//
//	sip:trunk/callee  — dial callee via the named trunk
//	sip:callee        — call a phone registered with the internal SIP server
func sipProducerHandler(rawURL string) (core.Producer, error) {
	sess, err := dialFromURL(rawURL)
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

// sipConsumerHandler is called by streams.Publish when a sip: destination is configured.
// It dials the remote party and forwards stream audio to it, routing the reply audio
// back as a backchannel.
//
//	sip:trunk/callee  — call callee via the named trunk
//	sip:callee        — call a phone registered with the internal SIP server
func sipConsumerHandler(rawURL string) (core.Consumer, func(), error) {
	sess, err := dialFromURL(rawURL)
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
		Msg("[sip] consumer established")

	// run blocks until the dialog ends; streams.Publish calls RemoveConsumer → Stop after it returns.
	run := func() {
		<-conn.dialog.Context().Done()
	}

	return conn, run, nil
}

// dialFromURL resolves trunk name and callee from a sip: URL and places the call.
func dialFromURL(rawURL string) (*sipSession, error) {
	trunkName, callee, err := parseSIPURL(rawURL)
	if err != nil {
		return nil, err
	}

	if trunkName != "" {
		t, ok := trunks[trunkName]
		if !ok {
			return nil, fmt.Errorf("SIP trunk %q not configured", trunkName)
		}
		return t.dialVia(callee)
	}

	if callee != "" {
		return dialRegistered(callee)
	}

	// Fallback: pass the raw URL directly to dialSIP (legacy full-URL form).
	return dialSIP(rawURL, &ThrunkConfig{})
}
