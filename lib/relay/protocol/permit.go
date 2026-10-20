package protocol

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
	"time"

	native "github.com/syncthing/syncthing/lib/protocol"
)

// ControlledProtocolName isolates permission-bearing control traffic from old
// public relays. This does not change the BEP data protocol.
const ControlledProtocolName = "bep-relay-private-v1"

var ErrPermit = errors.New("relay permission refused")

// Permit binds an allocation permission to one relay boot, not just its host.
// Times are UTC Unix seconds with no expiry grace. Join is registration only;
// connect authorizes one directed pair and never grants folder access.
type Permit struct {
	Version   int    `json:"version"`
	KeyID     string `json:"key_id"`
	Issuer    string `json:"issuer"`
	Audience  string `json:"audience"`
	Boot      string `json:"boot"`
	Purpose   string `json:"purpose"`
	Subject   string `json:"subject"`
	Target    string `json:"target"`
	ID        string `json:"id"`
	IssuedAt  int64  `json:"iat"`
	ExpiresAt int64  `json:"exp"`
}

// PermitToken must never be logged, added to URLs, or persisted with devices.
type PermitToken string

func (PermitToken) String() string   { return "[relay permission redacted]" }
func (PermitToken) GoString() string { return "[relay permission redacted]" }

func permitID(v string) bool {
	if len(v) != 32 {
		return false
	}
	for _, c := range v {
		if !(c >= 'a' && c <= 'f' || c >= '0' && c <= '9') {
			return false
		}
	}
	return true
}
func permitDevice(v string) bool {
	id, err := native.DeviceIDFromString(v)
	return err == nil && id.String() == v
}
func (p Permit) valid(now time.Time) bool {
	if p.Version != 1 || len(p.KeyID) == 0 || len(p.KeyID) > 64 || len(p.Issuer) == 0 || len(p.Issuer) > 128 || !permitDevice(p.Audience) || !permitDevice(p.Subject) || !permitID(p.Boot) || !permitID(p.ID) {
		return false
	}
	maxTTL := int64(0)
	switch p.Purpose {
	case "join":
		if p.Target != "" {
			return false
		}
		maxTTL = 300
	case "connect":
		if !permitDevice(p.Target) || p.Target == p.Subject {
			return false
		}
		maxTTL = 30
	default:
		return false
	}
	// Compare ordered positive values before subtracting to avoid overflow.
	return p.IssuedAt > 0 && p.IssuedAt <= now.Unix() && p.ExpiresAt > now.Unix() && p.ExpiresAt > p.IssuedAt && p.ExpiresAt-p.IssuedAt <= maxTTL
}

func SignPermit(p Permit, key ed25519.PrivateKey, now time.Time) (PermitToken, error) {
	if len(key) != ed25519.PrivateKeySize || !p.valid(now) {
		return "", ErrPermit
	}
	raw, err := json.Marshal(p)
	if err != nil {
		return "", ErrPermit
	}
	signature := ed25519.Sign(key, append([]byte(ControlledProtocolName+"\x00"), raw...))
	return PermitToken(base64.RawURLEncoding.EncodeToString(raw) + "." + base64.RawURLEncoding.EncodeToString(signature)), nil
}

// VerifyPermit authenticates before any target lookup or resource allocation.
// Keys are operator-configured current/previous keys; callers enforce their
// rotation window. Expected identities come from TLS and local deployment.
func VerifyPermit(token PermitToken, keys map[string]ed25519.PublicKey, issuer, audience, boot, purpose, subject, target string, now time.Time) (Permit, error) {
	var p Permit
	if len(token) > 4096 {
		return p, ErrPermit
	}
	parts := strings.Split(string(token), ".")
	if len(parts) != 2 {
		return p, ErrPermit
	}
	raw, e := base64.RawURLEncoding.Strict().DecodeString(parts[0])
	if e != nil {
		return p, ErrPermit
	}
	sig, e := base64.RawURLEncoding.Strict().DecodeString(parts[1])
	if e != nil || len(sig) != ed25519.SignatureSize {
		return p, ErrPermit
	}
	if json.Unmarshal(raw, &p) != nil {
		return Permit{}, ErrPermit
	}
	// Only the canonical encoding is signed/accepted. This also rejects unknown,
	// duplicate and case-aliased fields, trailing input and null fields.
	canonical, e := json.Marshal(p)
	if e != nil || !bytes.Equal(canonical, raw) {
		return Permit{}, ErrPermit
	}
	key := keys[p.KeyID]
	if len(key) != ed25519.PublicKeySize || !ed25519.Verify(key, append([]byte(ControlledProtocolName+"\x00"), raw...), sig) || !p.valid(now) || p.Issuer != issuer || p.Audience != audience || p.Boot != boot || p.Purpose != purpose || p.Subject != subject || p.Target != target {
		return Permit{}, ErrPermit
	}
	return p, nil
}

// Controlled requests use separate message numbers and ALPN. The entire body
// is the bounded signed token; Connect obtains its target only from the token,
// preventing a second unsigned target from disagreeing with the permission.
const (
	messageTypePrivateJoin    int32 = 256
	messageTypePrivateConnect int32 = 257
)

type PrivateJoinRequest struct{ Permit PermitToken }
type PrivateConnectRequest struct{ Permit PermitToken }

func (PrivateJoinRequest) String() string      { return "private relay join [redacted]" }
func (PrivateJoinRequest) GoString() string    { return "private relay join [redacted]" }
func (PrivateConnectRequest) String() string   { return "private relay connect [redacted]" }
func (PrivateConnectRequest) GoString() string { return "private relay connect [redacted]" }

func permitPayload(token PermitToken) ([]byte, error) {
	if len(token) == 0 || len(token) > 4096 {
		return nil, ErrPermit
	}
	return []byte(token), nil
}
