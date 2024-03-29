// Copyright 2011 The go-smtpd Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//
// Originally from https://github.com/bradfitz/go-smtpd

// Package smtpd implements an SMTP server. Hooks are provided to customize
// its behavior.
package smtpd

// TODO:
//  -- send 421 to connected clients on graceful server shutdown (s3.8)
//

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"regexp"
	"strings"
	"time"
	"unicode"
)

var (
	rcptToRE   = regexp.MustCompile(`[Tt][Oo]:\s*<(.+)>`)
	mailFromRE = regexp.MustCompile(`[Ff][Rr][Oo][Mm]:\s*<(.*)>`)
)

// Server is an SMTP server.
type Server struct {
	Addr         string        // TCP address to listen on, ":25" if empty
	Hostname     string        // optional Hostname to announce; "" to use system hostname
	ReadTimeout  time.Duration // optional read timeout
	WriteTimeout time.Duration // optional write timeout

	StartTLS *tls.Config // advertise STARTTLS and use the given config to upgrade the connection with

	// OnNewConnection, if non-nil, is called on new connections.
	// If it returns non-nil, the connection is closed.
	OnNewConnection func(c Connection) error

	// OnNewMail must be defined and is called when a new message beings.
	// (when a MAIL FROM line arrives)
	OnNewMail func(c Connection, from MailAddress) (Envelope, error)

	OnAuthentication func(c Connection, user string, password string) error
}

// MailAddress is defined by
type MailAddress interface {
	Email() string    // email address, as provided
	Hostname() string // canonical hostname, lowercase
}

// Connection is implemented by the SMTP library and provided to callers
// customizing their own Servers.
type Connection interface {
	IsAuthenticated() bool
	Addr() net.Addr
	Close() error // to force-close a connection
}

type Envelope interface {
	AddRecipient(rcpt MailAddress) error
	BeginData() error
	Write(line []byte) error
	Close() error
}

type BasicEnvelope struct {
	rcpts []MailAddress
}

func (e *BasicEnvelope) AddRecipient(rcpt MailAddress) error {
	e.rcpts = append(e.rcpts, rcpt)
	return nil
}

func (e *BasicEnvelope) BeginData() error {
	if len(e.rcpts) == 0 {
		return SMTPError("554 5.5.1 Error: no valid recipients")
	}
	return nil
}

func (e *BasicEnvelope) Write(line []byte) error {
	log.Printf("Line: %q", string(line))
	return nil
}

func (e *BasicEnvelope) Close() error {
	return nil
}

func (srv *Server) hostname() string {
	if srv.Hostname != "" {
		return srv.Hostname
	}
	out, err := os.Hostname()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// ListenAndServe listens on the TCP network address srv.Addr and then
// calls Serve to handle requests on incoming connections.  If
// srv.Addr is blank, ":25" is used.
func (srv *Server) ListenAndServe() error {
	addr := srv.Addr
	if addr == "" {
		addr = ":25"
	}
	ln, e := net.Listen("tcp", addr)
	if e != nil {
		return e
	}
	return srv.Serve(ln)
}

func (srv *Server) Serve(ln net.Listener) error {
	defer ln.Close()
	for {
		rw, e := ln.Accept()
		if e != nil {
			if ne, ok := e.(net.Error); ok && ne.Temporary() {
				log.Printf("smtpd: Accept error: %v", e)
				continue
			}
			return e
		}
		sess, err := srv.newSession(rw)
		if err != nil {
			continue
		}
		go sess.serve()
	}
	panic("not reached")
}

type session struct {
	srv *Server
	rwc net.Conn
	br  *bufio.Reader
	bw  *bufio.Writer

	env Envelope // current envelope, or nil

	helloType     string
	helloHost     string
	authenticated string
}

func (srv *Server) newSession(rwc net.Conn) (s *session, err error) {
	s = &session{
		srv: srv,
		rwc: rwc,
		br:  bufio.NewReader(rwc),
		bw:  bufio.NewWriter(rwc),
	}
	return
}

func (s *session) IsAuthenticated() bool {
	return s.authenticated != ""
}

func (s *session) errorf(format string, args ...interface{}) {
	log.Printf("Client error: "+format, args...)
}

func (s *session) sendf(format string, args ...interface{}) {
	if s.srv.WriteTimeout != 0 {
		s.rwc.SetWriteDeadline(time.Now().Add(s.srv.WriteTimeout))
	}
	fmt.Fprintf(s.bw, format, args...)
	s.bw.Flush()
}

func (s *session) sendlinef(format string, args ...interface{}) {
	s.sendf(format+"\r\n", args...)
}

func (s *session) sendSMTPErrorOrLinef(err error, format string, args ...interface{}) {
	if se, ok := err.(SMTPError); ok {
		s.sendlinef("%s", se.Error())
		return
	}
	s.sendlinef(format, args...)
}

func (s *session) Addr() net.Addr {
	return s.rwc.RemoteAddr()
}

func (s *session) Close() error { return s.rwc.Close() }

func (s *session) serve() {
	defer s.rwc.Close()
	if onc := s.srv.OnNewConnection; onc != nil {
		if err := onc(s); err != nil {
			s.sendSMTPErrorOrLinef(err, "554 connection rejected")
			return
		}
	}
	s.sendf("220 %s ESMTP gosmtpd\r\n", s.srv.hostname())
	for {
		if s.srv.ReadTimeout != 0 {
			s.rwc.SetReadDeadline(time.Now().Add(s.srv.ReadTimeout))
		}
		sl, err := s.br.ReadSlice('\n')
		if err != nil {
			s.errorf("read error: %v", err)
			return
		}
		line := cmdLine(string(sl))
		if err := line.checkValid(); err != nil {
			s.sendlinef("500 %v", err)
			continue
		}

		switch line.Verb() {
		case "HELO", "EHLO":
			s.handleHello(line.Verb(), line.Arg())
		case "STARTTLS":
			if s.srv.StartTLS == nil {
				s.sendlinef("502 5.5.2 Error: command not recognized")
				continue
			}
			s.sendlinef("220 Ready to start TLS")
			if err := s.handleStartTLS(); err != nil {
				s.errorf("failed to start tls: %s", err)
				s.sendSMTPErrorOrLinef(err, "550 ??? failed")
			}
		case "QUIT":
			s.sendlinef("221 2.0.0 Bye")
			return
		case "RSET":
			s.env = nil
			s.sendlinef("250 2.0.0 OK")
		case "NOOP":
			s.sendlinef("250 2.0.0 OK")
		case "MAIL":
			if !s.validateAuth() {
				return
			}
			arg := line.Arg() // "From:<foo@bar.com>"
			m := mailFromRE.FindStringSubmatch(arg)
			if m == nil {
				log.Printf("invalid MAIL arg: %q", arg)
				s.sendlinef("501 5.1.7 Bad sender address syntax")
				continue
			}
			s.handleMailFrom(m[1])
		case "RCPT":
			if !s.validateAuth() {
				return
			}
			s.handleRcpt(line)
		case "AUTH":
			s.handleAuth(line)
		case "DATA":
			if !s.validateAuth() {
				return
			}
			s.handleData()
		default:
			log.Printf("Client: %q, verhb: %q", line, line.Verb())
			s.sendlinef("502 5.5.2 Error: command not recognized")
		}
	}
}

func (s *session) handleStartTLS() error {
	tlsConn := tls.Server(s.rwc, s.srv.StartTLS)
	err := tlsConn.Handshake()
	if err != nil {
		return err
	}
	s.rwc = net.Conn(tlsConn)
	s.bw.Reset(s.rwc)
	s.br.Reset(s.rwc)
	return nil
}

func (s *session) handleHello(greeting, host string) {
	s.helloType = greeting
	s.helloHost = host
	fmt.Fprintf(s.bw, "250-%s\r\n", s.srv.hostname())
	extensions := []string{}
	if s.srv.OnAuthentication != nil {
		extensions = append(extensions, "250-AUTH PLAIN")
	}
	if s.srv.StartTLS != nil {
		extensions = append(extensions, "250-STARTTLS")
	}
	extensions = append(extensions, "250-PIPELINING",
		"250-SIZE 10240000",
		"250-ENHANCEDSTATUSCODES",
		"250-8BITMIME",
		"250 DSN")
	for _, ext := range extensions {
		fmt.Fprintf(s.bw, "%s\r\n", ext)
	}
	s.bw.Flush()
}

func (s *session) handleAuth(line cmdLine) {
	ah := s.srv.OnAuthentication
	if ah == nil {
		log.Printf("smtp: Server.OnAuthentication is nil; rejecting AUTH")
		s.sendlinef("502 5.5.2 Error: command not recognized")
		return
	}

	if ah != nil && s.IsAuthenticated() {
		log.Printf("smtp: invalid second AUTH on connection")
		s.sendlinef("503 5.5.1 Error: unable to AUTH more than once")
		return
	}

	p := strings.Split(line.Arg(), " ")
	if len(p) != 2 && p[0] != "PLAIN" {
		log.Printf("smtp: invalid AUTH argument format")
		s.sendlinef("502 5.5.2 Error: command not recognized")
		return
	}

	c, err := base64.StdEncoding.DecodeString(p[1])
	if err != nil {
		log.Printf("smtp: error decoding credentials %v", err)
		s.sendlinef("535 5.7.8 Authentication credentials invalid")
		return
	}

	cp := bytes.Split(c, []byte{0})
	if len(cp) != 3 {
		log.Printf("smtp: invalid decoded username and password")
		s.sendlinef("535 5.7.8 Authentication credentials invalid")
		return
	}

	user := string(cp[1])
	if err := ah(s, user, string(cp[2])); err != nil {
		log.Printf("smtp: authentication failed: %v", err)
		s.sendlinef("535 5.7.8 Authentication credentials invalid")
		return
	}

	s.authenticated = user
	log.Printf("smtp: successfully authenticated %s", user)
	s.sendlinef("235 2.7.0 Authentication Succeeded")
}

func (s *session) validateAuth() bool {
	if s.srv.OnAuthentication == nil {
		return true
	}
	if s.srv.OnAuthentication != nil && !s.IsAuthenticated() {
		log.Printf("smtp: authentication required but session not authenticated; rejecting")
		s.sendlinef("530 5.7.0  Authentication required")
		return false
	}
	return true
}

func (s *session) handleMailFrom(email string) {
	// TODO: 4.1.1.11.  If the server SMTP does not recognize or
	// cannot implement one or more of the parameters associated
	// qwith a particular MAIL FROM or RCPT TO command, it will return
	// code 555.

	if s.env != nil {
		s.sendlinef("503 5.5.1 Error: nested MAIL command")
		return
	}
	cb := s.srv.OnNewMail
	if cb == nil {
		log.Printf("smtp: Server.OnNewMail is nil; rejecting MAIL FROM")
		s.sendf("451 Server.OnNewMail not configured\r\n")
		return
	}
	s.env = nil
	env, err := cb(s, addrString(email))
	if err != nil {
		log.Printf("rejecting MAIL FROM %q: %v", email, err)
		s.sendf("451 denied\r\n")

		s.bw.Flush()
		time.Sleep(100 * time.Millisecond)
		s.rwc.Close()
		return
	}
	s.env = env
	s.sendlinef("250 2.1.0 Ok")
}

func (s *session) handleRcpt(line cmdLine) {
	// TODO: 4.1.1.11.  If the server SMTP does not recognize or
	// cannot implement one or more of the parameters associated
	// qwith a particular MAIL FROM or RCPT TO command, it will return
	// code 555.

	if s.env == nil {
		s.sendlinef("503 5.5.1 Error: need MAIL command")
		return
	}
	arg := line.Arg() // "To:<foo@bar.com>"
	m := rcptToRE.FindStringSubmatch(arg)
	if m == nil {
		log.Printf("bad RCPT address: %q", arg)
		s.sendlinef("501 5.1.7 Bad sender address syntax")
		return
	}
	err := s.env.AddRecipient(addrString(m[1]))
	if err != nil {
		s.sendSMTPErrorOrLinef(err, "550 bad recipient")
		return
	}
	s.sendlinef("250 2.1.0 Ok")
}

func (s *session) handleData() {
	if s.env == nil {
		s.sendlinef("503 5.5.1 Error: need RCPT command")
		return
	}
	if err := s.env.BeginData(); err != nil {
		s.handleError(err)
		return
	}
	s.sendlinef("354 Go ahead")
	for {
		sl, err := s.br.ReadSlice('\n')
		if err != nil {
			s.errorf("read error: %v", err)
			return
		}
		if bytes.Equal(sl, []byte(".\r\n")) {
			break
		}
		if sl[0] == '.' {
			sl = sl[1:]
		}
		err = s.env.Write(sl)
		if err != nil {
			s.sendSMTPErrorOrLinef(err, "550 ??? failed")
			return
		}
	}
	if err := s.env.Close(); err != nil {
		s.handleError(err)
		return
	}
	s.sendlinef("250 2.0.0 Ok: queued")
	s.env = nil
}

func (s *session) handleError(err error) {
	if se, ok := err.(SMTPError); ok {
		s.sendlinef("%s", se)
		return
	}
	log.Printf("Error: %s", err)
	s.env = nil
}

type addrString string

func (a addrString) Email() string {
	return string(a)
}

func (a addrString) Hostname() string {
	e := string(a)
	if idx := strings.Index(e, "@"); idx != -1 {
		return strings.ToLower(e[idx+1:])
	}
	return ""
}

type cmdLine string

func (cl cmdLine) checkValid() error {
	if !strings.HasSuffix(string(cl), "\r\n") {
		return errors.New(`line doesn't end in \r\n`)
	}
	// Check for verbs defined not to have an argument
	// (RFC 5321 s4.1.1)
	switch cl.Verb() {
	case "RSET", "DATA", "QUIT":
		if cl.Arg() != "" {
			return errors.New("unexpected argument")
		}
	}
	return nil
}

func (cl cmdLine) Verb() string {
	s := string(cl)
	if idx := strings.Index(s, " "); idx != -1 {
		return strings.ToUpper(s[:idx])
	}
	return strings.ToUpper(s[:len(s)-2])
}

func (cl cmdLine) Arg() string {
	s := string(cl)
	if idx := strings.Index(s, " "); idx != -1 {
		return strings.TrimRightFunc(s[idx+1:len(s)-2], unicode.IsSpace)
	}
	return ""
}

func (cl cmdLine) String() string {
	return string(cl)
}

type SMTPError string

func (e SMTPError) Error() string {
	return string(e)
}
