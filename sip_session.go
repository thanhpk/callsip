package main

import (
	"context"
	"sync"

	"github.com/ghettovoice/gosip/sip"
	"github.com/thanhpk/randstr"
)

type Status string

const (
	InviteSent       Status = "InviteSent"       /**< After INVITE s sent */
	InviteReceived   Status = "InviteReceived"   /**< After INVITE s received. */
	ReInviteReceived Status = "ReInviteReceived" /**< After re-INVITE/UPDATE s received */
	//Answer         Status = "Answer"           /**< After response for re-INVITE/UPDATE. */
	Provisional      Status = "Provisional" /**< After response for 1XX. */
	EarlyMedia       Status = "EarlyMedia"  /**< After response 1XX with sdp. */
	WaitingForAnswer Status = "WaitingForAnswer"
	WaitingForACK    Status = "WaitingForACK" /**< After 2xx s sent/received. */
	Answered         Status = "Answered"
	Canceled         Status = "Canceled"
	Confirmed        Status = "Confirmed"  /**< After ACK s sent/received. */
	Failure          Status = "Failure"    /**< Session s rejected or canceled. */
	Terminated       Status = "Terminated" /**< Session s terminated. */
)

type RequestCallback func(ctx context.Context, request sip.Request, authorizer sip.Authorizer, waitForResult bool, attempt int) (sip.Response, error)

type Session struct {
	lock           sync.Mutex
	requestCallbck RequestCallback
	status         Status
	callID         sip.CallID
	offer          string
	answer         string
	request        sip.Request
	response       sip.Response
	transaction    sip.Transaction
	contact        *sip.ContactHeader
	localURI       sip.Address
	remoteURI      sip.Address
	remoteTarget   sip.Uri
}

func NewInviteSession(reqcb RequestCallback,
	contact *sip.ContactHeader, req sip.Request, cid sip.CallID,
	tx sip.Transaction) *Session {
	s := &Session{
		requestCallbck: reqcb,
		callID:         cid,
		transaction:    tx,
		offer:          "",
		answer:         "",
		contact:        contact,
	}

	to, _ := req.To()
	from, _ := req.From()

	if to.Params != nil && !to.Params.Has("tag") {
		to.Params.Add("tag", sip.String{Str: randstr.String(8)})
		req.RemoveHeader("To")
		req.AppendHeader(to)
	}

	s.localURI = sip.Address{Uri: from.Address, Params: from.Params}
	s.remoteURI = sip.Address{Uri: to.Address, Params: to.Params}
	s.remoteTarget = req.Recipient()
	s.offer = req.Body()
	s.request = req
	return s
}

func (s *Session) RemoteSdp() string {
	return s.answer
}

func (s *Session) CallID() *sip.CallID {
	return &s.callID
}

func (s *Session) Tag() string {
	return ""
}

func (s *Session) Request() sip.Request {
	return s.request
}

func (s *Session) Response() sip.Response {
	return s.response
}

func (s *Session) StoreRequest(request sip.Request) {
	s.request = request
}

func (s *Session) StoreResponse(response sip.Response) {
	to, _ := response.To()
	if to.Params != nil && to.Params.Has("tag") {
		//Update to URI.
		s.remoteURI = sip.Address{Uri: to.Address, Params: to.Params}
	}

	sdp := response.Body()
	if len(sdp) > 0 {
		s.answer = sdp
	}
	s.response = response
}

func (s *Session) StoreTransaction(tx sip.Transaction) {
	if s.transaction != nil {
		s.transaction.Done()
	}
	s.transaction = tx
}

func (s *Session) SetState(status Status) {
	s.lock.Lock()
	defer s.lock.Unlock()
	s.status = status
}

func (s *Session) Status() Status {
	s.lock.Lock()
	defer s.lock.Unlock()
	return s.status
}

// GetEarlyMedia Get sdp for early media.
func (s *Session) GetEarlyMedia() string {
	return s.answer
}

// ProvideOffer .
func (s *Session) ProvideOffer(sdp string) {
	s.offer = sdp
}

// ProvideAnswer .
func (s *Session) ProvideAnswer(sdp string) {
	s.answer = sdp
}

// Bye send Bye request.
func (s *Session) Bye() (sip.Response, error) {
	req := s.makeRequest(sip.BYE, sip.MessageID(s.callID), s.request, s.response)
	return s.sendRequest(req)
}

func (s *Session) sendRequest(req sip.Request) (sip.Response, error) {
	return s.requestCallbck(context.TODO(), req, nil, false, 1)
}

// End end session
func (s *Session) End() {
	if s.status == Terminated {
		return
	}
	switch s.status {
	// - UAC -
	case InviteSent:
		fallthrough
	case Provisional:
		fallthrough
	case EarlyMedia:
		switch s.transaction.(type) {
		case sip.ClientTransaction:
			s.transaction.(sip.ClientTransaction).Cancel()
		case sip.ServerTransaction:
			s.transaction.(sip.ServerTransaction).Done()
		}

	case WaitingForACK:
		fallthrough
	case Confirmed:
		s.Bye()
	}
}

func (s *Session) makeRequest(method sip.RequestMethod, msgID sip.MessageID, inviteRequest sip.Request, inviteResponse sip.Response) sip.Request {
	var rh *sip.RouteHeader
	newRequest := sip.NewRequest(
		msgID,
		method,
		s.remoteTarget,
		inviteRequest.SipVersion(),
		[]sip.Header{},
		"",
		nil,
	)

	from := s.localURI.Clone().AsFromHeader()
	newRequest.AppendHeader(from)
	to := s.remoteURI.Clone().AsToHeader()
	newRequest.AppendHeader(to)
	newRequest.SetRecipient(s.request.Recipient())
	sip.CopyHeaders("Via", inviteRequest, newRequest)
	newRequest.AppendHeader(s.contact)

	for _, header := range s.response.Headers() {
		if header.Name() == "Record-Route" {
			h := header.(*sip.RecordRouteHeader)
			rh = &sip.RouteHeader{
				Addresses: h.Addresses,
			}
		}
	}

	if rh != nil && len(rh.Addresses) > 0 {
		newRequest.AppendHeader(rh)
	}
	if len(inviteRequest.GetHeaders("Route")) > 0 {
		sip.CopyHeaders("Route", inviteRequest, newRequest)
	}

	maxForwardsHeader := sip.MaxForwards(70)
	newRequest.AppendHeader(&maxForwardsHeader)
	sip.CopyHeaders("Call-ID", inviteRequest, newRequest)
	sip.CopyHeaders("CSeq", inviteRequest, newRequest)

	cseq, _ := newRequest.CSeq()
	cseq.SeqNo++
	cseq.MethodName = method

	return newRequest
}
