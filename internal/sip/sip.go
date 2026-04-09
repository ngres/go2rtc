package sip

import (
	"context"
	"net"
	"strconv"
	"strings"
	"sync"

	"github.com/AlexxIT/go2rtc/internal/app"
	"github.com/AlexxIT/go2rtc/internal/streams"
	"github.com/emiago/sipgo"
	sipmsg "github.com/emiago/sipgo/sip"
	"github.com/rs/zerolog"
)

var log zerolog.Logger

// Module-level SIP stack shared by server and client.
var (
	ua            *sipgo.UserAgent
	sipClient     *sipgo.Client
	clientDialogs *sipgo.DialogClientCache
	routing       map[string]string
)

// callEntry tracks an active server-side SIP session (phone calling us).
type callEntry struct {
	stream *streams.Stream
	conn   *UASConn
}

// calls maps SIP Call-ID → *callEntry for server-side sessions.
var calls sync.Map

// registrations maps SIP username → contact host:port for phones registered
// with the internal SIP server. Used to call internal phones via sip:callee.
var registrations sync.Map

func Init() {
	var conf struct {
		Mod struct {
			Listen  string                 `yaml:"listen" json:"listen"` // Port for the internal SIP server to listen on, e.g. ":5060". Empty to disable server mode.
			Trunks  map[string]TrunkConfig `yaml:"trunks" json:"trunks"`
			Routing map[string][]string    `yaml:"routing" json:"routing"`
		} `yaml:"sip"`
	}

	conf.Mod.Listen = ":5060"

	app.LoadConfig(&conf)
	app.Info["sip"] = conf.Mod

	log = app.GetLogger("sip")

	// Create the shared User Agent used for both server and client.
	var err error
	ua, err = sipgo.NewUA(sipgo.WithUserAgent("go2rtc"))
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

	// Start persistent registrations for all configured trunks.
	for name, config := range conf.Mod.Trunks {
		t, err := newTrunk(name, config)
		if err != nil {
			log.Error().Err(err).Str("trunk", name).Msg("[sip] init trunk")
			continue
		}
		trunks[name] = t
	}

	// Register handler so streams can use  sip:trunk/callee  or  sip:callee  as a source.
	streams.HandleFunc("sip", sipProducerHandler)

	// Register consumer handler so publish destinations like  sip:trunk/callee  or
	// sip:callee  forward stream audio to a remote party.
	streams.HandleConsumerFunc("sip", sipConsumerHandler)

	if conf.Mod.Listen == "" {
		return
	}

	routing = map[string]string{}
	for source, v := range conf.Mod.Routing {
		for _, username := range v {
			routing[strings.ToLower(username)] = source
		}
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

	srv.OnRegister(handleRegister)
	srv.OnInvite(handleInvite)
	srv.OnBye(handleBye)
	srv.OnAck(func(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
		// ACK completes the 3-way handshake; media is already flowing.
	})
	srv.OnOptions(func(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
		_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 200, "OK", nil))
	})

	if listen == "" {
		return
	}

	log.Info().Str("addr", listen).Msg("[sip] listen")

	if err := srv.ListenAndServe(context.Background(), "udp", listen); err != nil {
		log.Error().Err(err).Msg("[sip] serve")
	}
}

func handleRegister(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	user := req.From().Address.User
	if user == "" {
		_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 400, "Bad Request", nil))
		return
	}
	if contact := req.Contact(); contact != nil {
		addr := &contact.Address
		hostPort := net.JoinHostPort(addr.Host, strconv.Itoa(addr.Port))
		registrations.Store(user, hostPort)
		log.Info().Str("user", user).Str("contact", hostPort).Msg("[sip] phone registered")
	}
	res := sipmsg.NewResponseFromRequest(req, 200, "OK", nil)
	res.AppendHeader(sipmsg.NewHeader("Expires", strconv.Itoa(registerExpiry)))
	_ = tx.Respond(res)
}

func handleInvite(req *sipmsg.Request, tx sipmsg.ServerTransaction) {
	_ = tx.Respond(sipmsg.NewResponseFromRequest(req, 100, "Trying", nil))

	username := strings.ToLower(req.To().Address.User)
	streamName, ok := routing[username]
	if !ok {
		streamName = username
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
