package opcua

import (
	"context"
	"crypto/rand"
	"fmt"
	mrand "math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gopcua/opcua/ua"
	"github.com/gopcua/opcua/uacp"
	"github.com/gopcua/opcua/uasc"
)

func init() {
	mrand.Seed(time.Now().UnixNano())
}

var DefaultClientConfig = uasc.NewClientConfigSecurityNone(uint32(mrand.Int31()), 3600000)

// DefaultSessionConfig is the default configuration for a session.
var DefaultSessionConfig = uasc.NewClientSessionConfig(
	[]string{"en-US"},
	&ua.AnonymousIdentityToken{PolicyID: "open62541-anonymous-policy"},
)

// GetEndpoints returns the available endpoint descriptions for the server.
func GetEndpoints(endpoint string) ([]*ua.EndpointDescription, error) {
	c := &Client{EndpointURL: endpoint}
	if err := c.Dial(c.Config); err != nil {
		return nil, err
	}
	defer c.Close()
	res, err := c.GetEndpoints()
	if err != nil {
		return nil, err
	}
	return res.Endpoints, nil
}

// Client is a high-level client for an OPC/UA server.
// It establishes a secure channel and a session.
type Client struct {
	// EndpointURL is the endpoint URL the client connects to.
	EndpointURL string

	// Config is the configuration for the secure channel.
	Config *uasc.Config

	// SessionConfig is the configuration for the session.
	// The client uses DefaultSessionConfig if not set.
	SessionConfig *uasc.SessionConfig

	// sechan is the open secure channel.
	sechan *uasc.SecureChannel

	// session is the active session.
	session atomic.Value // *Session

	// once initializes session
	once sync.Once
}

// Connect establishes a secure channel and creates a new session.
func (c *Client) Connect() (err error) {
	if c.sechan != nil {
		return fmt.Errorf("already connected")
	}
	if err := c.Dial(c.Config); err != nil {
		return err
	}
	s, err := c.CreateSession(c.SessionConfig)
	if err != nil {
		_ = c.Close()
		return err
	}
	if err := c.ActivateSession(s); err != nil {
		_ = c.Close()
		return err
	}
	return nil
}

// Dial establishes a secure channel.
func (c *Client) Dial(cfg *uasc.Config) error {
	c.once.Do(func() { c.session.Store((*Session)(nil)) })
	if c.sechan != nil {
		return fmt.Errorf("secure channel already connected")
	}
	conn, err := uacp.Dial(context.Background(), c.EndpointURL)
	if err != nil {
		return err
	}
	if cfg == nil {
		cfg = DefaultClientConfig
	}
	sechan, err := uasc.NewSecureChannel(c.EndpointURL, conn, cfg)
	if err != nil {
		_ = conn.Close()
		return err
	}
	if err := sechan.Open(); err != nil {
		_ = conn.Close()
		return err
	}
	c.sechan = sechan
	return nil
}

// Close closes the session and the secure channel.
func (c *Client) Close() error {
	// try to close the session but ignore any error
	// so that we close the underlying channel and connection.
	_ = c.CloseSession()
	return c.sechan.Close()
}

// Session returns the active session.
func (c *Client) Session() *Session {
	return c.session.Load().(*Session)
}

// Session is a OPC/UA session as described in Part 4, 5.6.
type Session struct {
	cfg *uasc.SessionConfig

	// resp is the response to the CreateSession request which contains all
	// necessary parameters to activate the session.
	resp *ua.CreateSessionResponse

	// mySignature is is the client/serverSignature expected to receive from
	// the other endpoint. This parameter is automatically calculated and kept
	// temporarily until being used to verify received client/serverSignature.
	mySignature *ua.SignatureData

	// signatureToSend is the client/serverSignature defined in Part4, Table 15
	// and Table 17. This parameter is automatically calculated and kept
	// temporarily until it is sent in next message.
	signatureToSend *ua.SignatureData
}

// CreateSession creates a new session which is not yet activated and not
// associated with the client. Call ActivateSession to both activate and
// associate the session with the client
//
// See Part 4, 5.6.2
func (c *Client) CreateSession(cfg *uasc.SessionConfig) (*Session, error) {
	if c.sechan == nil {
		return nil, fmt.Errorf("secure channel not connected")
	}
	if cfg == nil {
		cfg = DefaultSessionConfig
	}

	var clientCert []byte
	if c.Config != nil {
		clientCert = c.Config.Certificate
	}

	nonce := make([]byte, 32)
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}

	req := &ua.CreateSessionRequest{
		ClientDescription: cfg.ClientDescription,
		EndpointURL:       c.EndpointURL,
		SessionName:       fmt.Sprintf("gopcua-%d", time.Now().UnixNano()),
		ClientNonce:       nonce,
		ClientCertificate: clientCert,
	}

	var s *Session
	// for the CreateSessionRequest the authToken is always nil.
	// use c.sechan.Send() to enforce this.
	err := c.sechan.Send(req, nil, func(v interface{}) error {
		resp, ok := v.(*ua.CreateSessionResponse)
		if !ok {
			return fmt.Errorf("invalid response. Got %T, want CreateSessionResponse", v)
		}

		s = &Session{
			cfg:             cfg,
			resp:            resp,
			mySignature:     &ua.SignatureData{},
			signatureToSend: &ua.SignatureData{},
		}
		// todo(dw): fix crypto: calculate signature data
		// s.signatureToSend = ua.NewSignatureDataFrom(resp.ServerCertificate, resp.ServerNonce)
		// s.mySignature = ua.NewSignatureDataFrom(s.sechan.cfg.Certificate, nonce)
		return nil
	})
	return s, err
}

// ActivateSession activates the session and associates it with the client. If
// the client already has a session it will be closed. To retain the current
// session call DetachSession.
//
// See Part 4, 5.6.3
func (c *Client) ActivateSession(s *Session) error {
	req := &ua.ActivateSessionRequest{
		ClientSignature:            s.signatureToSend,
		ClientSoftwareCertificates: nil,
		LocaleIDs:                  s.cfg.LocaleIDs,
		UserIdentityToken:          ua.NewExtensionObject(s.cfg.UserIdentityToken),
		UserTokenSignature:         s.cfg.UserTokenSignature,
	}
	return c.sechan.Send(req, s.resp.AuthenticationToken, func(v interface{}) error {
		_, ok := v.(*ua.ActivateSessionResponse)
		if !ok {
			return fmt.Errorf("invalid response. Got %T, want ActivateSessionResponse", v)
		}
		// todo(dw): retain resp.ServerNonce for next ActivateSession call
		// e.g. s.serverNonce = resp.ServerNonce
		if err := c.CloseSession(); err != nil {
			// try to close the newly created session but report
			// only the initial error.
			_ = c.closeSession(s)
			return err
		}
		c.session.Store(s)
		return nil
	})
}

// CloseSession closes the current session.
//
// See Part 4, 5.6.4
func (c *Client) CloseSession() error {
	if err := c.closeSession(c.Session()); err != nil {
		return err
	}
	c.session.Store((*Session)(nil))
	return nil
}

// closeSession closes the given session.
func (c *Client) closeSession(s *Session) error {
	if s == nil {
		return nil
	}
	req := &ua.CloseSessionRequest{DeleteSubscriptions: true}
	return c.Send(req, func(v interface{}) error {
		_, ok := v.(*ua.CloseSessionResponse)
		if !ok {
			return fmt.Errorf("invalid response. Got %T, want ActivateSessionResponse", v)
		}
		return nil
	})
}

// DetachSession removes the session from the client without closing it. The
// caller is responsible to close or re-activate the session. If the client
// does not have an active session the function returns no error.
func (c *Client) DetachSession() (*Session, error) {
	s := c.Session()
	c.session.Store(nil)
	return s, nil
}

// Send sends the request via the secure channel and registers a handler for
// the response. If the client has an active session it injects the
// authenticaton token.
func (c *Client) Send(req interface{}, h func(interface{}) error) error {
	var authToken *ua.NodeID
	if s := c.Session(); s != nil {
		authToken = s.resp.AuthenticationToken
	}
	return c.sechan.Send(req, authToken, h)
}

// Node returns a node object which accesses its attributes
// through this client connection.
func (c *Client) Node(id *ua.NodeID) *Node {
	return &Node{ID: id, c: c}
}

func (c *Client) GetEndpoints() (*ua.GetEndpointsResponse, error) {
	req := &ua.GetEndpointsRequest{
		EndpointURL: c.EndpointURL,
	}
	var res *ua.GetEndpointsResponse
	err := c.Send(req, func(v interface{}) error {
		r, ok := v.(*ua.GetEndpointsResponse)
		if !ok {
			return fmt.Errorf("invalid response: %T", v)
		}
		res = r
		return nil
	})
	return res, err
}

// Read executes a synchronous read request.
//
// By default, the function requests the value of the nodes
// in the default encoding of the server.
func (c *Client) Read(req *ua.ReadRequest) (*ua.ReadResponse, error) {
	// clone the request and the ReadValueIDs to set defaults without
	// manipulating them in-place.
	rvs := make([]*ua.ReadValueID, len(req.NodesToRead))
	for i, rv := range req.NodesToRead {
		rc := &ua.ReadValueID{}
		*rc = *rv
		if rc.AttributeID == 0 {
			rc.AttributeID = ua.AttributeIDValue
		}
		if rc.DataEncoding == nil {
			rc.DataEncoding = &ua.QualifiedName{}
		}
		rvs[i] = rc
	}
	req = &ua.ReadRequest{
		MaxAge:             req.MaxAge,
		TimestampsToReturn: req.TimestampsToReturn,
		NodesToRead:        rvs,
	}

	var res *ua.ReadResponse
	err := c.Send(req, func(v interface{}) error {
		r, ok := v.(*ua.ReadResponse)
		if !ok {
			return fmt.Errorf("invalid response: %T", v)
		}
		res = r
		return nil
	})
	return res, err
}

// Write executes a synchronous write request.
func (c *Client) Write(req *ua.WriteRequest) (res *ua.WriteResponse, err error) {
	err = c.Send(req, func(v interface{}) error {
		r, ok := v.(*ua.WriteResponse)
		if !ok {
			return fmt.Errorf("invalid response: %T", v)
		}
		res = r
		return nil
	})
	return
}

// Browse executes a synchronous browse request.
func (c *Client) Browse(req *ua.BrowseRequest) (*ua.BrowseResponse, error) {
	var res *ua.BrowseResponse
	err := c.Send(req, func(v interface{}) error {
		r, ok := v.(*ua.BrowseResponse)
		if !ok {
			return fmt.Errorf("invalid response: %T", v)
		}
		res = r
		return nil
	})
	return res, err
}

// todo(fs): this is not done yet since we need to be able to register
// todo(fs): monitored items.
type Subscription struct {
	res *ua.CreateSubscriptionResponse
}

// todo(fs): return subscription object with channel
func (c *Client) Subscribe(intv time.Duration) (*Subscription, error) {
	req := &ua.CreateSubscriptionRequest{
		RequestedPublishingInterval: float64(intv / time.Millisecond),
		RequestedLifetimeCount:      60,
		RequestedMaxKeepAliveCount:  20,
		PublishingEnabled:           true,
	}

	var res *ua.CreateSubscriptionResponse
	err := c.Send(req, func(v interface{}) error {
		r, ok := v.(*ua.CreateSubscriptionResponse)
		if !ok {
			return fmt.Errorf("invalid response: %T", v)
		}
		res = r
		return nil
	})
	return &Subscription{res}, err
}
