package main

import (
	"context"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/ghettovoice/gosip/sip"
	"github.com/ghettovoice/gosip/sip/parser"
	"github.com/ghettovoice/gosip/transaction"
	"github.com/google/uuid"
	"github.com/pion/rtp"
	"github.com/pion/sdp/v3"
	"github.com/pion/webrtc/v4/pkg/media/oggwriter"
	"github.com/subiz/log"
	"github.com/thanhpk/randstr"
)

var asteriskIp = "27.71.226.145"
var privateIp = "192.168.5.131"

// UserAgent .
type UserAgent struct {
	stack    *SipStack
	iss      sync.Map /*Invite Session*/
	register *Register

	username, password string
	profile            *Profile
	Udp                *UDPConn
	Session            *Session
}

// NewUserAgent .
func NewUserAgent(stack *SipStack, username, password string) *UserAgent {
	ua := &UserAgent{
		stack:    stack,
		iss:      sync.Map{},
		username: username,
		password: password,
	}
	stack.OnRequest(sip.INVITE, ua.handleInvite)
	stack.OnRequest(sip.ACK, ua.handleACK)
	stack.OnRequest(sip.CANCEL, ua.handleCancel)
	stack.OnRequest(sip.BYE, ua.handleBye)
	stack.OnRequest(sip.UPDATE, ua.handleUpdate)
	stack.OnRequest(sip.OPTIONS, ua.handleOptions)
	return ua
}

func isConnectionClosed(err error) bool {
	if err == nil {
		return false
	}
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		return true
	}

	if strings.Contains(err.Error(), "io: read/write on closed pipe") {
		return true
	}

	return strings.Contains(err.Error(), "use of closed network connection")
}

func (me *UserAgent) Invite(callid string, target sip.Uri, recipient sip.SipUri) error {
	udp, port := NewUDPConn()
	me.Udp = udp

	sdp := BuildLocalSdp(privateIp, port)

	profile := me.profile
	from := &sip.Address{
		DisplayName: sip.String{Str: profile.DisplayName},
		Uri:         profile.URI,
		Params:      sip.NewParams().Add("tag", sip.String{Str: randstr.String(8)}),
	}

	contact := profile.Contact()
	to := &sip.Address{Uri: target}

	ci := sip.CallID(callid)
	request, err := buildRequest(sip.INVITE, from, to, contact, recipient, profile.Routes, &ci)
	if err != nil {
		return err
	}

	(*request).SetBody(sdp, true)
	contentType := sip.ContentType("application/sdp")
	(*request).AppendHeader(&contentType)

	var authorizer sip.Authorizer
	if profile.AuthInfo != nil {
		authorizer = &sip.DefaultAuthorizer{User: sip.String{Str: profile.AuthInfo.AuthUser}, Password: sip.String{Str: profile.AuthInfo.Password}}
	}

	resp, err := me.RequestWithContext(context.TODO(), *request, authorizer, false, 1)
	if err != nil {
		return log.EServer(err)
	}
	if resp != nil {
		stateCode := resp.StatusCode()
		log.Info("", "INVITE: resp %d => %s", stateCode, resp.String())
		return log.EServer(nil, log.M{"reason": resp.String(), "msg": "Invite session is unsuccessful", "code": stateCode})
	}

	return nil
}

func buildRequest(
	method sip.RequestMethod,
	from *sip.Address,
	to *sip.Address,
	contact *sip.Address,
	recipient sip.SipUri,
	routes []sip.Uri,
	callID *sip.CallID) (*sip.Request, error) {
	builder := sip.NewRequestBuilder()
	builder.SetMethod(method)
	builder.SetTransport("UDP")
	builder.SetFrom(from)
	builder.SetTo(to)
	builder.SetContact(contact)
	builder.SetRecipient(recipient.Clone())

	if len(routes) > 0 {
		builder.SetRoutes(routes)
	}

	if callID != nil {
		builder.SetCallID(callID)
	}

	req, err := builder.Build()
	if err != nil {
		return nil, err
	}
	return &req, nil
}

func (ua *UserAgent) handleInviteState(is *Session, request *sip.Request, response *sip.Response, state Status, tx *sip.Transaction) {
	if request != nil && *request != nil {
		is.StoreRequest(*request)
	}

	if response != nil && *response != nil {
		is.StoreResponse(*response)
	}

	if tx != nil {
		is.StoreTransaction(*tx)
	}
	is.SetState(state)
	// callid := is.CallID().Value()
	// log.Info("subiz", fmt.Sprintf("#########\n######### InviteStateHandler: state => %v, type => %s, id => %s\n\n", state, is.Direction(), callid))

	switch state {
	case EarlyMedia, Confirmed:
		ua.handleCallMedia(is)
	case Canceled:
		fallthrough
	case Failure:
		fallthrough
	case Terminated:
		ua.Session = nil
	}

}

func (ua *UserAgent) Register(domain string, userdata interface{}) error {
	sipuri, err := parser.ParseSipUri("sip:" + ua.username + "@" + domain + ";transport=udp")
	if err != nil {
		return log.EInvalidField("subiz", "url", "sip:"+ua.username+"@"+domain, log.M{"err": err})
	}

	profile := NewProfile(sipuri.Clone(), "Subiz WC", &AuthInfo{AuthUser: ua.username, Password: ua.password}, 1800, ua.stack)
	ua.profile = profile
	expires := profile.Expires

	register := NewRegister(ua, profile, sipuri, userdata)
	ua.register = register
	if err := register.SendRegister(expires); err != nil {
		log.Err("", err, "SendRegister failed")
		return err
	}

	go func() {
		for ua.register == register {
			log.Info("subiz", "sending register")
			if err := register.SendRegister(expires); err != nil {
				log.Err("", err, "SendRegister failed")
			}
			time.Sleep(5 * time.Minute)
		}
	}()
	return nil
}

// func (ua *UserAgent) Request(req *sip.Request) (sip.ClientTransaction, error) {
// return ua.stack.Request(*req)
// }

func (ua *UserAgent) handleBye(request sip.Request, tx sip.ServerTransaction) {
	log.Info("", "handleBye: Request => %s, body => %s", request.Short(), request.Body())
	response := sip.NewResponseFromRequest(request.MessageID(), request, 200, "OK", "")

	if viaHop, ok := request.ViaHop(); ok {
		var port sip.Port
		host := viaHop.Host
		if viaHop.Params != nil {
			if received, ok := viaHop.Params.Get("received"); ok && received.String() != "" {
				host = received.String()
			}
			if viaHop.Port != nil {
				port = *viaHop.Port
			} else if rport, ok := viaHop.Params.Get("rport"); ok && rport != nil && rport.String() != "" {
				if p, err := strconv.Atoi(rport.String()); err == nil {
					port = sip.Port(uint16(p))
				}
			} else {
				port = sip.DefaultPort(request.Transport())
			}
		}

		dest := fmt.Sprintf("%v:%v", host, port)
		response.SetDestination(dest)
	}

	tx.Respond(response)
	callID, ok := request.CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			is := v.(*Session)
			ua.iss.Delete(*callID)
			var transaction sip.Transaction = tx.(sip.Transaction)
			ua.handleInviteState(is, &request, &response, Terminated, &transaction)
		}
	}
}

func (ua *UserAgent) handleCancel(request sip.Request, tx sip.ServerTransaction) {
	log.Info("", "handleCancel: Request => %s, body => %s", request.Short(), request.Body())
	response := sip.NewResponseFromRequest(request.MessageID(), request, 200, "OK", "")
	tx.Respond(response)

	callID, ok := request.CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			is := v.(*Session)
			ua.iss.Delete(*callID)
			var transaction sip.Transaction = tx.(sip.Transaction)
			is.SetState(Canceled)
			ua.handleInviteState(is, &request, nil, Canceled, &transaction)
		}
	}
}

func (ua *UserAgent) handleACK(request sip.Request, tx sip.ServerTransaction) {
	log.Info("", "handleACK => ", request.Short(), "body", request.Body())
	callID, ok := request.CallID()
	if ok {
		if v, found := ua.iss.Load(*callID); found {
			// handle Ringing or Processing with sdp
			is := v.(*Session)
			is.SetState(Confirmed)
			ua.handleInviteState(is, &request, nil, Confirmed, nil)
		}
	}
}

func (ua *UserAgent) handleInvite(request sip.Request, tx sip.ServerTransaction) {
	log.Info("", "handleInvite => "+request.Short())

	callID, ok := request.CallID()
	if ok {
		var transaction sip.Transaction = tx.(sip.Transaction)
		if toHdr, ok := request.To(); ok && toHdr.Params.Has("tag") {
			v, found := ua.iss.Load(*callID)
			if found {
				is := v.(*Session)
				is.SetState(ReInviteReceived)
				ua.handleInviteState(is, &request, nil, ReInviteReceived, &transaction)
			} else {
				// reinvite for transaction we have no record of; reject it
				response := sip.NewResponseFromRequest(request.MessageID(), request, sip.StatusCode(481), "Call/Transaction does not exist", "")
				tx.Respond(response)
			}
		} else {
			contactHdr, _ := request.Contact()
			contactAddr := ua.updateContact2UAAddr(request.Transport(), contactHdr.Address)
			contactHdr.Address = contactAddr

			is := NewInviteSession(ua.RequestWithContext, "UAS", contactHdr, request, *callID, transaction, Incoming)
			ua.iss.Store(*callID, is)
			is.SetState(InviteReceived)
			ua.handleInviteState(is, &request, nil, InviteReceived, &transaction)
			is.SetState(WaitingForAnswer)
		}
	}

	go func() {
		cancel := <-tx.Cancels()
		if cancel == nil {
			return
		}
		log.Info("", "Cancel \n", cancel.String())
		response := sip.NewResponseFromRequest(cancel.MessageID(), cancel, 200, "OK", "")
		if callID, ok := response.CallID(); ok {
			if v, found := ua.iss.Load(*callID); found {
				ua.iss.Delete(*callID)
				is := v.(*Session)
				is.SetState(Canceled)
				ua.handleInviteState(is, &request, &response, Canceled, nil)
			}
		}

		tx.Respond(response)
	}()

	go func() {
		ack := <-tx.Acks()
		if ack != nil {
			log.Info("", "ack =>", ack)
		}
	}()
}

func (ua *UserAgent) handleUpdate(request sip.Request, tx sip.ServerTransaction) {
	log.Info("", "handleUpdate: Request =>", request.Short())
	response := sip.NewResponseFromRequest(request.MessageID(), request, 200, "OK", "")
	tx.Respond(response)
}

func (ua *UserAgent) handleOptions(request sip.Request, tx sip.ServerTransaction) {
	response := sip.NewResponseFromRequest(request.MessageID(), request, 200, "OK", "")
	tx.Respond(response)
}

// RequestWithContext .
func (ua *UserAgent) RequestWithContext(ctx context.Context, request sip.Request, authorizer sip.Authorizer, waitForResult bool, attempt int) (sip.Response, error) {
	tx, err := ua.stack.Request(request)
	if err != nil {
		return nil, err
	}
	var cts sip.Transaction = tx.(sip.Transaction)
	if request.IsInvite() {
		if callID, ok := request.CallID(); ok {
			if _, found := ua.iss.Load(*callID); !found {
				contactHdr, _ := request.Contact()
				contactAddr := ua.updateContact2UAAddr(request.Transport(), contactHdr.Address)
				contactHdr.Address = contactAddr
				is := NewInviteSession(ua.RequestWithContext, "UAC", contactHdr, request, *callID, cts, Outgoing)
				ua.iss.Store(*callID, is)
				is.ProvideOffer(request.Body())
				is.SetState(InviteSent)
				ua.handleInviteState(is, &request, nil, InviteSent, &cts)
			}
		}
	}

	responses := make(chan sip.Response)
	provisionals := make(chan sip.Response)
	errs := make(chan error)
	go func() {
		var lastResponse sip.Response

		previousResponses := make([]sip.Response, 0)
		previousResponsesStatuses := make(map[sip.StatusCode]bool)

		for {
			select {
			case <-ctx.Done():
				if lastResponse != nil && lastResponse.IsProvisional() {
					ua.stack.CancelRequest(request, lastResponse)
				}
				if lastResponse != nil {
					lastResponse.SetPrevious(previousResponses)
				}
				errs <- sip.NewRequestError(487, "Request Terminated", request, lastResponse)
				// pull out later possible transaction responses and errors
				go func() {
					for {
						select {
						case <-tx.Done():
							return
						case <-tx.Errors():
						case <-tx.Responses():
						}
					}
				}()
				return
			case err, ok := <-tx.Errors():
				if !ok {
					if lastResponse != nil {
						lastResponse.SetPrevious(previousResponses)
					}
					errs <- sip.NewRequestError(487, "Request Terminated", request, lastResponse)
					return
				}

				switch err.(type) {
				case *transaction.TxTimeoutError:
					{
						errs <- sip.NewRequestError(408, "Request Timeout", request, lastResponse)
						return
					}
				}

				//errs <- err
				return
			case response, ok := <-tx.Responses():
				if !ok {
					if lastResponse != nil {
						lastResponse.SetPrevious(previousResponses)
					}
					errs <- sip.NewRequestError(487, "Request Terminated", request, lastResponse)
					return
				}

				response = sip.CopyResponse(response)
				lastResponse = response

				if response.IsProvisional() {
					if _, ok := previousResponsesStatuses[response.StatusCode()]; !ok {
						previousResponses = append(previousResponses, response)
					}
					provisionals <- response
					continue
				}

				// success
				if response.IsSuccess() {
					response.SetPrevious(previousResponses)

					if request.IsInvite() {
						ua.stack.AckInviteRequest(request, response)
						ua.stack.RememberInviteRequest(request)
						go func() {
							for response := range tx.Responses() {
								ua.stack.AckInviteRequest(request, response)
							}
						}()
					}
					responses <- response
					tx.Done()
					return
				}

				// unauth request
				needAuth := (response.StatusCode() == 401 || response.StatusCode() == 407) && attempt < 2
				if needAuth && authorizer != nil {
					if err := authorizer.AuthorizeRequest(request, response); err != nil {
						errs <- err
						return
					}
					if response, err := ua.RequestWithContext(ctx, request, authorizer, true, attempt+1); err == nil {
						responses <- response
					} else {
						errs <- err
					}
					return
				}

				// failed request
				lastResponse.SetPrevious(previousResponses)
				errs <- sip.NewRequestError(uint(response.StatusCode()), response.Reason(), request, lastResponse)
				return
			}
		}
	}()

	waitForResponse := func(cts *sip.Transaction) (sip.Response, error) {
		for {
			select {
			case provisional := <-provisionals:
				callID, ok := provisional.CallID()
				if ok {
					if v, found := ua.iss.Load(*callID); found {
						is := v.(*Session)
						is.StoreResponse(provisional)
						// handle Ringing or Processing with sdp
						ua.handleInviteState(is, &request, &provisional, Provisional, cts)
						if len(provisional.Body()) > 0 {
							is.SetState(EarlyMedia)
							ua.handleInviteState(is, &request, &provisional, EarlyMedia, cts)
						}
					}
				}
			case err := <-errs:
				//TODO: error type switch transaction.TxTimeoutError
				switch err.(type) {
				case *transaction.TxTimeoutError:
					//errs <- sip.NewRequestError(408, "Request Timeout", nil, nil)
					return nil, err
				}
				sipreq, ok := err.(*sip.RequestError)
				if !ok {
					log.Info("subiz", "EEEEEEEEEEEEE", err)
					return nil, err
				}
				request := sipreq.Request
				response := sipreq.Response
				callID, ok := request.CallID()
				if ok {
					if v, found := ua.iss.Load(*callID); found {
						is := v.(*Session)
						ua.iss.Delete(*callID)
						is.SetState(Failure)
						ua.handleInviteState(is, &request, &response, Failure, nil)
					}
				}
				return nil, err
			case response := <-responses:
				callID, ok := response.CallID()
				if ok {
					if v, found := ua.iss.Load(*callID); found {
						if request.IsInvite() {
							is := v.(*Session)
							is.SetState(Confirmed)
							ua.handleInviteState(is, &request, &response, Confirmed, nil)
						} else if request.Method() == sip.BYE {
							is := v.(*Session)
							ua.iss.Delete(*callID)
							is.SetState(Terminated)
							ua.handleInviteState(is, &request, &response, Terminated, nil)
						}
					}
				}
				return response, nil
			}
		}
	}

	if !waitForResult {
		go waitForResponse(&cts)
		return nil, err
	}
	return waitForResponse(&cts)
}

func (ua *UserAgent) Shutdown() {
	if ua.register != nil {
		ua.register.SendRegister(0)
	}
	ua.register = nil
	if ua.Session != nil {
		ua.Session.End()
	}
	ua.Session = nil
	ua.stack.Shutdown()
	time.Sleep(200 * time.Millisecond)
}

func (ua *UserAgent) updateContact2UAAddr(transport string, from sip.ContactUri) sip.ContactUri {
	stackAddr := ua.stack.GetNetworkInfo(transport)
	ret := from.Clone()
	ret.SetHost(stackAddr.Host)
	ret.SetPort(stackAddr.Port)
	return ret
}

var a = 0

func generateHello() ([]byte, []byte, []byte, []byte, []byte) {
	ts := rand.IntN(1002303)
	ssrc := uint32(rand.IntN(100230320934))
	seq := uint16(rand.IntN(102303))
	// Create a new RTP packet
	packet0 := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    uint8(95),  // Example payload type for OPUS
			SSRC:           ssrc,       // Synchronization source (SSRC) identifier
			SequenceNumber: seq,        // Sequence number
			Timestamp:      uint32(ts), // Optional Example timestamp in milliseconds
		},
		Payload: []byte{0},
	}

	//	bs, err := hex.DecodeString("780be4c136ecc58d8c49469904c5aaed92e7634a3a1898ee62cb60ff6c1b2900")
	//if err != nil {
	//panic(err)
	//}
	bs := []byte{0}
	packet1 := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    uint8(107), // OPUS
			SSRC:           ssrc,       // Synchronization source (SSRC) identifier
			SequenceNumber: seq + 1,    // Sequence number
			Timestamp:      uint32(ts), // Optional Example timestamp in milliseconds
			Marker:         true,
		},

		Payload: bs,
	}

	packet2 := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    uint8(107),       // OPUS
			SSRC:           ssrc,             // Synchronization source (SSRC) identifier
			SequenceNumber: seq + 2,          // Sequence number
			Timestamp:      uint32(ts + 100), // Optional Example timestamp in milliseconds
		},
		Payload: bs,
	}

	packet3 := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    uint8(107),       // OPUS
			SSRC:           ssrc,             // Synchronization source (SSRC) identifier
			SequenceNumber: seq + 3,          // Sequence number
			Timestamp:      uint32(ts + 200), // Optional Example timestamp in milliseconds
		},
		Payload: bs,
	}

	packet4 := rtp.Packet{
		Header: rtp.Header{
			Version:        2,
			PayloadType:    uint8(107),       // OPUS
			SSRC:           ssrc,             // Synchronization source (SSRC) identifier
			SequenceNumber: seq + 4,          // Sequence number
			Timestamp:      uint32(ts + 300), // Optional Example timestamp in milliseconds
		},
		Payload: bs,
	}

	// Create a sample payload (replace with your actual payload)
	// Marshal the packet with the payload
	b0, _ := packet0.Marshal()
	b1, _ := packet1.Marshal()
	b2, _ := packet2.Marshal()
	b3, _ := packet3.Marshal()
	b4, _ := packet4.Marshal()
	return b0, b1, b2, b3, b4
}

func (me *UserAgent) listenFromUDP(session *Session, udpconn *UDPConn) {
	a++
	if a > 1 {
		return
	}
	start := time.Now()

	pk0, pk1, pk2, pk3, pk4 := generateHello()
	udpconn.Write(pk0)
	udpconn.Write(pk1)
	udpconn.Write(pk2)
	time.Sleep(100 * time.Millisecond)
	udpconn.Write(pk3)
	time.Sleep(100 * time.Millisecond)
	udpconn.Write(pk4)

	filename := fmt.Sprintf("./output_%02d%02d%02d_%d.ogg", time.Now().Year()-2000, time.Now().Month(), time.Now().Day(), rand.IntN(1000))
	fmt.Println("WRITING TO", filename)
	oggFile, err := oggwriter.New(filename, 48000, 2)
	if err != nil {
		panic(err)
	}

	defer func() {
		fmt.Println("SAVED", filename)
		oggFile.Close()
	}()
	// Read RTP packets forever and send them to the WebRTC Client
	packet := make([]byte, 1600) // UDP MTU
	for me.Session == session {  // server is still running
		udpconn.SetReadDeadline(time.Now().Add(5 * time.Second))
		n, _, err := udpconn.ReadFrom(packet)
		fmt.Println(time.Since(start), "RECORDING TO", filename, "BYTES", n)
		rtpPacket := &rtp.Packet{}
		rtpPacket.Unmarshal(packet[:n])
		if err := oggFile.WriteRTP(rtpPacket); err != nil {
			fmt.Println(err)
			return
		}

		if err != nil {
			if isConnectionClosed(err) {
				// what to to when connection break
				fmt.Println("udp_connection_breaked")
				me.Session.End()
				return
			}
			time.Sleep(1 * time.Second)
			continue
		}

		time.Sleep(10 * time.Millisecond)
	}
}

func (me *UserAgent) handleCallMedia(sess *Session) {
	me.Session = sess
	desc := sdp.SessionDescription{}
	if err := desc.Unmarshal([]byte(sess.RemoteSdp())); err != nil {
		sess.End()
		return
	}

	for _, medesc := range desc.MediaDescriptions {
		if medesc.MediaName.Media != "audio" {
			continue
		}
		me.Udp.SetRemoteAddr(&net.UDPAddr{IP: net.ParseIP(asteriskIp), Port: medesc.MediaName.Port.Value})
		go me.listenFromUDP(sess, me.Udp)
		break
	}
}

func BuildLocalSdp(host string, port int) string {
	ms := time.Now().UnixMilli()

	media := (&sdp.MediaDescription{
		MediaName: sdp.MediaName{
			Media:  "audio",
			Port:   sdp.RangedPort{Value: port},
			Protos: []string{"RTP", "AVP"},
		},
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: host},
		},
	}).
		WithCodec(107, "opus", 48000, 2, "minptime=20;useinbandfec=1").
		WithCodec(101, "telephone-event", 8000, 0, "0-16").
		WithPropertyAttribute(sdp.AttrKeySendRecv)

	desc := sdp.SessionDescription{
		Origin: sdp.Origin{
			Username:       "S",
			SessionID:      uint64(ms),
			SessionVersion: uint64(ms),
			NetworkType:    "IN",
			AddressType:    "IP4",
			UnicastAddress: host,
		},
		SessionName: "-",
		TimeDescriptions: []sdp.TimeDescription{{
			Timing:      sdp.Timing{StartTime: 0, StopTime: 0},
			RepeatTimes: nil,
		}},
		ConnectionInformation: &sdp.ConnectionInformation{
			NetworkType: "IN",
			AddressType: "IP4",
			Address:     &sdp.Address{Address: host},
		},
		MediaDescriptions: []*sdp.MediaDescription{media},
	}

	pisdp, _ := desc.Marshal()
	return string(pisdp)
}

func main() {
	var username, password string
	if len(os.Args) > 1 {
		username = os.Args[1]
	}

	if len(os.Args) > 2 {
		password = os.Args[2]
	}

	var callto string
	if len(os.Args) > 3 {
		callto = os.Args[3]
	}

	if username == "" || password == "" || callto == "" {
		fmt.Println("missing username or password or callto parameters, should call ./callsip username password 201@callcenter.subiz.com.vn")
		os.Exit(1)
		return
	}

	stack := NewSipStack(privateIp, "SubizTestCall 1")
	if err := stack.Listen("udp", fmt.Sprintf("%s:%d", privateIp, 5067)); err != nil {
		panic(err)
	}

	var domain string
	cs := strings.Split(callto, "@")
	if len(cs) > 1 {
		domain = cs[len(cs)-1]
	}
	if domain == "" {
		domain = "callcenter.subiz.com.vn"
		callto = callto + "@" + domain
	}
	useragent := NewUserAgent(stack, username, password)
	if err := useragent.Register(domain, nil); err != nil {
		fmt.Println("Cannot Authenticate:", err.Error())
		os.Exit(1)
	}

	fmt.Println("TEST CALL TO", "sip:"+callto+";transport=udp")
	go func() {
		called, _ := parser.ParseUri("sip:" + callto + ";transport=udp")
		sipuri, _ := parser.ParseSipUri("sip:" + callto + ";transport=udp")
		callid := uuid.New().String()
		if err := useragent.Invite(callid, called, sipuri); err != nil {
			useragent.Shutdown()
			panic(err)
		}
	}()
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)

	done := make(chan bool, 1)
	go func() {
		sig := <-sigs
		fmt.Println()
		fmt.Println(sig)
		done <- true
	}()

	<-done
	useragent.Shutdown()
}
