package main

import (
	"bytes"
	"fmt"
	"log"
	"os"

	"code.crute.us/mcrute/ses-smtpd-proxy/smtpd"
	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ses"
)

const (
	SesSizeLimit = 10000000
	DefaultAddr  = ":2500"
)

var sesClient *ses.SES

type Envelope struct {
	from  string
	rcpts []*string
	b     bytes.Buffer
}

func (e *Envelope) AddRecipient(rcpt smtpd.MailAddress) error {
	email := rcpt.Email()
	e.rcpts = append(e.rcpts, &email)
	return nil
}

func (e *Envelope) BeginData() error {
	if len(e.rcpts) == 0 {
		return smtpd.SMTPError("554 5.5.1 Error: no valid recipients")
	}
	return nil
}

func (e *Envelope) Write(line []byte) error {
	e.b.Write(line)
	if e.b.Len() > SesSizeLimit { // SES limitation
		log.Printf("message size %d exceeds SES limit of %d\n", e.b.Len(), SesSizeLimit)
		return smtpd.SMTPError("554 5.5.1 Error: maximum message size exceeded")
	}
	return nil
}

func (e *Envelope) logMessageSend() {
	dr := make([]string, len(e.rcpts))
	for i := range e.rcpts {
		dr[i] = *e.rcpts[i]
	}
	log.Printf("sending message from %+v to %+v", e.from, dr)
}

func (e *Envelope) Close() error {
	e.logMessageSend()
	r := &ses.SendRawEmailInput{
		Source:       &e.from,
		Destinations: e.rcpts,
		RawMessage:   &ses.RawMessage{Data: e.b.Bytes()},
	}
	_, err := sesClient.SendRawEmail(r)
	if err != nil {
		log.Printf("ERROR: ses: %v", err)
		return smtpd.SMTPError(fmt.Sprintf("451 4.5.1 Temporary server error. Please try again later: %v", err))
	}
	return err
}

func main() {
	sesClient = ses.New(session.Must(session.NewSession()))
	addr := DefaultAddr

	if len(os.Args) == 2 {
		addr = os.Args[1]
	} else if len(os.Args) > 2 {
		log.Fatalf("usage: %s [listen_host:port]", os.Args[0])
	}

	s := &smtpd.Server{
		Addr: addr,
		OnNewMail: func(c smtpd.Connection, from smtpd.MailAddress) (smtpd.Envelope, error) {
			return &Envelope{from: from.Email()}, nil
		},
	}
	log.Printf("ListenAndServe on %s", addr)
	err := s.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
