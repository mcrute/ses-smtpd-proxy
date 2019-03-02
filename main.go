package main

import (
	"bytes"
	"fmt"
	"log"
	"os"

	"github.com/aws/aws-sdk-go/aws/session"
	"github.com/aws/aws-sdk-go/service/ses"
	"github.com/mcrute/go-smtpd/smtpd"
)

const (
	SES_SIZE_LIMIT = 10000000
	DEFAULT_ADDR   = ":2500"
)

var sesClient *ses.SES

type Envelope struct {
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
	if e.b.Len() > SES_SIZE_LIMIT { // SES limitation
		log.Println("message size %d exceeds SES limit of %d", e.b.Len(), SES_SIZE_LIMIT)
		return smtpd.SMTPError("554 5.5.1 Error: maximum message size exceeded")
	}
	return nil
}

func (e *Envelope) logMessageSend() {
	dr := make([]string, len(e.rcpts))
	for i := range e.rcpts {
		dr[i] = *e.rcpts[i]
	}
	log.Printf("sending message to %+v", dr)
}

func (e *Envelope) Close() error {
	e.logMessageSend()
	r := &ses.SendRawEmailInput{
		Destinations: e.rcpts,
		RawMessage:   &ses.RawMessage{Data: e.b.Bytes()},
	}
	_, err := sesClient.SendRawEmail(r)
	if err != nil {
		log.Printf("ERROR: ses: %v", err)
		return smtpd.SMTPError(fmt.Sprintf("554 5.5.1 Error: %v", err))
	}
	return err
}

func main() {
	sesClient = ses.New(session.New())
	addr := DEFAULT_ADDR

	if len(os.Args) == 2 {
		addr = os.Args[1]
	} else if len(os.Args) > 2 {
		log.Fatalf("usage: %s [listen_host:port]", os.Args[0])
	}

	s := &smtpd.Server{
		Addr: addr,
		OnNewMail: func(c smtpd.Connection, from smtpd.MailAddress) (smtpd.Envelope, error) {
			return &Envelope{}, nil
		},
	}
	log.Printf("ListenAndServe on %s", addr)
	err := s.ListenAndServe()
	if err != nil {
		log.Fatalf("ListenAndServe: %v", err)
	}
}
