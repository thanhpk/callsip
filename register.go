package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/ghettovoice/gosip/sip"
	"github.com/ghettovoice/gosip/sip/parser"
	"github.com/google/uuid"
	"github.com/subiz/log"
	"github.com/thanhpk/randstr"
)

type Register struct {
	ua         *UserAgent
	timer      *time.Timer
	profile    *Profile
	authorizer sip.Authorizer
	recipient  sip.SipUri
	request    *sip.Request
	ctx        context.Context
	cancel     context.CancelFunc
	data       interface{}
}

func NewRegister(ua *UserAgent, profile *Profile, recipient sip.SipUri, data interface{}) *Register {
	r := &Register{
		ua:        ua,
		profile:   profile,
		recipient: recipient,
		request:   nil,
		data:      data,
	}
	r.ctx, r.cancel = context.WithCancel(context.Background())
	return r
}

func (r *Register) SendRegister(expires uint32) error {
	profile := r.profile
	recipient := r.recipient

	from := &sip.Address{
		Uri:    profile.URI,
		Params: sip.NewParams().Add("tag", sip.String{Str: randstr.String(8)}),
	}

	to := &sip.Address{Uri: profile.URI}
	contact := profile.Contact()

	if r.request == nil || expires == 0 {
		request, err := buildRequest(sip.REGISTER, from, to, contact, recipient, profile.Routes, nil)
		if err != nil {
			log.Err("", err, "Register: err")
			return err
		}
		expiresHeader := sip.Expires(expires)
		(*request).AppendHeader(&expiresHeader)
		r.request = request
	} else {
		cseq, _ := (*r.request).CSeq()
		cseq.SeqNo++
		cseq.MethodName = sip.REGISTER

		(*r.request).RemoveHeader("Expires")
		// replace Expires header.
		expiresHeader := sip.Expires(expires)
		(*r.request).AppendHeader(&expiresHeader)
	}

	if profile.AuthInfo != nil && r.authorizer == nil {
		r.authorizer = &sip.DefaultAuthorizer{User: sip.String{Str: profile.AuthInfo.AuthUser}, Password: sip.String{Str: profile.AuthInfo.Password}}
	}
	resp, err := r.ua.RequestWithContext(r.ctx, *r.request, r.authorizer, true, 1)

	if err != nil {
		log.Err("", err, "Request Failed", sip.REGISTER)
		var code sip.StatusCode
		var reason string
		if _, ok := err.(*sip.RequestError); ok {
			reqErr := err.(*sip.RequestError)
			code = sip.StatusCode(reqErr.Code)
			reason = reqErr.Reason
		} else {
			code = 500
			reason = err.Error()
		}

		state := RegisterState{
			Account:    profile,
			Response:   nil,
			StatusCode: sip.StatusCode(code),
			Reason:     reason,
			Expiration: 0,
			UserData:   r.data,
		}
		log.Err("", err, "Request ", sip.REGISTER, ", has state", state)
	}
	if resp != nil {
		// stateCode := resp.StatusCode()
		// log.Info("", sip.REGISTER, "resp", stateCode, "=>", resp.String())
		var expires uint32 = 0
		hdrs := resp.GetHeaders("Expires")
		if len(hdrs) > 0 {
			expires = uint32(*(hdrs[0]).(*sip.Expires))
		} else {
			hdrs = resp.GetHeaders("Contact")
			if len(hdrs) > 0 {
				if cexpires, cexpirescok := (hdrs[0].(*sip.ContactHeader)).Params.Get("expires"); cexpirescok {
					cexpiresint, _ := strconv.Atoi(cexpires.String())
					expires = uint32(cexpiresint)
				}
			}
		}
		if expires > 0 {
			go func() {
				if r.timer == nil {
					r.timer = time.NewTimer(time.Second * time.Duration(expires-10))
				} else {
					r.timer.Reset(time.Second * time.Duration(expires-10))
				}
				select {
				case <-r.timer.C:
					r.SendRegister(expires)
				case <-r.ctx.Done():
					return
				}
			}()
		} else if expires == 0 {
			if r.timer != nil {
				r.timer.Stop()
				r.timer = nil
			}
			r.request = nil
		}

		// log.Info("", "Request", sip.REGISTER, "response: state =>", state)
	}
	return nil
}

func (r *Register) Stop() {
	if r.timer != nil {
		r.timer.Stop()
		r.timer = nil
	}
	r.cancel()
}

// AuthInfo .
type AuthInfo struct {
	AuthUser string
	Realm    string
	Password string
	Ha1      string
}

// RegisterState .
type RegisterState struct {
	Account    *Profile
	StatusCode sip.StatusCode
	Reason     string
	Expiration uint32
	Response   sip.Response
	UserData   interface{}
}

// Profile .
type Profile struct {
	URI           sip.Uri
	DisplayName   string
	AuthInfo      *AuthInfo
	Expires       uint32
	InstanceID    string
	Routes        []sip.Uri
	ContactURI    sip.Uri
	ContactParams map[string]string
}

// Contact .
func (p *Profile) Contact() *sip.Address {
	var uri sip.Uri
	if p.ContactURI != nil {
		uri = p.ContactURI
	} else {
		uri = p.URI.Clone()
	}

	contact := &sip.Address{
		Uri:    uri,
		Params: sip.NewParams(),
	}
	if p.InstanceID != "nil" {
		contact.Params.Add("+sip.instance", sip.String{Str: p.InstanceID})
	}

	for key, value := range p.ContactParams {
		contact.Params.Add(key, sip.String{Str: value})
	}

	//TODO: Add more necessary parameters.
	//etc: ip:port, transport=udp|tcp, +sip.ice, +sip.instance, +sip.pnsreg,

	return contact
}

// NewProfile .
func NewProfile(uri sip.Uri, displayName string, authInfo *AuthInfo, expires uint32, stack *SipStack) *Profile {
	p := &Profile{
		URI:         uri,
		DisplayName: displayName,
		AuthInfo:    authInfo,
		Expires:     expires,
	}
	if stack != nil { // populate the Contact field
		var transport string
		if tp, ok := uri.UriParams().Get("transport"); ok {
			transport = tp.String()
		} else {
			transport = "udp"
		}
		addr := stack.GetNetworkInfo(transport)
		uri, err := parser.ParseUri(fmt.Sprintf("sip:%s@%s;transport=%s", p.URI.User(), addr.Addr(), transport))
		if err == nil {
			p.ContactURI = uri
		} else {
			log.Err("", err, "Error parsing contact URI")
		}
	}

	uid, err := uuid.NewUUID()
	if err != nil {
		log.Err("", err, "could not create UUID")
	}
	p.InstanceID = fmt.Sprintf(`"<%s>"`, uid.URN())
	return p
}
