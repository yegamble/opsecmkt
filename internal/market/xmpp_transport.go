package market

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/xml"
	"errors"
	"io"
	"net"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

// The destination host is explicit administrator configuration. There is no SRV
// lookup or plaintext fallback, and SASL credentials are sent only after TLS has
// verified that host against the operating system's trusted certificates.
type xmppConfig struct {
	Enabled                bool
	AllowContents          bool
	ContentPolicyVersion   int64
	Sender, Host, Password string
	Port                   int
	TLSMode                string
}

const (
	xmppMaxBodyBytes = 256 * 1024
	xmppStreamNS     = "http://etherx.jabber.org/streams"
	xmppTLSNS        = "urn:ietf:params:xml:ns:xmpp-tls"
	xmppSASLNS       = "urn:ietf:params:xml:ns:xmpp-sasl"
	xmppBindNS       = "urn:ietf:params:xml:ns:xmpp-bind"
	xmppClientNS     = "jabber:client"
)

var (
	errXMPPConfig   = errors.New("XMPP configuration is invalid")
	errXMPPConnect  = errors.New("XMPP connection failed")
	errXMPPTLS      = errors.New("XMPP TLS verification or handshake failed")
	errXMPPProtocol = errors.New("XMPP server protocol failed")
	errXMPPAuth     = errors.New("XMPP authentication failed")
	errXMPPDelivery = errors.New("XMPP submission failed")
)

// validateXMPPJID accepts a conservative ASCII subset of bare JIDs. A resource,
// escaped localpart, Unicode preparation, and IP-literal JID domains are not
// supported; rejecting them avoids ambiguous normalization of stored recipients.
func validateXMPPJID(s string) bool {
	if len(s) > 254 || strings.Count(s, "@") != 1 {
		return false
	}
	local, domain, _ := strings.Cut(s, "@")
	if len(local) == 0 || len(local) > 64 || !validXMPPDomain(domain) {
		return false
	}
	for _, c := range local {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("._-+", c)) {
			return false
		}
	}
	return true
}

func validXMPPDomain(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if len(label) == 0 || len(label) > 63 || label[0] == '-' || label[len(label)-1] == '-' {
			return false
		}
		for _, c := range label {
			if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-') {
				return false
			}
		}
	}
	return true
}

func validXMPPHost(s string) bool {
	return net.ParseIP(s) != nil || validXMPPDomain(s)
}

func (c xmppConfig) valid() bool {
	return validateXMPPJID(c.Sender) && validXMPPHost(c.Host) && c.Port > 0 && c.Port <= 65535 &&
		(c.TLSMode == "starttls" || c.TLSMode == "direct") && len(c.Password) > 0 && len(c.Password) <= 4096 &&
		utf8.ValidString(c.Password) && !strings.ContainsRune(c.Password, 0)
}

func sendXMPP(ctx context.Context, cfg xmppConfig, to, id, body string) error {
	return sendXMPPWithRoots(ctx, cfg, to, id, body, nil)
}

// sendXMPPWithRoots exists for certificate-verified loopback protocol tests.
// Production always passes nil to use the system trust store.
func sendXMPPWithRoots(ctx context.Context, cfg xmppConfig, to, id, body string, roots *x509.CertPool) error {
	if !cfg.valid() || !validateXMPPJID(to) || len(id) == 0 || len(id) > 128 || len(body) > xmppMaxBodyBytes || !validXMPPText(id) || !validXMPPText(body) {
		return errXMPPConfig
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	raw, err := (&net.Dialer{}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port)))
	if err != nil {
		return errXMPPConnect
	}
	defer raw.Close()
	deadline, _ := ctx.Deadline()
	if err = raw.SetDeadline(deadline); err != nil {
		return errXMPPConnect
	}
	// Cancellation must interrupt a server stalled between protocol steps too.
	stop := context.AfterFunc(ctx, func() { raw.Close() })
	defer stop()
	var conn net.Conn = raw
	upgrade := func() error {
		secured := tls.Client(raw, &tls.Config{ServerName: cfg.Host, RootCAs: roots, MinVersion: tls.VersionTLS12})
		if secured.HandshakeContext(ctx) != nil {
			return errXMPPTLS
		}
		conn = secured
		return nil
	}
	if cfg.TLSMode == "direct" {
		if err = upgrade(); err != nil {
			return err
		}
	}
	_, domain, _ := strings.Cut(cfg.Sender, "@")
	stream := &xmppStream{conn: conn}
	features, err := stream.open(domain)
	if err != nil {
		return errXMPPProtocol
	}
	if cfg.TLSMode == "starttls" {
		if features.child(xmppTLSNS, "starttls") == nil || stream.write("<starttls xmlns='"+xmppTLSNS+"'/>") != nil {
			return errXMPPTLS
		}
		answer, err := stream.element()
		if err != nil || answer.Name != (xml.Name{Space: xmppTLSNS, Local: "proceed"}) {
			return errXMPPTLS
		}
		if err = upgrade(); err != nil {
			return err
		}
		stream = &xmppStream{conn: conn}
		if features, err = stream.open(domain); err != nil {
			return errXMPPProtocol
		}
	}
	mechanisms := features.child(xmppSASLNS, "mechanisms")
	plain := false
	if mechanisms != nil {
		for _, mechanism := range mechanisms.Children {
			plain = plain || mechanism.Name == (xml.Name{Space: xmppSASLNS, Local: "mechanism"}) && mechanism.Text == "PLAIN"
		}
	}
	if !plain {
		return errXMPPAuth
	}
	local, _, _ := strings.Cut(cfg.Sender, "@")
	credentials := base64.StdEncoding.EncodeToString([]byte("\x00" + local + "\x00" + cfg.Password))
	if stream.write("<auth xmlns='"+xmppSASLNS+"' mechanism='PLAIN'>"+credentials+"</auth>") != nil {
		return errXMPPAuth
	}
	answer, err := stream.element()
	if err != nil || answer.Name != (xml.Name{Space: xmppSASLNS, Local: "success"}) {
		return errXMPPAuth
	}
	// SASL success restarts the XML stream without closing the TLS connection.
	stream = &xmppStream{conn: conn}
	if features, err = stream.open(domain); err != nil || features.child(xmppBindNS, "bind") == nil {
		return errXMPPProtocol
	}
	// Let the server assign a unique resource so concurrent app instances or an
	// administrator's test submission cannot replace another connection.
	if stream.write("<iq type='set' id='xmpp-bind'><bind xmlns='"+xmppBindNS+"'/></iq>") != nil {
		return errXMPPProtocol
	}
	bound, err := stream.iq("xmpp-bind", "")
	if err != nil || bound.attr("type") != "result" {
		return errXMPPProtocol
	}
	bind := bound.child(xmppBindNS, "bind")
	if bind == nil || bind.child(xmppBindNS, "jid") == nil {
		return errXMPPProtocol
	}
	boundBare, resource, hasResource := strings.Cut(bind.child(xmppBindNS, "jid").Text, "/")
	// Our supported ASCII JID subset uses XMPP's case-insensitive localpart and
	// domain comparison; servers commonly return their normalized spelling.
	if !hasResource || resource == "" || !strings.EqualFold(boundBare, cfg.Sender) {
		return errXMPPProtocol
	}
	// Older servers may require the deprecated session establishment step.
	const sessionNS = "urn:ietf:params:xml:ns:xmpp-session"
	if session := features.child(sessionNS, "session"); session != nil && session.child(sessionNS, "optional") == nil {
		if stream.write("<iq type='set' id='xmpp-session'><session xmlns='"+sessionNS+"'/></iq>") != nil {
			return errXMPPProtocol
		}
		answer, err = stream.iq("xmpp-session", "")
		if err != nil || answer.attr("type") != "result" {
			return errXMPPProtocol
		}
	}
	if stream.write("<message to='"+xmppEscape(to)+"' type='chat' id='"+xmppEscape(id)+"'><body>"+xmppEscape(body)+"</body></message>"+
		"<iq to='"+xmppEscape(domain)+"' type='get' id='xmpp-submitted'><ping xmlns='urn:xmpp:ping'/></iq>") != nil {
		return errXMPPDelivery
	}
	// A reply to the following IQ establishes that the server read the preceding
	// message, even if it replies service-unavailable to the ping. This is server
	// submission, not an end-recipient delivery receipt. A network loss can still
	// require a retry with the same message ID.
	if _, err = stream.iq("xmpp-submitted", id); err != nil {
		return errXMPPDelivery
	}
	_ = stream.write("</stream:stream>")
	return nil
}

func validXMPPText(s string) bool {
	if !utf8.ValidString(s) {
		return false
	}
	for _, c := range s {
		if !(c == '\t' || c == '\n' || c == '\r' || c >= 0x20 && c <= 0xd7ff || c >= 0xe000 && c <= 0xfffd || c >= 0x10000 && c <= 0x10ffff) {
			return false
		}
	}
	return true
}

func xmppEscape(s string) string {
	var b bytes.Buffer
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}

type xmppElement struct {
	Name     xml.Name
	Attrs    []xml.Attr
	Text     string
	Children []*xmppElement
}

func (e *xmppElement) child(space, local string) *xmppElement {
	for _, c := range e.Children {
		if c.Name == (xml.Name{Space: space, Local: local}) {
			return c
		}
	}
	return nil
}

func (e *xmppElement) attr(name string) string {
	for _, a := range e.Attrs {
		if a.Name.Space == "" && a.Name.Local == name {
			return a.Value
		}
	}
	return ""
}

type xmppStream struct {
	conn net.Conn
	dec  *xml.Decoder
}

func (s *xmppStream) write(text string) error {
	_, err := io.WriteString(s.conn, text)
	return err
}

func (s *xmppStream) open(domain string) (*xmppElement, error) {
	if err := s.write("<stream:stream xmlns='jabber:client' xmlns:stream='" + xmppStreamNS + "' to='" + xmppEscape(domain) + "' version='1.0'>"); err != nil {
		return nil, errXMPPProtocol
	}
	// The whole server stream, including whitespace, XML tokens and unsolicited
	// stanzas, is bounded. Each restart has a fresh limit and a nesting cap below.
	s.dec = xml.NewDecoder(io.LimitReader(s.conn, 256*1024))
	start, err := s.start()
	if err != nil || start.Name != (xml.Name{Space: xmppStreamNS, Local: "stream"}) {
		return nil, errXMPPProtocol
	}
	features, err := s.element()
	if err != nil || features.Name != (xml.Name{Space: xmppStreamNS, Local: "features"}) {
		return nil, errXMPPProtocol
	}
	return features, nil
}

func (s *xmppStream) start() (xml.StartElement, error) {
	for {
		token, err := s.dec.Token()
		if err != nil {
			return xml.StartElement{}, errXMPPProtocol
		}
		switch t := token.(type) {
		case xml.StartElement:
			return t, nil
		case xml.CharData:
			if strings.TrimSpace(string(t)) != "" {
				return xml.StartElement{}, errXMPPProtocol
			}
		case xml.ProcInst:
			if t.Target != "xml" {
				return xml.StartElement{}, errXMPPProtocol
			}
		case xml.Comment:
		default:
			return xml.StartElement{}, errXMPPProtocol
		}
	}
}

func (s *xmppStream) element() (*xmppElement, error) {
	start, err := s.start()
	if err != nil {
		return nil, err
	}
	return s.readElement(start, 0)
}

func (s *xmppStream) readElement(start xml.StartElement, depth int) (*xmppElement, error) {
	if depth >= 16 {
		return nil, errXMPPProtocol
	}
	e := &xmppElement{Name: start.Name, Attrs: start.Attr}
	var text strings.Builder
	for {
		token, err := s.dec.Token()
		if err != nil {
			return nil, errXMPPProtocol
		}
		switch t := token.(type) {
		case xml.StartElement:
			child, err := s.readElement(t, depth+1)
			if err != nil {
				return nil, err
			}
			e.Children = append(e.Children, child)
		case xml.CharData:
			text.Write(t)
		case xml.EndElement:
			e.Text = text.String()
			return e, nil
		case xml.Comment:
		default:
			return nil, errXMPPProtocol
		}
	}
}

func (s *xmppStream) iq(id, messageID string) (*xmppElement, error) {
	for range 32 {
		e, err := s.element()
		if err != nil {
			return nil, err
		}
		if e.Name == (xml.Name{Space: xmppClientNS, Local: "iq"}) && e.attr("id") == id && (e.attr("type") == "result" || e.attr("type") == "error") {
			return e, nil
		}
		if e.Name == (xml.Name{Space: xmppStreamNS, Local: "error"}) ||
			e.Name == (xml.Name{Space: xmppClientNS, Local: "message"}) && e.attr("type") == "error" && e.attr("id") == messageID {
			return nil, errXMPPDelivery
		}
	}
	return nil, errXMPPProtocol
}
