package api

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"gna/internal/recording"
	"gna/internal/sshproxy"
)

// Protocole du terminal sur la WebSocket :
//   - messages binaires : flux du terminal, dans les deux sens ;
//   - messages texte (JSON) : contrôle.
//     navigateur → Gná : {"type":"resize","cols":120,"rows":40}
//     Gná → navigateur : {"type":"connected","fingerprint":"SHA256:…"} | {"type":"error","message":"…"}
const (
	revalidateEvery  = 30 * time.Second
	maxCols, maxRows = 1000, 500
)

type controlMsg struct {
	Type    string `json:"type"`
	Cols    int    `json:"cols,omitempty"`
	Rows    int    `json:"rows,omitempty"`
	Message string `json:"message,omitempty"`
	FP      string `json:"fingerprint,omitempty"`
	Rec     bool   `json:"recording,omitempty"`
}

func (s *Server) handleSSHTerminal(w http.ResponseWriter, r *http.Request) {
	id := identity(r)
	t, ok := s.loadAllowedTarget(w, r)
	if !ok {
		return
	}
	if t.Protocol != "ssh" {
		writeError(w, http.StatusBadRequest, "cette cible n'est pas une cible SSH")
		return
	}
	cols := clamp(atoi(r.URL.Query().Get("cols")), 20, maxCols, 80)
	rows := clamp(atoi(r.URL.Query().Get("rows")), 5, maxRows, 24)

	// Les délais de lecture/écriture du serveur HTTP ne doivent pas couper une session longue.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Time{})
	_ = rc.SetWriteDeadline(time.Time{})

	// Accept vérifie que l'en-tête Origin correspond à l'hôte (pas de connexion depuis un autre site).
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(64 << 10)
	defer ws.CloseNow()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ip := s.clientIP(r)

	fail := func(msg string) {
		sendControl(ws, controlMsg{Type: "error", Message: msg})
		ws.Close(websocket.StatusNormalClosure, "")
	}

	cred, err := s.decryptCredentials(t)
	if err != nil {
		slog.Error("identifiants de la cible", "cible", t.Name, "err", err)
		fail("identifiants de la cible illisibles")
		return
	}
	dialCtx, dialCancel := context.WithTimeout(ctx, 15*time.Second)
	client, err := sshproxy.Dial(dialCtx, t.Host, t.Port, t.HostKey, cred)
	dialCancel()
	cred = sshproxy.Credentials{} // ne pas garder les secrets en mémoire plus que nécessaire
	if err != nil {
		event := "ssh.connect_failed"
		if errors.Is(err, sshproxy.ErrHostKeyMismatch) {
			event = "ssh.hostkey_mismatch"
		}
		_ = s.st.Audit(ctx, &id.User.ID, id.User.Username, event, ip, map[string]any{"target": t.Name, "error": err.Error()})
		fail(err.Error())
		return
	}
	defer client.Close()

	shell, err := sshproxy.OpenShell(client, cols, rows)
	if err != nil {
		fail(err.Error())
		return
	}
	defer shell.Close()

	accessID, err := s.st.StartAccessSession(ctx, id.User.ID, id.User.Username, t, ip)
	if err != nil {
		slog.Error("enregistrement de la session d'accès", "err", err)
	}
	// Transfert de fichiers (SFTP) par la même connexion, tant que le terminal est ouvert.
	sf, detachSFTP := s.openSFTP(r, client, t.Name)
	defer detachSFTP()
	if sf != nil {
		sf.accessID = accessID
	}

	// Enregistrement : si activé et impossible (disque plein, droits…), la session est
	// refusée plutôt que d'ouvrir un accès non tracé.
	var rec *recording.Writer
	if s.cfg.RecordSessions {
		var finish func()
		rec, finish, err = s.startRecording(ctx, accessID, func(path string) (*recording.Writer, error) {
			return recording.Create(path, accessID, s.auth.Box(), recording.Header{
				Width: cols, Height: rows, Title: id.User.Username + "@" + t.Name,
				Env: map[string]string{"TERM": "xterm-256color"},
			}, s.cfg.RecordInput)
		})
		if err != nil {
			slog.Error("création de l'enregistrement", "err", err)
			if accessID != 0 {
				_ = s.st.EndAccessSession(ctx, accessID, "enregistrement impossible", 0, 0)
			}
			_ = s.st.Audit(ctx, &id.User.ID, id.User.Username, "recording.failed", ip,
				map[string]any{"target": t.Name, "session": accessID, "error": err.Error()})
			fail("enregistrement de la session impossible : connexion refusée")
			return
		}
		defer finish()
	}

	_ = s.st.Audit(ctx, &id.User.ID, id.User.Username, "ssh.session_start", ip,
		map[string]any{"target": t.Name, "host": t.Host, "session": accessID, "recorded": rec != nil})
	started := time.Now()
	sendControl(ws, controlMsg{Type: "connected", FP: sshproxy.Fingerprint(t.HostKey), Rec: rec != nil})

	var bytesIn, bytesOut atomic.Int64 // in = vers la cible, out = vers le navigateur
	var userActive atomic.Bool
	var once sync.Once
	reason := "fermée"
	stop := func(why string) {
		once.Do(func() { reason = why; cancel() })
	}

	// Cible → navigateur
	go func() {
		buf := make([]byte, 32<<10)
		for {
			n, err := shell.Stdout.Read(buf)
			if n > 0 {
				bytesOut.Add(int64(n))
				rec.Output(buf[:n])
				if werr := ws.Write(ctx, websocket.MessageBinary, buf[:n]); werr != nil {
					stop("navigateur déconnecté")
					return
				}
			}
			if err != nil {
				if errors.Is(err, io.EOF) {
					stop("fin de session sur la cible")
				} else {
					stop("connexion à la cible perdue")
				}
				return
			}
		}
	}()

	// Navigateur → cible
	go func() {
		for {
			typ, data, err := ws.Read(ctx)
			if err != nil {
				stop("navigateur déconnecté")
				return
			}
			if typ == websocket.MessageBinary {
				userActive.Store(true)
				bytesIn.Add(int64(len(data)))
				rec.Input(data)
				if _, err := shell.Stdin.Write(data); err != nil {
					stop("connexion à la cible perdue")
					return
				}
				continue
			}
			var m controlMsg
			if json.Unmarshal(data, &m) == nil && m.Type == "resize" {
				c, r := clamp(m.Cols, 20, maxCols, cols), clamp(m.Rows, 5, maxRows, rows)
				_ = shell.Resize(c, r)
				rec.Resize(c, r)
			}
		}
	}()

	// Supervision : la session web doit rester valide (déconnexion, compte désactivé,
	// expiration ou inactivité ferment aussi le terminal).
	go s.supervise(ctx, id, t.ID, &userActive, func(why string) {
		msg := "session Gná expirée : reconnectez-vous"
		if why == "accès à la cible retiré" {
			msg = "votre accès à cette cible a été retiré"
		}
		sendControl(ws, controlMsg{Type: "error", Message: msg})
		stop(why)
	})

	<-ctx.Done()
	ws.Close(websocket.StatusNormalClosure, "")
	rec.Marker(reason)

	endCtx, endCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer endCancel()
	if accessID != 0 {
		_ = s.st.EndAccessSession(endCtx, accessID, reason, bytesIn.Load(), bytesOut.Load())
	}
	_ = s.st.Audit(endCtx, &id.User.ID, id.User.Username, "ssh.session_end", ip, map[string]any{
		"target": t.Name, "session": accessID, "reason": reason,
		"duration_s": int(time.Since(started).Seconds()), "bytes_in": bytesIn.Load(), "bytes_out": bytesOut.Load(),
	})
}

func sendControl(ws *websocket.Conn, m controlMsg) {
	b, _ := json.Marshal(m)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_ = ws.Write(ctx, websocket.MessageText, b)
}

func atoi(s string) int { n, _ := strconv.Atoi(s); return n }

func clamp(v, lo, hi, def int) int {
	if v == 0 {
		return def
	}
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}
