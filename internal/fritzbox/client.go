// Package fritzbox talks to a FRITZ!Box over interfaces AVM documents and keeps
// stable across firmware releases: login_sid.lua for the session, the
// AHA-HTTP-Interface for smart home data, and UPnP IGD for the internet
// connection's traffic.
//
// It deliberately does not touch the web UI. Scraping the UI is what makes
// home-grown FRITZ!Box scripts break on every firmware update; these are
// specified and versioned.
//
//	https://fritz.support/resources/HTTP_Session-ID_EN.pdf
//	https://avm.de/fileadmin/user_upload/Global/Service/Schnittstellen/AHA-HTTP-Interface.pdf
//	https://fritz.support/resources/TR-064_WAN_Common_Interface_Config.pdf
package fritzbox

import (
	"context"
	"crypto/md5"
	"crypto/pbkdf2"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf16"
)

// invalidSID is what the box returns when authentication failed.
const invalidSID = "0000000000000000"

// sessionTTL is conservative: AVM expires a session after 20 minutes idle, so
// re-authenticating well before that avoids a failed poll.
const sessionTTL = 10 * time.Minute

type Client struct {
	BaseURL  string
	Username string
	Password string
	HTTP     *http.Client

	// IGD is the box's UPnP address, derived from BaseURL.
	IGD string

	mu      sync.Mutex
	sid     string
	sidTime time.Time
}

func New(baseURL, username, password string) *Client {
	return &Client{
		BaseURL:  strings.TrimSuffix(baseURL, "/"),
		Username: username,
		Password: password,
		HTTP:     &http.Client{Timeout: 15 * time.Second},
		IGD:      igdBase(baseURL),
	}
}

type sessionInfo struct {
	SID       string `xml:"SID"`
	Challenge string `xml:"Challenge"`
	BlockTime int    `xml:"BlockTime"`
}

// sessionID returns a cached session, logging in when needed.
func (c *Client) sessionID(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.sid != "" && c.sid != invalidSID && time.Since(c.sidTime) < sessionTTL {
		return c.sid, nil
	}
	sid, err := c.login(ctx)
	if err != nil {
		return "", err
	}
	c.sid, c.sidTime = sid, time.Now()
	return sid, nil
}

func (c *Client) login(ctx context.Context) (string, error) {
	info, err := c.challenge(ctx)
	if err != nil {
		return "", err
	}
	if info.BlockTime > 0 {
		// The box rate-limits failed logins. Saying so beats a bare 403.
		return "", fmt.Errorf("login blocked for another %ds after a failed attempt", info.BlockTime)
	}

	response, err := answer(info.Challenge, c.Password)
	if err != nil {
		return "", err
	}

	form := url.Values{"username": {c.Username}, "response": {response}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		c.BaseURL+"/login_sid.lua?version=2", strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")

	var out sessionInfo
	if err := c.doXML(req, &out); err != nil {
		return "", err
	}
	if out.SID == "" || out.SID == invalidSID {
		return "", errors.New("login rejected: check the username and password, and that the user has Smart Home permission")
	}
	return out.SID, nil
}

func (c *Client) challenge(ctx context.Context) (sessionInfo, error) {
	var out sessionInfo
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/login_sid.lua?version=2", nil)
	if err != nil {
		return out, err
	}
	err = c.doXML(req, &out)
	return out, err
}

// answer computes the challenge response. Firmware from FRITZ!OS 7.24 sends a
// PBKDF2 challenge beginning "2$"; anything else is the legacy MD5 scheme.
func answer(challenge, password string) (string, error) {
	if strings.HasPrefix(challenge, "2$") {
		return pbkdf2Answer(challenge, password)
	}
	return md5Answer(challenge, password), nil
}

// pbkdf2Answer implements 2$<iter1>$<salt1>$<iter2>$<salt2>: hash the password
// with the first salt, then hash *that* with the second.
func pbkdf2Answer(challenge, password string) (string, error) {
	parts := strings.Split(challenge, "$")
	if len(parts) != 5 {
		return "", fmt.Errorf("malformed PBKDF2 challenge %q", challenge)
	}
	iter1, err := strconv.Atoi(parts[1])
	if err != nil {
		return "", fmt.Errorf("bad first iteration count: %w", err)
	}
	salt1, err := hex.DecodeString(parts[2])
	if err != nil {
		return "", fmt.Errorf("bad first salt: %w", err)
	}
	iter2, err := strconv.Atoi(parts[3])
	if err != nil {
		return "", fmt.Errorf("bad second iteration count: %w", err)
	}
	salt2, err := hex.DecodeString(parts[4])
	if err != nil {
		return "", fmt.Errorf("bad second salt: %w", err)
	}

	hash1, err := pbkdf2.Key(sha256.New, password, salt1, iter1, 32)
	if err != nil {
		return "", err
	}
	hash2, err := pbkdf2.Key(sha256.New, string(hash1), salt2, iter2, 32)
	if err != nil {
		return "", err
	}
	return parts[4] + "$" + hex.EncodeToString(hash2), nil
}

// md5Answer is the pre-7.24 scheme. The digest is taken over UTF-16LE, which
// is the detail everyone gets wrong.
func md5Answer(challenge, password string) string {
	text := challenge + "-" + password
	var buf []byte
	for _, r := range utf16.Encode([]rune(text)) {
		buf = append(buf, byte(r), byte(r>>8))
	}
	sum := md5.Sum(buf)
	return challenge + "-" + hex.EncodeToString(sum[:])
}

func (c *Client) doXML(req *http.Request, out any) error {
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("contacting %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("%s returned HTTP %d", req.URL.Path, resp.StatusCode)
	}
	if err := xml.Unmarshal(body, out); err != nil {
		return fmt.Errorf("parsing response from %s: %w", req.URL.Path, err)
	}
	return nil
}

// aha issues one AHA command and returns the raw body.
func (c *Client) aha(ctx context.Context, cmd string) ([]byte, error) {
	sid, err := c.sessionID(ctx)
	if err != nil {
		return nil, err
	}
	q := url.Values{"sid": {sid}, "switchcmd": {cmd}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		c.BaseURL+"/webservices/homeautoswitch.lua?"+q.Encode(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("contacting %s: %w", c.BaseURL, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusForbidden {
		// The session went stale or the user lacks the Smart Home permission.
		// Drop the cached SID so the next call logs in again.
		c.mu.Lock()
		c.sid = ""
		c.mu.Unlock()
		return nil, errors.New("AHA request forbidden: session expired, or the user lacks the Smart Home permission")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("AHA request returned HTTP %d", resp.StatusCode)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
}
