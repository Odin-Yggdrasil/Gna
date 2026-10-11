package sshproxy

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
)

// Serveur SSH minimal pour les tests : mot de passe « secret » ou clé autorisée,
// shell factice qui renvoie ce qu'on tape et signale les redimensionnements.
type testServer struct {
	addr    string
	hostKey ssh.Signer
	port    int
}

func startTestServer(t *testing.T, authorized ssh.PublicKey) *testServer {
	t.Helper()
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	signer, _ := ssh.NewSignerFromKey(priv)
	cfg := &ssh.ServerConfig{
		PasswordCallback: func(c ssh.ConnMetadata, pw []byte) (*ssh.Permissions, error) {
			if c.User() == "odin" && string(pw) == "secret" {
				return nil, nil
			}
			return nil, fmt.Errorf("refusé")
		},
		PublicKeyCallback: func(c ssh.ConnMetadata, k ssh.PublicKey) (*ssh.Permissions, error) {
			if authorized != nil && string(k.Marshal()) == string(authorized.Marshal()) {
				return nil, nil
			}
			return nil, fmt.Errorf("refusé")
		},
	}
	cfg.AddHostKey(signer)
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
			go serveConn(c, cfg)
		}
	}()
	return &testServer{addr: ln.Addr().String(), hostKey: signer, port: ln.Addr().(*net.TCPAddr).Port}
}

func serveConn(c net.Conn, cfg *ssh.ServerConfig) {
	_, chans, reqs, err := ssh.NewServerConn(c, cfg)
	if err != nil {
		return
	}
	go ssh.DiscardRequests(reqs)
	for nc := range chans {
		ch, creqs, err := nc.Accept()
		if err != nil {
			continue
		}
		go func() {
			for r := range creqs {
				switch r.Type {
				case "pty-req", "shell":
					r.Reply(true, nil)
					if r.Type == "shell" {
						go func() {
							io.WriteString(ch, "Bienvenue\r\n")
							buf := make([]byte, 1024)
							for {
								n, err := ch.Read(buf)
								if err != nil {
									return
								}
								if strings.Contains(string(buf[:n]), "exit") {
									ch.SendRequest("exit-status", false, []byte{0, 0, 0, 0})
									ch.Close()
									return
								}
								ch.Write(buf[:n])
							}
						}()
					}
				case "window-change":
					cols := binary.BigEndian.Uint32(r.Payload[0:4])
					rows := binary.BigEndian.Uint32(r.Payload[4:8])
					fmt.Fprintf(ch, "[resize %dx%d]", cols, rows)
				default:
					r.Reply(false, nil)
				}
			}
		}()
	}
}

func TestScanDialAndShell(t *testing.T) {
	srv := startTestServer(t, nil)
	ctx := context.Background()

	line, fp, err := ScanHostKey(ctx, "127.0.0.1", srv.port)
	if err != nil {
		t.Fatal(err)
	}
	if fp != ssh.FingerprintSHA256(srv.hostKey.PublicKey()) {
		t.Fatalf("empreinte inattendue : %s", fp)
	}

	if _, err := Dial(ctx, "127.0.0.1", srv.port, line, Credentials{Username: "odin", Password: "faux"}); err == nil {
		t.Error("un mauvais mot de passe doit être refusé")
	}
	if _, err := Dial(ctx, "127.0.0.1", srv.port, "", Credentials{Username: "odin", Password: "secret"}); err != ErrNoHostKey {
		t.Error("une cible sans clé épinglée doit être refusée")
	}

	client, err := Dial(ctx, "127.0.0.1", srv.port, line, Credentials{Username: "odin", Password: "secret"})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	sh, err := OpenShell(client, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer sh.Close()
	// L'écho de la saisie et la réponse au redimensionnement partent de deux goroutines
	// du serveur de test : on attend l'un puis l'autre pour ne pas dépendre de leur ordre.
	sh.Stdin.Write([]byte("bonjour"))
	got := readUntil(t, sh.Stdout, "bonjour")
	if !strings.Contains(got, "Bienvenue") {
		t.Errorf("sortie inattendue : %q", got)
	}
	sh.Resize(120, 40)
	readUntil(t, sh.Stdout, "[resize 120x40]")
}

func TestHostKeyMismatch(t *testing.T) {
	srv := startTestServer(t, nil)
	other := startTestServer(t, nil) // autre serveur = autre clé
	line, _, err := ScanHostKey(context.Background(), "127.0.0.1", other.port)
	if err != nil {
		t.Fatal(err)
	}
	_, err = Dial(context.Background(), "127.0.0.1", srv.port, line, Credentials{Username: "odin", Password: "secret"})
	if err != ErrHostKeyMismatch {
		t.Fatalf("attendu ErrHostKeyMismatch, obtenu %v", err)
	}
}

func TestPrivateKeyAuth(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	sshPub, _ := ssh.NewPublicKey(pub)
	srv := startTestServer(t, sshPub)
	line, _, _ := ScanHostKey(context.Background(), "127.0.0.1", srv.port)

	block, err := ssh.MarshalPrivateKeyWithPassphrase(priv, "", []byte("phrase"))
	if err != nil {
		t.Fatal(err)
	}
	keyPEM := pem.EncodeToMemory(block)
	if err := CheckPrivateKey(keyPEM, nil); err == nil || !strings.Contains(err.Error(), "phrase de passe") {
		t.Errorf("une clé protégée sans phrase doit être signalée, obtenu %v", err)
	}
	c, err := Dial(context.Background(), "127.0.0.1", srv.port, line,
		Credentials{Username: "odin", PrivateKey: keyPEM, Passphrase: []byte("phrase")})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
}

func readUntil(t *testing.T, r io.Reader, want string) string {
	t.Helper()
	var sb strings.Builder
	buf := make([]byte, 1024)
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if strings.Contains(sb.String(), want) {
			return sb.String()
		}
		if err != nil {
			break
		}
	}
	t.Fatalf("%q non reçu, sortie : %q", want, sb.String())
	return ""
}
