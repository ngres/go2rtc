package sip

import (
	"fmt"
	"net"
	"strconv"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/sdp/v3"
)

// UASConn is a server-side SIP media session (phone called us, UAS role).
// It implements core.Consumer so go2rtc pushes stream audio to the SIP peer.
// When two-way audio is negotiated it also acts as core.Producer (backchannel).
type UASConn struct {
	SIPConn
	codecs []*core.Codec
}

func newServerConn(offerSDP []byte, localIP net.IP) (*UASConn, error) {
	remoteAddr, codecs, err := parseAudioAddr(offerSDP)
	if err != nil {
		return nil, err
	}

	udpConn, err := net.ListenUDP("udp", &net.UDPAddr{IP: localIP, Port: 0})
	if err != nil {
		return nil, fmt.Errorf("bind RTP socket: %w", err)
	}

	conn := &UASConn{
		SIPConn: SIPConn{
			udpConn:    udpConn,
			remoteAddr: remoteAddr,
		},
		codecs: codecs,
	}
	conn.init("")

	// Consumer perspective:
	//   sendonly – consume stream audio and forward to the SIP peer.
	//   recvonly – accept audio from the SIP peer for the backchannel.
	conn.Medias = []*core.Media{
		{Kind: core.KindAudio, Direction: core.DirectionSendonly, Codecs: codecs},
		{Kind: core.KindAudio, Direction: core.DirectionRecvonly, Codecs: codecs},
	}

	return conn, nil
}

// Start implements core.Producer — no-op; readLoop is started inside GetTrack.
func (c *UASConn) Start() error {
	return nil
}

// answerSDP builds the SDP for the 200 OK response.
func (c *UASConn) answerSDP() ([]byte, error) {
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
					Value: fmt.Sprintf("%d %s", codec.PayloadType, codec),
				},
				{Key: core.DirectionSendRecv},
			},
		}},
	}

	return sd.Marshal()
}
