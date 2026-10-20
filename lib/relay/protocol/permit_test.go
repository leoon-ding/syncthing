package protocol

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	native "github.com/syncthing/syncthing/lib/protocol"
	"strings"
	"testing"
	"time"
)

func TestPermitIdentityExpiryAndCanonicalEncoding(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Unix(1800000000, 0)
	p := Permit{1, "current", "discovery", native.NewDeviceID([]byte("relay")).String(), strings.Repeat("a", 32), "connect", native.NewDeviceID([]byte("source")).String(), native.NewDeviceID([]byte("target")).String(), strings.Repeat("b", 32), now.Unix(), now.Unix() + 30}
	token, err := SignPermit(p, key, now)
	if err != nil {
		t.Fatal(err)
	}
	verify := func(tok PermitToken, expected Permit, at time.Time) error {
		_, e := VerifyPermit(tok, map[string]ed25519.PublicKey{"current": pub}, expected.Issuer, expected.Audience, expected.Boot, expected.Purpose, expected.Subject, expected.Target, at)
		return e
	}
	if e := verify(token, p, now); e != nil {
		t.Fatal(e)
	}
	for _, field := range []string{"issuer", "audience", "boot", "purpose", "subject", "target"} {
		q := p
		switch field {
		case "issuer":
			q.Issuer = "other"
		case "audience":
			q.Audience = q.Subject
		case "boot":
			q.Boot = strings.Repeat("c", 32)
		case "purpose":
			q.Purpose = "join"
		case "subject":
			q.Subject = q.Target
		case "target":
			q.Target = q.Audience
		}
		if verify(token, q, now) == nil {
			t.Fatal(field)
		}
	}
	if verify(token, p, now.Add(30*time.Second)) == nil || verify(token, p, now.Add(-time.Second)) == nil {
		t.Fatal("time boundary accepted")
	}
	for _, delta := range []int64{31, 301} {
		q := p
		q.ExpiresAt = q.IssuedAt + delta
		if _, e := SignPermit(q, key, now); e == nil {
			t.Fatal("excess lifetime")
		}
	}
	raw, _ := base64.RawURLEncoding.DecodeString(strings.Split(string(token), ".")[0])
	for _, bad := range []string{strings.Replace(string(raw), `"version":1`, `"version":1,"version":1`, 1), strings.Replace(string(raw), `"version"`, `"Version"`, 1), string(raw) + " ", strings.Replace(string(raw), `"version":1`, `"extra":0,"version":1`, 1)} {
		sig := ed25519.Sign(key, append([]byte(ControlledProtocolName+"\x00"), []byte(bad)...))
		tok := PermitToken(base64.RawURLEncoding.EncodeToString([]byte(bad)) + "." + base64.RawURLEncoding.EncodeToString(sig))
		if verify(tok, p, now) == nil {
			t.Fatal("accepted ambiguous signed payload")
		}
	}
	if strings.Contains(fmt.Sprintf("%v %#v", token, token), string(token)) {
		t.Fatal("token logged")
	}
	if _, e := VerifyPermit(token, map[string]ed25519.PublicKey{}, p.Issuer, p.Audience, p.Boot, p.Purpose, p.Subject, p.Target, now); e == nil {
		t.Fatal("unknown key")
	}
}

func TestPrivatePermitFraming(t *testing.T) {
	for _, tc := range []struct {
		msg    any
		golden string
	}{
		{PrivateJoinRequest{Permit: "abc"}, "9e79bc400000010000000003616263"},
		{PrivateConnectRequest{Permit: "abc"}, "9e79bc400000010100000003616263"},
	} {
		var b bytes.Buffer
		if err := WriteMessage(&b, tc.msg); err != nil {
			t.Fatal(err)
		}
		raw := b.Bytes()
		if hex.EncodeToString(raw) != tc.golden {
			t.Fatal(hex.EncodeToString(raw))
		}
		msg, err := ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		var round bytes.Buffer
		if err := WriteMessage(&round, msg); err != nil || !bytes.Equal(raw, round.Bytes()) {
			t.Fatal(err)
		}
		for n := 0; n < len(raw); n++ {
			if _, err := ReadMessage(bytes.NewReader(raw[:n])); err == nil {
				t.Fatal("truncated request accepted", n)
			}
		}
	}
	var b bytes.Buffer
	for _, token := range []PermitToken{"", PermitToken(strings.Repeat("x", 4097))} {
		if WriteMessage(&b, PrivateJoinRequest{Permit: token}) == nil {
			t.Fatal("unbounded token")
		}
	}
	// Legacy framing remains byte-for-byte unchanged.
	b.Reset()
	if err := WriteMessage(&b, ConnectRequest{ID: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if hex.EncodeToString(b.Bytes()) != "9e79bc4000000005000000080000000101000000" {
		t.Fatal(hex.EncodeToString(b.Bytes()))
	}
}
