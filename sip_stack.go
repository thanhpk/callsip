package main

import (
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/ghettovoice/gosip/log"
	"github.com/ghettovoice/gosip/sip"
	"github.com/ghettovoice/gosip/transaction"
	"github.com/ghettovoice/gosip/transport"
	"github.com/ghettovoice/gosip/util"
	"github.com/tevino/abool"
)

// RequestHandler is a callback that will be called on the incoming request
// of the certain method
// tx argument can be nil for 2xx ACK request
type RequestHandler func(req sip.Request, tx sip.ServerTransaction)

// SipStack a golang SIP Stack
type SipStack struct {
	running               abool.AtomicBool
	listenPorts           map[string]*sip.Port
	tp                    transport.Layer
	tx                    transaction.Layer
	userAgent             string
	host                  string
	hwg                   *sync.WaitGroup
	hmu                   *sync.RWMutex
	requestHandlers       map[sip.RequestMethod]RequestHandler
	handleConnectionError func(err *transport.ConnectionError)
	invites               map[transaction.TxKey]sip.Request
	invitesLock           *sync.RWMutex
}

// NewSipStack creates new instance of SipStack.
func NewSipStack(host, userAgent string) *SipStack {
	var ip net.IP
	if addr, err := net.ResolveIPAddr("ip", host); err == nil {
		ip = addr.IP
	} else {
		panic("resolve host IP failed: " + err.Error())
	}

	s := &SipStack{
		userAgent:       userAgent,
		listenPorts:     make(map[string]*sip.Port),
		host:            host,
		hwg:             new(sync.WaitGroup),
		hmu:             new(sync.RWMutex),
		requestHandlers: make(map[sip.RequestMethod]RequestHandler),
		invites:         make(map[transaction.TxKey]sip.Request),
		invitesLock:     new(sync.RWMutex),
	}

	s.tp = transport.NewLayer(ip, net.DefaultResolver, nil, NewLogger())
	sipTp := &sipTransport{tpl: s.tp, s: s}
	s.tx = transaction.NewLayer(sipTp, NewLogger())

	s.running.Set()
	go s.serve()

	return s
}

func (s *SipStack) Listen(protocol string, listenAddr string) error {
	network := strings.ToUpper(protocol)
	err := s.tp.Listen(network, listenAddr)
	if err == nil {
		target, err := transport.NewTargetFromAddr(listenAddr)
		if err != nil {
			return err
		}
		target = transport.FillTargetHostAndPort(network, target)
		if _, ok := s.listenPorts[network]; !ok {
			s.listenPorts[network] = target.Port
		}
	}
	return err
}

func (s *SipStack) serve() {
	defer s.Shutdown()

	for {
		select {
		case _, ok := <-s.tx.Done():
			if !ok {
				return
			}
		case tx, ok := <-s.tx.Requests():
			if !ok {
				return
			}
			s.hwg.Add(1)
			go s.handleRequest(tx.Origin(), tx)
		case ack, ok := <-s.tx.Acks():
			if !ok {
				return
			}
			s.hwg.Add(1)
			go s.handleRequest(ack, nil)
		case response, ok := <-s.tx.Responses():
			if !ok {
				return
			}
			if key, err := transaction.MakeClientTxKey(response); err == nil {
				s.invitesLock.RLock()
				inviteRequest, ok := s.invites[key]
				s.invitesLock.RUnlock()
				if ok {
					go s.AckInviteRequest(inviteRequest, response)
				}
			}

		case _, ok := <-s.tp.Done():
			if !ok {
				return
			}

		case err, ok := <-s.tx.Errors():
			if !ok {
				return
			}
			fmt.Println(err, "received SIP transaction error")
		case err, ok := <-s.tp.Errors():
			if !ok {
				return
			}

			var ferr *sip.MalformedMessageError
			if errors.Is(err, io.EOF) {
				fmt.Println(err, "received SIP transport error")
			} else if errors.As(err, &ferr) {
				fmt.Println(err, "received SIP transport error")
			} else {
				fmt.Println(err, "received SIP transport error")
			}

			if connError, ok := err.(*transport.ConnectionError); ok {
				if s.handleConnectionError != nil {
					s.handleConnectionError(connError)
				}
			}
		}
	}
}

func (s *SipStack) handleRequest(req sip.Request, tx sip.ServerTransaction) {
	defer s.hwg.Done()

	fmt.Println("routing incoming SIP request", req.Method())
	if req.Method() != sip.OPTIONS {
		fmt.Println(req.String())
	}

	s.hmu.RLock()
	handler, ok := s.requestHandlers[req.Method()]
	s.hmu.RUnlock()

	if !ok {
		fmt.Println("SIP request %v handler not found", req.Method())

		go func(tx sip.ServerTransaction) {
			for {
				select {
				case <-s.tx.Done():
					return
				case err, ok := <-tx.Errors():
					if !ok {
						return
					}

					fmt.Println(err, "error from SIP server transaction", tx)
				}
			}
		}(tx)

		res := sip.NewResponseFromRequest("", req, 405, "Method Not Allowed", "")
		if _, err := s.Respond(res); err != nil {
			fmt.Println(err, "respond '405 Method Not Allowed' failed")
		}

		return
	}

	go handler(req, tx)
}

// Request Send SIP message
func (s *SipStack) Request(req sip.Request) (sip.ClientTransaction, error) {
	if !s.running.IsSet() {
		return nil, fmt.Errorf("can not send through stopped server")
	}

	request := s.prepareRequest(req)
	// fmt.Println("SENDING", request.String())
	return s.tx.Request(request)
}

func (s *SipStack) GetNetworkInfo(protocol string) *transport.Target {
	var target transport.Target
	if s.host != "" {
		target.Host = s.host
	} else if v, err := util.ResolveSelfIP(); err == nil {
		target.Host = v.String()
	} else {
		panic("resolve host IP failed" + err.Error())
	}

	network := strings.ToUpper(protocol)
	if p, ok := s.listenPorts[network]; ok {
		target.Port = p
	} else {
		defPort := sip.DefaultPort(network)
		target.Port = &defPort
	}
	return &target
}

func (s *SipStack) RememberInviteRequest(request sip.Request) {
	if key, err := transaction.MakeClientTxKey(request); err == nil {
		s.invitesLock.Lock()
		s.invites[key] = request
		s.invitesLock.Unlock()

		time.AfterFunc(time.Minute, func() {
			s.invitesLock.Lock()
			delete(s.invites, key)
			s.invitesLock.Unlock()
		})
	} else {
		fmt.Println(err, "remember of the request failed")
	}
}

func (s *SipStack) AckInviteRequest(request sip.Request, response sip.Response) {
	ackRequest := sip.NewAckRequest("", request, response, "", nil)
	if err := s.Send(ackRequest); err != nil {
		fmt.Println(err, "send ACK request failed")
	}
}

func (s *SipStack) CancelRequest(request sip.Request, response sip.Response) {
	cancelRequest := sip.NewCancelRequest("", request, nil)
	if err := s.Send(cancelRequest); err != nil {
		fmt.Println(err, "send CANCEL request failed")
	}
}

func (s *SipStack) prepareRequest(req sip.Request) sip.Request {
	if viaHop, ok := req.ViaHop(); ok {
		if viaHop.Params == nil {
			viaHop.Params = sip.NewParams()
		}
		if !viaHop.Params.Has("branch") {
			viaHop.Params.Add("branch", sip.String{Str: sip.GenerateBranch()})
		}
	} else {
		viaHop = &sip.ViaHop{
			ProtocolName:    "SIP",
			ProtocolVersion: "2.0",
			Params: sip.NewParams().
				Add("branch", sip.String{Str: sip.GenerateBranch()}),
		}

		req.PrependHeaderAfter(sip.ViaHeader{viaHop}, "Route")
	}

	s.appendAutoHeaders(req)
	return req
}

// Respond .
func (s *SipStack) Respond(res sip.Response) (sip.ServerTransaction, error) {
	if !s.running.IsSet() {
		return nil, fmt.Errorf("can not send through stopped server")
	}

	return s.tx.Respond(s.prepareResponse(res))
}

// Send .
func (s *SipStack) Send(msg sip.Message) error {
	if !s.running.IsSet() {
		return fmt.Errorf("can not send through stopped server")
	}

	switch m := msg.(type) {
	case sip.Request:
		msg = s.prepareRequest(m)
	case sip.Response:
		msg = s.prepareResponse(m)
	}

	return s.tp.Send(msg)
}

func (s *SipStack) prepareResponse(res sip.Response) sip.Response {
	s.appendAutoHeaders(res)
	return res
}

// Shutdown gracefully shutdowns SIP server
func (s *SipStack) Shutdown() {
	if !s.running.IsSet() {
		return
	}
	s.running.UnSet()
	// stop transaction layer
	s.tx.Cancel()
	<-s.tx.Done()
	// stop transport layer
	s.tp.Cancel()
	<-s.tp.Done()
	// wait for handlers
	s.hwg.Wait()
}

// OnRequest registers new request callback
func (s *SipStack) OnRequest(method sip.RequestMethod, handler RequestHandler) error {
	s.hmu.Lock()
	s.requestHandlers[method] = handler
	s.hmu.Unlock()

	return nil
}

func (s *SipStack) OnConnectionError(handler func(err *transport.ConnectionError)) {
	s.hmu.Lock()
	s.handleConnectionError = handler
	s.hmu.Unlock()
}

func (s *SipStack) appendAutoHeaders(msg sip.Message) {
	autoAppendMethods := map[sip.RequestMethod]bool{
		sip.INVITE:   true,
		sip.REGISTER: true,
		sip.OPTIONS:  true,
		sip.REFER:    true,
		sip.NOTIFY:   true,
	}

	var msgMethod sip.RequestMethod
	switch m := msg.(type) {
	case sip.Request:
		msgMethod = m.Method()
	case sip.Response:
		if cseq, ok := m.CSeq(); ok && !m.IsProvisional() {
			msgMethod = cseq.MethodName
		}
	}
	if len(msgMethod) > 0 {
		if _, ok := autoAppendMethods[msgMethod]; ok {
			hdrs := msg.GetHeaders("Allow")
			if len(hdrs) == 0 {
				allow := make(sip.AllowHeader, 0)
				for _, method := range s.getAllowedMethods() {
					allow = append(allow, method)
				}

				msg.AppendHeader(allow)
			}
		}
	}

	if hdrs := msg.GetHeaders("User-Agent"); len(hdrs) == 0 {
		userAgentHeader := sip.UserAgentHeader(s.userAgent)
		msg.AppendHeader(&userAgentHeader)
	} else if len(s.userAgent) > 0 {
		msg.RemoveHeader("User-Agent")
		userAgentHeader := sip.UserAgentHeader(s.userAgent)
		msg.AppendHeader(&userAgentHeader)
	}

	if hdrs := msg.GetHeaders("Content-Length"); len(hdrs) == 0 {
		msg.SetBody(msg.Body(), true)
	}
}

func (s *SipStack) getAllowedMethods() []sip.RequestMethod {
	methods := []sip.RequestMethod{
		sip.INVITE,
		sip.ACK,
		sip.BYE,
		sip.CANCEL,
		sip.INFO,
		sip.OPTIONS,
	}
	added := map[sip.RequestMethod]bool{
		sip.INVITE:  true,
		sip.ACK:     true,
		sip.BYE:     true,
		sip.CANCEL:  true,
		sip.INFO:    true,
		sip.OPTIONS: true,
	}

	s.hmu.RLock()
	for method := range s.requestHandlers {
		if _, ok := added[method]; !ok {
			methods = append(methods, method)
		}
	}
	s.hmu.RUnlock()

	return methods
}

type sipTransport struct {
	tpl transport.Layer
	s   *SipStack
}

func (tp *sipTransport) Messages() <-chan sip.Message {
	return tp.tpl.Messages()
}

func (tp *sipTransport) Send(msg sip.Message) error {
	return tp.s.Send(msg)
}

func (tp *sipTransport) IsReliable(network string) bool {
	return tp.tpl.IsReliable(network)
}

func (tp *sipTransport) IsStreamed(network string) bool {
	return tp.tpl.IsStreamed(network)
}

func NewLogger() log.Logger {
	return &MuteLogger{} // mute

	// Output to stdout instead of the default stderr
	// Can be any io.Writer, see below for File example
}

type MuteLogger struct{}

func (l *MuteLogger) Print(args ...interface{})                           {}
func (l *MuteLogger) Printf(format string, args ...interface{})           {}
func (l *MuteLogger) Trace(args ...interface{})                           {}
func (l *MuteLogger) Tracef(format string, args ...interface{})           {}
func (l *MuteLogger) Debug(args ...interface{})                           {}
func (l *MuteLogger) Debugf(format string, args ...interface{})           {}
func (l *MuteLogger) Info(args ...interface{})                            {}
func (l *MuteLogger) Infof(format string, args ...interface{})            {}
func (l *MuteLogger) Warn(args ...interface{})                            {}
func (l *MuteLogger) Warnf(format string, args ...interface{})            {}
func (l *MuteLogger) Error(args ...interface{})                           {}
func (l *MuteLogger) Errorf(format string, args ...interface{})           {}
func (l *MuteLogger) Fatal(args ...interface{})                           {}
func (l *MuteLogger) Fatalf(format string, args ...interface{})           {}
func (l *MuteLogger) Panic(args ...interface{})                           {}
func (l *MuteLogger) Panicf(format string, args ...interface{})           {}
func (l *MuteLogger) WithPrefix(prefix string) log.Logger                 { return l }
func (l *MuteLogger) Prefix() string                                      { return "" }
func (l *MuteLogger) WithFields(fields map[string]interface{}) log.Logger { return l }
func (l *MuteLogger) Fields() log.Fields                                  { return log.Fields{} }
func (l *MuteLogger) SetLevel(level uint32)                               {}
