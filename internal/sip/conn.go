package sip

import (
	"net"

	"github.com/AlexxIT/go2rtc/pkg/core"
	"github.com/pion/rtp"
)

type SIPConn struct {
	core.Connection

	udpConn    *net.UDPConn
	remoteAddr *net.UDPAddr
	codec      *core.Codec

	receivers map[byte]*core.Receiver
}

func (c *SIPConn) init(rawURL string) {
	c.ID = core.NewID()
	c.FormatName = "sip"
	c.Protocol = "udp"
	c.Source = rawURL
	c.Transport = c.udpConn
	c.receivers = make(map[byte]*core.Receiver)
}

func (c *SIPConn) GetTrack(media *core.Media, codec *core.Codec) (*core.Receiver, error) {
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

func (c *SIPConn) AddTrack(media *core.Media, codec *core.Codec, track *core.Receiver) error {
	c.codec = codec
	sender := core.NewSender(media, codec)
	sender.Handler = c.sendRTP
	sender.WithParent(track)
	sender.Start()
	c.Senders = append(c.Senders, sender)
	return nil
}

func (c *SIPConn) sendRTP(packet *core.Packet) {
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

func (c *SIPConn) readLoop() {
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
