// Package sshproxy ouvre les connexions SSH vers les machines cibles.
//
// La clé d'hôte de chaque cible est épinglée : Gná refuse de se connecter si le
// serveur présente une autre clé (protection contre l'interception).
package sshproxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"golang.org/x/crypto/ssh"
)

const dialTimeout = 10 * time.Second

var (
	ErrNoHostKey       = errors.New("aucune clé d'hôte enregistrée pour cette cible")
	ErrHostKeyMismatch = errors.New("la clé d'hôte du serveur ne correspond pas à celle enregistrée : connexion refusée (possible interception)")
)

// Credentials sont les identifiants déchiffrés, gardés en mémoire le temps de la connexion.
type Credentials struct {
	Username   string
	Password   string // si authentification par mot de passe
	PrivateKey []byte // si authentification par clé
	Passphrase []byte
}

// ScanHostKey récupère la clé d'hôte d'un serveur sans s'authentifier.
// Renvoie la clé au format authorized_keys et son empreinte SHA256.
func ScanHostKey(ctx context.Context, host string, port int) (line, fingerprint string, err error) {
	var found ssh.PublicKey
	cfg := &ssh.ClientConfig{
		User: "gna-scan",
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			found = key
			return errors.New("scan terminé") // interrompt la poignée de main volontairement
		},
		Timeout: dialTimeout,
	}
	conn, err := dialTCP(ctx, host, port)
	if err != nil {
		return "", "", err
	}
	defer conn.Close()
	_, _, _, _ = ssh.NewClientConn(conn, addr(host, port), cfg)
	if found == nil {
		return "", "", fmt.Errorf("impossible de lire la clé d'hôte de %s (est-ce bien un serveur SSH ?)", addr(host, port))
	}
	return strings.TrimSpace(string(ssh.MarshalAuthorizedKey(found))), ssh.FingerprintSHA256(found), nil
}

// ParseHostKey valide une clé au format authorized_keys ("ssh-ed25519 AAAA…").
func ParseHostKey(line string) (ssh.PublicKey, error) {
	key, _, _, _, err := ssh.ParseAuthorizedKey([]byte(strings.TrimSpace(line)))
	if err != nil {
		return nil, errors.New("clé d'hôte invalide (format attendu : « ssh-ed25519 AAAA… »)")
	}
	return key, nil
}

// Fingerprint renvoie l'empreinte SHA256 d'une clé au format authorized_keys ("" si invalide).
func Fingerprint(line string) string {
	key, err := ParseHostKey(line)
	if err != nil {
		return ""
	}
	return ssh.FingerprintSHA256(key)
}

// CheckPrivateKey vérifie qu'une clé privée est lisible (avec sa phrase de passe éventuelle).
func CheckPrivateKey(pem, passphrase []byte) error {
	_, err := parseSigner(pem, passphrase)
	return err
}

// Dial ouvre une connexion SSH authentifiée vers la cible, avec la clé d'hôte épinglée.
func Dial(ctx context.Context, host string, port int, hostKey string, cred Credentials) (*ssh.Client, error) {
	if strings.TrimSpace(hostKey) == "" {
		return nil, ErrNoHostKey
	}
	pinned, err := ParseHostKey(hostKey)
	if err != nil {
		return nil, err
	}

	var auth []ssh.AuthMethod
	switch {
	case len(cred.PrivateKey) > 0:
		signer, err := parseSigner(cred.PrivateKey, cred.Passphrase)
		if err != nil {
			return nil, err
		}
		auth = append(auth, ssh.PublicKeys(signer))
	case cred.Password != "":
		pw := cred.Password
		auth = append(auth, ssh.Password(pw), ssh.KeyboardInteractive(
			func(_, _ string, questions []string, _ []bool) ([]string, error) {
				answers := make([]string, len(questions))
				for i := range answers {
					answers[i] = pw
				}
				return answers, nil
			}))
	default:
		return nil, errors.New("aucun identifiant enregistré pour cette cible")
	}

	var mismatch atomic.Bool // le rappel peut s'exécuter dans une autre goroutine
	cfg := &ssh.ClientConfig{
		User: cred.Username,
		Auth: auth,
		HostKeyCallback: func(_ string, _ net.Addr, key ssh.PublicKey) error {
			if string(key.Marshal()) != string(pinned.Marshal()) {
				mismatch.Store(true)
				return ErrHostKeyMismatch
			}
			return nil
		},
		HostKeyAlgorithms: hostKeyAlgorithms(pinned),
		Timeout:           dialTimeout,
	}
	conn, err := dialTCP(ctx, host, port)
	if err != nil {
		return nil, err
	}
	c, chans, reqs, err := ssh.NewClientConn(conn, addr(host, port), cfg)
	if err != nil {
		conn.Close()
		if mismatch.Load() {
			return nil, ErrHostKeyMismatch
		}
		if strings.Contains(err.Error(), "unable to authenticate") {
			return nil, errors.New("authentification refusée par la cible (identifiant, mot de passe ou clé incorrects)")
		}
		return nil, fmt.Errorf("connexion SSH : %w", err)
	}
	return ssh.NewClient(c, chans, reqs), nil
}

// Shell ouvre un terminal interactif sur la connexion.
type Shell struct {
	session *ssh.Session
	Stdin   io.WriteCloser
	Stdout  io.Reader
}

func OpenShell(client *ssh.Client, cols, rows int) (*Shell, error) {
	sess, err := client.NewSession()
	if err != nil {
		return nil, err
	}
	modes := ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 115200, ssh.TTY_OP_OSPEED: 115200}
	if err := sess.RequestPty("xterm-256color", rows, cols, modes); err != nil {
		sess.Close()
		return nil, fmt.Errorf("demande de terminal : %w", err)
	}
	stdin, err := sess.StdinPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	stdout, err := sess.StdoutPipe()
	if err != nil {
		sess.Close()
		return nil, err
	}
	sess.Stderr = nil // en mode PTY, stderr arrive mélangé à stdout
	if err := sess.Shell(); err != nil {
		sess.Close()
		return nil, fmt.Errorf("ouverture du shell : %w", err)
	}
	return &Shell{session: sess, Stdin: stdin, Stdout: stdout}, nil
}

func (s *Shell) Resize(cols, rows int) error { return s.session.WindowChange(rows, cols) }
func (s *Shell) Wait() error                 { return s.session.Wait() }
func (s *Shell) Close() error                { return s.session.Close() }

// ---------- utilitaires ----------

func parseSigner(pem, passphrase []byte) (ssh.Signer, error) {
	var signer ssh.Signer
	var err error
	if len(passphrase) > 0 {
		signer, err = ssh.ParsePrivateKeyWithPassphrase(pem, passphrase)
	} else {
		signer, err = ssh.ParsePrivateKey(pem)
	}
	var missing *ssh.PassphraseMissingError
	switch {
	case errors.As(err, &missing):
		return nil, errors.New("la clé privée est protégée : renseigner sa phrase de passe")
	case err != nil:
		return nil, errors.New("clé privée illisible ou phrase de passe incorrecte")
	}
	return signer, nil
}

// hostKeyAlgorithms demande au serveur le même type de clé que celle épinglée,
// sinon il pourrait présenter une autre de ses clés (ex. ECDSA au lieu d'Ed25519).
func hostKeyAlgorithms(k ssh.PublicKey) []string {
	if k.Type() == ssh.KeyAlgoRSA {
		return []string{ssh.KeyAlgoRSASHA512, ssh.KeyAlgoRSASHA256}
	}
	return []string{k.Type()}
}

func addr(host string, port int) string { return net.JoinHostPort(host, strconv.Itoa(port)) }

func dialTCP(ctx context.Context, host string, port int) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr(host, port))
	if err != nil {
		return nil, fmt.Errorf("cible injoignable (%s) : %w", addr(host, port), err)
	}
	return conn, nil
}
