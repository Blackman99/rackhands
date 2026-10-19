// Package redfish is a small Redfish (DMTF DSP0266) HTTP client: basic or
// session auth, ETag-aware PATCH, collection expansion. No vendor logic.
package redfish

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

// Options configure a Client.
type Options struct {
	BaseURL        string // https://10.0.0.5 or 10.0.0.5:8443
	Username       string
	Password       string
	Insecure       bool   // skip TLS verification (self-signed BMC certs)
	CAFile         string // PEM bundle to trust instead
	TLSFingerprint string // SHA-256 of leaf cert (hex / colon-hex / sha256:); pin wins over Insecure/CAFile
	Session        bool   // use SessionService (X-Auth-Token) instead of basic auth
	Timeout        time.Duration
}

// Client talks to one Redfish service.
type Client struct {
	base  *url.URL
	opt   Options
	http  *http.Client
	mu    sync.Mutex
	token string
	sess  string // session resource URI, deleted on Close
}

// ErrUnreachable wraps transport-level failures (connection refused, TLS,
// timeouts) and services that do not answer /redfish/v1 at all.
var ErrUnreachable = errors.New("redfish service unreachable")

// HTTPError is a non-2xx answer.
type HTTPError struct {
	Method, Path string
	Status       int
	Message      string
}

func (e *HTTPError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s %s: HTTP %d: %s", e.Method, e.Path, e.Status, e.Message)
	}
	return fmt.Sprintf("%s %s: HTTP %d", e.Method, e.Path, e.Status)
}

// IsStatus reports whether err is an HTTPError with the given status.
func IsStatus(err error, status int) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == status
}

// New builds a client. It does not contact the service.
func New(o Options) (*Client, error) {
	raw := o.BaseURL
	if !strings.Contains(raw, "://") {
		raw = "https://" + raw
	}
	u, err := url.Parse(strings.TrimRight(raw, "/"))
	if err != nil || u.Host == "" {
		return nil, fmt.Errorf("bad redfish address %q", o.BaseURL)
	}
	if o.Timeout == 0 {
		o.Timeout = 20 * time.Second
	}
	tlsCfg := &tls.Config{MinVersion: tls.VersionTLS12}
	if o.TLSFingerprint != "" {
		pin, err := NormalizeFingerprint(o.TLSFingerprint)
		if err != nil {
			return nil, err
		}
		// Pin wins: only the leaf SHA-256 is trusted. InsecureSkipVerify is
		// solely the hook so VerifyPeerCertificate runs without system CAs;
		// a mismatch still fails closed (never falls through to insecure).
		tlsCfg.InsecureSkipVerify = true //nolint:gosec // custom pin verifier enforces trust
		tlsCfg.VerifyPeerCertificate = pinVerifier(pin)
	} else {
		tlsCfg.InsecureSkipVerify = o.Insecure //nolint:gosec // opt-in for self-signed BMCs
		if o.CAFile != "" {
			pem, err := os.ReadFile(o.CAFile)
			if err != nil {
				return nil, err
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("no certificates in %s", o.CAFile)
			}
			tlsCfg.RootCAs = pool
		}
	}
	tr := &http.Transport{
		TLSClientConfig:     tlsCfg,
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
		MaxIdleConnsPerHost: 4, // BMCs are tiny web servers; be gentle
	}
	return &Client{base: u, opt: o, http: &http.Client{Transport: tr, Timeout: o.Timeout}}, nil
}

// Host returns host[:port] of the service.
func (c *Client) Host() string { return c.base.Host }

// Username is the configured login (used to find our own account).
func (c *Client) Username() string { return c.opt.Username }

func (c *Client) url(path string) string {
	if strings.HasPrefix(path, "http://") || strings.HasPrefix(path, "https://") {
		return path
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return c.base.String() + path
}

func (c *Client) login(ctx context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token != "" {
		return nil
	}
	body, _ := json.Marshal(map[string]string{"UserName": c.opt.Username, "Password": c.opt.Password})
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, c.url("/redfish/v1/SessionService/Sessions"), bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode/100 != 2 {
		return &HTTPError{Method: "POST", Path: "/redfish/v1/SessionService/Sessions", Status: resp.StatusCode, Message: "session login failed"}
	}
	c.token = resp.Header.Get("X-Auth-Token")
	c.sess = resp.Header.Get("Location")
	if c.token == "" {
		return errors.New("session login returned no X-Auth-Token")
	}
	return nil
}

// Close ends the session, if one was opened.
func (c *Client) Close(ctx context.Context) {
	c.mu.Lock()
	sess, tok := c.sess, c.token
	c.token, c.sess = "", ""
	c.mu.Unlock()
	if sess == "" || tok == "" {
		return
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodDelete, c.url(sess), nil)
	req.Header.Set("X-Auth-Token", tok)
	if resp, err := c.http.Do(req); err == nil {
		resp.Body.Close()
	}
}

// Response is a decoded reply.
type Response struct {
	Status   int
	Header   http.Header
	Body     []byte
	Location string
}

// Do sends a request with JSON body (nil for none) and extra headers. With
// session auth, a 401 on a token that used to work (the BMC restarted, e.g.
// to apply its own firmware, and forgot every session) logs in once more and
// retries the request once.
func (c *Client) Do(ctx context.Context, method, path string, body any, hdr map[string]string) (*Response, error) {
	r, tok, err := c.do(ctx, method, path, body, hdr)
	if c.opt.Session && tok != "" && IsStatus(err, http.StatusUnauthorized) {
		c.mu.Lock()
		if c.token == tok {
			c.token, c.sess = "", ""
		}
		c.mu.Unlock()
		r, _, err = c.do(ctx, method, path, body, hdr)
	}
	return r, err
}

func (c *Client) do(ctx context.Context, method, path string, body any, hdr map[string]string) (*Response, string, error) {
	if c.opt.Session {
		if err := c.login(ctx); err != nil {
			return nil, "", err
		}
	}
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, "", err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.url(path), rd)
	if err != nil {
		return nil, "", err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("OData-Version", "4.0")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	c.mu.Lock()
	tok := c.token
	c.mu.Unlock()
	if tok != "" {
		req.Header.Set("X-Auth-Token", tok)
	} else if c.opt.Username != "" {
		req.SetBasicAuth(c.opt.Username, c.opt.Password)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, tok, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if err != nil {
		return nil, tok, fmt.Errorf("%w: %v", ErrUnreachable, err)
	}
	r := &Response{Status: resp.StatusCode, Header: resp.Header, Body: data, Location: resp.Header.Get("Location")}
	if resp.StatusCode/100 != 2 {
		return r, tok, &HTTPError{Method: method, Path: path, Status: resp.StatusCode, Message: extendedMessage(data)}
	}
	return r, tok, nil
}

// ExtendedMessage is the human message of a Redfish error or
// @Message.ExtendedInfo body ("" if there is none).
func ExtendedMessage(b []byte) string {
	if m := extendedMessage(b); m != "" {
		return m
	}
	var ok struct {
		Ext []struct {
			Message string `json:"Message"`
		} `json:"@Message.ExtendedInfo"`
	}
	if json.Unmarshal(b, &ok) == nil && len(ok.Ext) > 0 {
		return ok.Ext[0].Message
	}
	return ""
}

// extendedMessage pulls the human message out of a Redfish error body.
func extendedMessage(b []byte) string {
	var e struct {
		Error struct {
			Message string `json:"message"`
			Ext     []struct {
				Message string `json:"Message"`
			} `json:"@Message.ExtendedInfo"`
		} `json:"error"`
	}
	if json.Unmarshal(b, &e) != nil {
		return ""
	}
	if len(e.Error.Ext) > 0 && e.Error.Ext[0].Message != "" {
		return e.Error.Ext[0].Message
	}
	return e.Error.Message
}

// Get fetches path into a generic object.
func (c *Client) Get(ctx context.Context, path string) (Object, error) {
	r, err := c.Do(ctx, http.MethodGet, path, nil, nil)
	if err != nil {
		return nil, err
	}
	var o Object
	if err := json.Unmarshal(r.Body, &o); err != nil {
		return nil, fmt.Errorf("GET %s: invalid JSON: %v", path, err)
	}
	if et := r.Header.Get("ETag"); et != "" {
		o["@rackhands.etag"] = et
	}
	return o, nil
}

// Members follows a collection and fetches every member (bounded concurrency).
func (c *Client) Members(ctx context.Context, path string) ([]Object, error) {
	return c.MembersN(ctx, path, 0)
}

// MembersN is Members limited to the last n members (n <= 0: all). Job
// queues keep hundreds of finished entries; the newest are last.
func (c *Client) MembersN(ctx context.Context, path string, n int) ([]Object, error) {
	col, err := c.Get(ctx, path)
	if err != nil {
		return nil, err
	}
	refs := col.Refs("Members")
	if n > 0 && len(refs) > n {
		refs = refs[len(refs)-n:]
	}
	out := make([]Object, len(refs))
	errs := make([]error, len(refs))
	sem := make(chan struct{}, 4)
	var wg sync.WaitGroup
	for i, ref := range refs {
		wg.Add(1)
		go func(i int, ref string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			out[i], errs[i] = c.Get(ctx, ref)
		}(i, ref)
	}
	wg.Wait()
	res := out[:0]
	for i, o := range out {
		if errs[i] != nil {
			if errors.Is(errs[i], ErrUnreachable) {
				return nil, errs[i]
			}
			continue // one broken member must not hide the rest
		}
		res = append(res, o)
	}
	return res, nil
}

// Patch sends a PATCH with If-Match when the resource carries an ETag
// (Lenovo XCC and newer iDRAC firmware reject PATCH without it).
func (c *Client) Patch(ctx context.Context, path string, body any) (*Response, error) {
	hdr := map[string]string{}
	if cur, err := c.Get(ctx, path); err == nil {
		if et := cur.Str("@rackhands.etag"); et != "" {
			hdr["If-Match"] = et
		} else if et := cur.Str("@odata.etag"); et != "" {
			hdr["If-Match"] = et
		}
	}
	r, err := c.Do(ctx, http.MethodPatch, path, body, hdr)
	if err != nil && IsStatus(err, http.StatusPreconditionFailed) && hdr["If-Match"] != "" {
		// Some firmware hands out weak ETags it will not accept back.
		return c.Do(ctx, http.MethodPatch, path, body, map[string]string{"If-Match": "*"})
	}
	return r, err
}

// Post sends an action/collection POST.
func (c *Client) Post(ctx context.Context, path string, body any) (*Response, error) {
	if body == nil {
		body = map[string]any{}
	}
	return c.Do(ctx, http.MethodPost, path, body, nil)
}
