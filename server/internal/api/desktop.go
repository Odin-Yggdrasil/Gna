package api

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"gna/internal/auth"
	"gna/internal/rdp"
	"gna/internal/recording"
	"gna/internal/transfer"
	"gna/internal/vncproxy"
)

// handleVNC relaie une session de bureau à distance : WebSocket (noVNC) ⇄ Gná ⇄ cible.
//   - VNC : Gná se connecte en TCP et réalise lui-même l'authentification VNC ;
//   - RDP : Gná lance la passerelle gna-rdp, qui ouvre la session RDP et la présente
//     en VNC. La suite (relais, enregistrement, relecture) est commune.
//
// Dans les deux cas, le navigateur ne voit jamais le mot de passe de la cible.
func (s *Server) handleVNC(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	t, ok := s.loadAllowedTarget(w, r)
	if !ok {
		return
	}
	if t.Protocol != "vnc" && t.Protocol != "rdp" {
		writeError(w, http.StatusBadRequest, "cette cible n'est pas un bureau à distance")
		return
	}
	proto := t.Protocol // préfixe des événements d'audit : vnc.* ou rdp.*
	width := clamp(atoi(r.URL.Query().Get("w")), 640, 3840, 1280)
	height := clamp(atoi(r.URL.Query().Get("h")), 480, 2160, 800)

	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	ws, err := websocket.Accept(w, r, &websocket.AcceptOptions{Subprotocols: []string{"binary"}})
	if err != nil {
		return
	}
	ws.SetReadLimit(1 << 20)
	defer ws.CloseNow()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	browser := websocket.NetConn(ctx, ws, websocket.MessageBinary)
	ip := s.clientIP(r)

	cred, err := s.decryptCredentials(t)
	if err != nil {
		slog.Error("identifiants de la cible", "cible", t.Name, "err", err)
		vncproxy.RejectClient(browser, "identifiants de la cible illisibles")
		return
	}
	var target io.ReadWriteCloser
	var xfer *transfer.Session // dossier partagé (lecteur « Gna »), RDP uniquement
	if proto == "rdp" {
		xfer = s.openTransfer(r, t.Name)
		if xfer != nil {
			defer s.xfer.Close(xfer)
		}
		p := rdp.Params{
			Host: t.Host, Port: t.Port, Username: cred.Username, Password: cred.Password,
			Width: width, Height: height, Fingerprint: t.HostKey, Layout: t.Layout,
			Title: id.User.Username + " → " + t.Name, Clipboard: s.cfg.Clipboard,
		}
		if xfer != nil {
			p.DriveDir, p.DriveMaxMB = xfer.Dir, s.cfg.TransferMaxMB
		}
		target, err = s.dialRDP(ctx, p)
	} else {
		dialCtx, dialCancel := context.WithTimeout(ctx, 20*time.Second)
		target, err = vncproxy.DialAndAuth(dialCtx, t.Host, t.Port, cred.Password)
		dialCancel()
	}
	cred.Password = ""
	if err != nil {
		event := proto + ".connect_failed"
		if errors.Is(err, rdp.ErrCertMismatch) {
			event = "rdp.cert_mismatch"
		}
		_ = s.st.Audit(ctx, &id.User.ID, id.User.Username, event, ip,
			map[string]any{"target": t.Name, "error": err.Error()})
		vncproxy.RejectClient(browser, err.Error())
		return
	}
	defer target.Close()

	accessID, err := s.st.StartAccessSession(ctx, id.User.ID, id.User.Username, t, ip)
	if err != nil {
		slog.Error("enregistrement de la session d'accès", "err", err)
	}
	if xfer != nil {
		xfer.AccessID = accessID
	}

	// Enregistrement : si activé et impossible, la session est refusée.
	var rec *recording.Writer
	if s.cfg.RecordVNC {
		var finish func()
		rec, finish, err = s.startRecording(ctx, accessID, func(path string) (*recording.Writer, error) {
			return recording.CreateVNC(path, accessID, s.auth.Box(), recording.VNCHeader{Title: id.User.Username + "@" + t.Name})
		})
		if err != nil {
			slog.Error("création de l'enregistrement", "err", err)
			if accessID != 0 {
				_ = s.st.EndAccessSession(ctx, accessID, "enregistrement impossible", 0, 0)
			}
			_ = s.st.Audit(ctx, &id.User.ID, id.User.Username, "recording.failed", ip,
				map[string]any{"target": t.Name, "session": accessID, "error": err.Error()})
			vncproxy.RejectClient(browser, "enregistrement de la session impossible : connexion refusée")
			return
		}
		defer finish()
	}

	if err := vncproxy.AcceptClient(browser); err != nil {
		slog.Warn("poignée de main VNC avec le navigateur", "err", err)
		if accessID != 0 {
			_ = s.st.EndAccessSession(ctx, accessID, "navigateur déconnecté", 0, 0)
		}
		return
	}
	_ = s.st.Audit(ctx, &id.User.ID, id.User.Username, proto+".session_start", ip,
		map[string]any{"target": t.Name, "host": t.Host, "session": accessID, "recorded": rec != nil,
			"clipboard": s.cfg.Clipboard, "file_transfer": xfer != nil})
	started := time.Now()

	var bytesIn, bytesOut atomic.Int64
	var userActive atomic.Bool
	var once sync.Once
	reason := "fermée"
	stop := func(why string) { once.Do(func() { reason = why; cancel() }) }

	// Cible → navigateur
	go func() {
		buf := make([]byte, 64<<10)
		for {
			n, err := target.Read(buf)
			if n > 0 {
				bytesOut.Add(int64(n))
				rec.Frame(buf[:n])
				if _, werr := browser.Write(buf[:n]); werr != nil {
					stop("navigateur déconnecté")
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) || errors.Is(err, net.ErrClosed) {
					stop("fin de session sur la cible")
				} else {
					stop("connexion à la cible perdue")
				}
				return
			}
		}
	}()
	// Navigateur → cible (clavier, souris, demandes de rafraîchissement)
	go func() {
		if proto == "vnc" && !s.cfg.Clipboard {
			// Presse-papiers interdit : messages analysés et copier-coller retiré.
			// (En RDP, c'est la passerelle qui l'applique.)
			err := vncproxy.RelayWithoutClipboard(target, browser, func(n int) {
				userActive.Store(true)
				bytesIn.Add(int64(n))
			})
			if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
				slog.Warn("relais VNC", "err", err)
			}
			stop("navigateur déconnecté")
			return
		}
		buf := make([]byte, 32<<10)
		for {
			n, err := browser.Read(buf)
			if n > 0 {
				userActive.Store(true)
				bytesIn.Add(int64(n))
				if _, werr := target.Write(buf[:n]); werr != nil {
					stop("connexion à la cible perdue")
					return
				}
			}
			if err != nil {
				stop("navigateur déconnecté")
				return
			}
		}
	}()
	go s.supervise(ctx, id, t.ID, &userActive, stop)

	<-ctx.Done()
	target.Close()
	ws.Close(websocket.StatusNormalClosure, "")

	endCtx, endCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer endCancel()
	if accessID != 0 {
		_ = s.st.EndAccessSession(endCtx, accessID, reason, bytesIn.Load(), bytesOut.Load())
	}
	_ = s.st.Audit(endCtx, &id.User.ID, id.User.Username, proto+".session_end", ip, map[string]any{
		"target": t.Name, "session": accessID, "reason": reason,
		"duration_s": int(time.Since(started).Seconds()), "bytes_in": bytesIn.Load(), "bytes_out": bytesOut.Load(),
	})
}

// supervise revérifie régulièrement, pendant une connexion longue, que la session web
// est toujours valide et que l'utilisateur a toujours accès à la cible. Sinon, stop est
// appelé avec le motif (déconnexion, compte désactivé, inactivité, accès retiré).
// En VNC, le navigateur demande en continu des rafraîchissements d'écran : tant que le
// bureau est ouvert, il compte comme actif (le délai d'inactivité ne s'applique pas).
func (s *Server) supervise(ctx context.Context, id *auth.Identity, targetID int64, active *atomic.Bool, stop func(string)) {
	tick := time.NewTicker(revalidateEvery)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			u, err := s.auth.Revalidate(ctx, id.Token, active.Swap(false))
			if errors.Is(err, auth.ErrUnauthenticated) {
				stop("session web expirée ou révoquée")
				return
			}
			if err != nil || u.IsAdmin {
				continue
			}
			if ok, err := s.st.CanAccessTarget(ctx, u.ID, targetID); err == nil && !ok {
				stop("accès à la cible retiré")
				return
			}
		}
	}
}

// dialRDP lance la passerelle RDP et réalise la poignée de main VNC avec elle.
func (s *Server) dialRDP(ctx context.Context, p rdp.Params) (io.ReadWriteCloser, error) {
	bin, err := rdp.Bridge(s.cfg.RDPBridge)
	if err != nil {
		return nil, err
	}
	conn, err := rdp.Start(ctx, bin, p)
	if err != nil {
		return nil, err
	}
	if err := vncproxy.Handshake(conn, ""); err != nil {
		conn.Close()
		return nil, fmt.Errorf("passerelle RDP : %w", err)
	}
	return conn, nil
}
