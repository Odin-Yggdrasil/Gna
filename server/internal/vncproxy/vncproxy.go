// Package vncproxy relaie le protocole VNC (RFB) entre le navigateur et une cible.
//
// Gná s'authentifie lui-même auprès de la cible avec le mot de passe VNC chiffré en
// base, puis présente au navigateur une connexion déjà authentifiée : le mot de passe
// ne quitte jamais le serveur.
//
//	navigateur (noVNC) ──RFB sans auth──► Gná ──RFB + mot de passe VNC──► cible
package vncproxy

import (
	"context"
	"crypto/des"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"time"
)

const (
	secInvalid = 0
	secNone    = 1
	secVNCAuth = 2

	MaxPasswordLen = 8 // limite du protocole VNC
	dialTimeout    = 10 * time.Second
)

// DialAndAuth ouvre la connexion vers la cible et réalise l'authentification VNC.
// La connexion renvoyée est prête pour l'étape ClientInit, qui viendra du navigateur.
func DialAndAuth(ctx context.Context, host string, port int, password string) (net.Conn, error) {
	d := net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("cible injoignable (%s) : %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(15 * time.Second))
	if err := handshakeTarget(conn, password); err != nil {
		conn.Close()
		return nil, err
	}
	_ = conn.SetDeadline(time.Time{})
	return conn, nil
}

// Handshake réalise la poignée de main RFB côté client sur un flux quelconque
// (connexion TCP vers un serveur VNC, ou passerelle RDP).
func Handshake(c io.ReadWriter, password string) error { return handshakeTarget(c, password) }

func handshakeTarget(c io.ReadWriter, password string) error {
	var ver [12]byte
	if _, err := io.ReadFull(c, ver[:]); err != nil {
		return errors.New("pas de réponse VNC de la cible (est-ce bien un serveur VNC ?)")
	}
	var major, minor int
	if _, err := fmt.Sscanf(string(ver[:]), "RFB %03d.%03d\n", &major, &minor); err != nil || major != 3 {
		return errors.New("la cible ne parle pas le protocole VNC (RFB 3.x)")
	}
	switch {
	case minor >= 8:
		minor = 8
	case minor >= 7:
		minor = 7
	default:
		minor = 3
	}
	if _, err := fmt.Fprintf(c, "RFB 003.%03d\n", minor); err != nil {
		return err
	}

	var chosen byte
	if minor == 3 {
		// RFB 3.3 : le serveur impose le type de sécurité.
		var t uint32
		if err := binary.Read(c, binary.BigEndian, &t); err != nil {
			return err
		}
		if t == secInvalid {
			return fmt.Errorf("connexion refusée par la cible : %s", readReason(c))
		}
		chosen = byte(t)
	} else {
		var n [1]byte
		if _, err := io.ReadFull(c, n[:]); err != nil {
			return err
		}
		if n[0] == 0 {
			return fmt.Errorf("connexion refusée par la cible : %s", readReason(c))
		}
		types := make([]byte, n[0])
		if _, err := io.ReadFull(c, types); err != nil {
			return err
		}
		chosen = pickSecurity(types, password != "")
		if chosen == secInvalid {
			return fmt.Errorf("méthodes de sécurité de la cible non prises en charge (%v) : "+
				"activer l'authentification VNC classique (SecurityTypes=VncAuth)", types)
		}
		if _, err := c.Write([]byte{chosen}); err != nil {
			return err
		}
	}

	switch chosen {
	case secNone:
	case secVNCAuth:
		if password == "" {
			return errors.New("la cible demande un mot de passe VNC, aucun n'est enregistré")
		}
		var challenge [16]byte
		if _, err := io.ReadFull(c, challenge[:]); err != nil {
			return err
		}
		resp, err := vncAuthResponse(password, challenge[:])
		if err != nil {
			return err
		}
		if _, err := c.Write(resp); err != nil {
			return err
		}
	default:
		return fmt.Errorf("méthode de sécurité %d non prise en charge", chosen)
	}

	// Résultat : toujours en 3.8, seulement après une authentification en 3.3/3.7.
	if minor == 8 || chosen == secVNCAuth {
		var result uint32
		if err := binary.Read(c, binary.BigEndian, &result); err != nil {
			return err
		}
		if result != 0 {
			if minor == 8 {
				if r := readReason(c); r != "" {
					return fmt.Errorf("authentification VNC refusée : %s", r)
				}
			}
			return errors.New("authentification VNC refusée : mot de passe incorrect")
		}
	}
	return nil
}

func pickSecurity(types []byte, havePassword bool) byte {
	hasNone, hasVNC := false, false
	for _, t := range types {
		hasNone = hasNone || t == secNone
		hasVNC = hasVNC || t == secVNCAuth
	}
	switch {
	case hasVNC && havePassword:
		return secVNCAuth
	case hasNone:
		return secNone
	case hasVNC:
		return secVNCAuth // échouera avec un message clair : pas de mot de passe
	}
	return secInvalid
}

// vncAuthResponse chiffre le défi en DES avec le mot de passe (bits de chaque octet
// inversés, particularité historique du protocole VNC).
func vncAuthResponse(password string, challenge []byte) ([]byte, error) {
	var key [8]byte
	for i := 0; i < len(password) && i < 8; i++ {
		b := password[i]
		var r byte
		for j := 0; j < 8; j++ {
			r = r<<1 | (b>>j)&1
		}
		key[i] = r
	}
	block, err := des.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	out := make([]byte, 16)
	block.Encrypt(out[:8], challenge[:8])
	block.Encrypt(out[8:], challenge[8:])
	return out, nil
}

func readReason(r io.Reader) string {
	var n uint32
	if err := binary.Read(r, binary.BigEndian, &n); err != nil || n > 4096 {
		return "raison inconnue"
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return "raison inconnue"
	}
	return string(buf)
}

// ---------- côté navigateur ----------

// AcceptClient joue le rôle du serveur VNC auprès du navigateur : version 3.8,
// sécurité « aucune » (l'utilisateur est déjà authentifié par Gná).
func AcceptClient(c io.ReadWriter) error {
	if _, err := io.WriteString(c, "RFB 003.008\n"); err != nil {
		return err
	}
	var ver [12]byte
	if _, err := io.ReadFull(c, ver[:]); err != nil {
		return err
	}
	if string(ver[:]) != "RFB 003.008\n" {
		return fmt.Errorf("version RFB du navigateur non prise en charge : %q", ver[:])
	}
	if _, err := c.Write([]byte{1, secNone}); err != nil {
		return err
	}
	var choice [1]byte
	if _, err := io.ReadFull(c, choice[:]); err != nil {
		return err
	}
	if choice[0] != secNone {
		return errors.New("le navigateur a choisi une méthode de sécurité inattendue")
	}
	return binary.Write(c, binary.BigEndian, uint32(0)) // SecurityResult : OK
}

// RejectClient signale au navigateur un échec, avec un message que noVNC affiche.
func RejectClient(c io.ReadWriter, reason string) {
	if _, err := io.WriteString(c, "RFB 003.008\n"); err != nil {
		return
	}
	var ver [12]byte
	if _, err := io.ReadFull(c, ver[:]); err != nil {
		return
	}
	// noVNC lit la raison en Latin-1 : on convertit pour garder les accents français.
	msg := make([]byte, 0, len(reason))
	for _, r := range reason {
		if r < 256 {
			msg = append(msg, byte(r))
		} else {
			msg = append(msg, '?')
		}
	}
	buf := make([]byte, 0, 5+len(msg))
	buf = append(buf, 0) // aucun type de sécurité = refus
	buf = binary.BigEndian.AppendUint32(buf, uint32(len(msg)))
	buf = append(buf, msg...)
	_, _ = c.Write(buf)
}
