package vncproxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"io"
	"net"
	"strings"
	"testing"
)

// Serveur RFB 3.8 minimal : VncAuth (mot de passe "secret") ou aucune sécurité.
func fakeServer(t *testing.T, password string, types []byte) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				io.WriteString(c, "RFB 003.008\n")
				var v [12]byte
				io.ReadFull(c, v[:])
				c.Write(append([]byte{byte(len(types))}, types...))
				var ch [1]byte
				io.ReadFull(c, ch[:])
				if ch[0] == secVNCAuth {
					challenge := make([]byte, 16)
					rand.Read(challenge)
					c.Write(challenge)
					resp := make([]byte, 16)
					io.ReadFull(c, resp)
					want, _ := vncAuthResponse(password, challenge)
					if !bytes.Equal(resp, want) {
						binary.Write(c, binary.BigEndian, uint32(1))
						msg := "mauvais mot de passe"
						binary.Write(c, binary.BigEndian, uint32(len(msg)))
						io.WriteString(c, msg)
						return
					}
				}
				binary.Write(c, binary.BigEndian, uint32(0))
				var shared [1]byte
				io.ReadFull(c, shared[:]) // ClientInit
				io.WriteString(c, "SERVERINIT")
			}()
		}
	}()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestDialAndAuth(t *testing.T) {
	port := fakeServer(t, "secret", []byte{19, secVNCAuth}) // 19 = VeNCrypt, ignoré
	ctx := context.Background()

	if _, err := DialAndAuth(ctx, "127.0.0.1", port, "faux"); err == nil || !strings.Contains(err.Error(), "refusée") {
		t.Fatalf("un mauvais mot de passe doit être refusé, obtenu %v", err)
	}
	if _, err := DialAndAuth(ctx, "127.0.0.1", port, ""); err == nil {
		t.Fatal("sans mot de passe, la connexion doit échouer")
	}
	c, err := DialAndAuth(ctx, "127.0.0.1", port, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte{1})
	buf := make([]byte, 10)
	if _, err := io.ReadFull(c, buf); err != nil || string(buf) != "SERVERINIT" {
		t.Fatalf("relais après authentification incorrect : %q %v", buf, err)
	}
}

func TestUnsupportedSecurity(t *testing.T) {
	port := fakeServer(t, "", []byte{19})
	_, err := DialAndAuth(context.Background(), "127.0.0.1", port, "secret")
	if err == nil || !strings.Contains(err.Error(), "VncAuth") {
		t.Fatalf("attendu un message sur SecurityTypes, obtenu %v", err)
	}
}

func TestAcceptAndRejectClient(t *testing.T) {
	a, b := net.Pipe()
	go func() { _ = AcceptClient(a) }()
	var v [12]byte
	io.ReadFull(b, v[:])
	b.Write(v[:])
	var types [2]byte
	io.ReadFull(b, types[:])
	if types != [2]byte{1, secNone} {
		t.Fatalf("types annoncés : %v", types)
	}
	b.Write([]byte{secNone})
	var res uint32
	binary.Read(b, binary.BigEndian, &res)
	if res != 0 {
		t.Fatal("SecurityResult doit être OK")
	}

	a2, b2 := net.Pipe()
	go RejectClient(a2, "cible injoignable : refusée")
	io.ReadFull(b2, v[:])
	b2.Write(v[:])
	var n [1]byte
	io.ReadFull(b2, n[:])
	if n[0] != 0 || readReason(b2) != "cible injoignable : refus\xe9e" {
		t.Fatal("le refus doit transmettre la raison au navigateur")
	}
}
