// Package rdp pilote la passerelle gna-rdp (programme Rust fondé sur IronRDP).
//
// Pour chaque session, Gná lance la passerelle, lui transmet les paramètres de
// connexion (identifiants compris) sur son entrée standard, attend sa ligne d'état,
// puis dialogue avec elle en VNC (RFB) : la session RDP réutilise ainsi l'affichage,
// l'enregistrement et la relecture des sessions VNC.
package rdp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Erreurs signalées par la passerelle, distinguées pour l'audit.
var (
	ErrCertMismatch = errors.New("certificat RDP différent de celui enregistré")
	ErrAuth         = errors.New("authentification RDP refusée")
	ErrNoBridge     = errors.New("passerelle RDP (gna-rdp) introuvable : relancer l'installation de Gná")
)

// Layouts acceptés pour le clavier de la cible.
var Layouts = map[string]string{
	"fr":       "Français (Windows)",
	"fr-linux": "Français (Linux / xrdp)",
	"us":       "Anglais (États-Unis)",
}

// Bridge localise l'exécutable gna-rdp.
func Bridge(configured string) (string, error) {
	cands := []string{}
	if configured != "" {
		cands = append(cands, configured)
	}
	if exe, err := os.Executable(); err == nil {
		cands = append(cands, filepath.Join(filepath.Dir(exe), "gna-rdp"))
	}
	cands = append(cands, "/usr/local/bin/gna-rdp")
	for _, c := range cands {
		if st, err := os.Stat(c); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return c, nil
		}
	}
	return "", ErrNoBridge
}

type status struct {
	Status      string `json:"status"`
	Kind        string `json:"kind"`
	Message     string `json:"message"`
	Fingerprint string `json:"fingerprint"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
}

func statusError(s status) error {
	switch s.Kind {
	case "cert":
		return fmt.Errorf("%w : %s", ErrCertMismatch, s.Message)
	case "auth":
		return fmt.Errorf("%w : %s", ErrAuth, s.Message)
	default:
		return errors.New(s.Message)
	}
}

// Probe relève l'empreinte SHA-256 du certificat RDP de la cible.
func Probe(ctx context.Context, bin, host string, port int) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, bin, "probe", host, strconv.Itoa(port))
	out, _ := cmd.CombinedOutput()
	var s status
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if json.Unmarshal([]byte(line), &s) == nil && s.Status != "" {
			break
		}
	}
	switch s.Status {
	case "fingerprint":
		return s.Fingerprint, nil
	case "error":
		return "", statusError(s)
	}
	if ctx.Err() != nil {
		return "", errors.New("délai dépassé : la cible ne répond pas en RDP")
	}
	return "", fmt.Errorf("réponse inattendue de la passerelle RDP : %.200s", out)
}

// Params décrit une connexion RDP.
type Params struct {
	Host        string `json:"host"`
	Port        int    `json:"port"`
	Username    string `json:"username"`
	Password    string `json:"password"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Fingerprint string `json:"fingerprint"`
	Layout      string `json:"layout"`
	Title       string `json:"title"`
	Clipboard   bool   `json:"clipboard"`
	DriveDir    string `json:"drive_dir,omitempty"` // dossier partagé (lecteur « Gna »)
	DriveMaxMB  int    `json:"drive_max_mb,omitempty"`
}

// Conn est le flux VNC exposé par la passerelle pour une session.
type Conn struct {
	cmd    *exec.Cmd
	in     io.WriteCloser
	out    io.ReadCloser
	once   sync.Once
	Width  int
	Height int
}

func (c *Conn) Read(p []byte) (int, error)  { return c.out.Read(p) }
func (c *Conn) Write(p []byte) (int, error) { return c.in.Write(p) }

// Close termine la session : fermeture propre de l'entrée (la passerelle déconnecte
// la session RDP), puis arrêt forcé si elle ne s'est pas terminée.
func (c *Conn) Close() error {
	c.once.Do(func() {
		_ = c.in.Close()
		done := make(chan struct{})
		go func() { _ = c.cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			_ = c.cmd.Process.Kill()
			<-done
		}
	})
	return nil
}

// Start lance la passerelle et ouvre la session RDP. Les identifiants transitent
// uniquement par le tube vers la passerelle (jamais en argument ni en variable
// d'environnement, visibles par les autres processus).
func Start(ctx context.Context, bin string, p Params) (*Conn, error) {
	cmd := exec.Command(bin, "bridge")
	cmd.Env = []string{"PATH=/usr/bin:/bin"}
	if os.Getenv("GNA_RDP_DEBUG") != "" {
		cmd.Env = append(cmd.Env, "GNA_RDP_DEBUG=1") // journalise les événements clavier (diagnostic)
	}
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	errPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("lancement de la passerelle RDP : %w", err)
	}
	conn := &Conn{cmd: cmd, in: in, out: out}

	line, _ := json.Marshal(p)
	if _, err := in.Write(append(line, '\n')); err != nil {
		conn.Close()
		return nil, fmt.Errorf("passerelle RDP : %w", err)
	}
	for i := range line {
		line[i] = 0 // effacer la copie contenant le mot de passe
	}

	// Première ligne de la sortie d'erreur : l'état de la connexion. Les suivantes
	// sont des messages de diagnostic, journalisés.
	errs := bufio.NewScanner(errPipe)
	first := make(chan status, 1)
	go func() {
		got := false
		for errs.Scan() {
			var s status
			if !got && json.Unmarshal(errs.Bytes(), &s) == nil && s.Status != "" {
				first <- s
				got = true
				continue
			}
			slog.Info("passerelle RDP", "message", errs.Text())
		}
		if !got {
			first <- status{Status: "error", Kind: "protocol", Message: "la passerelle RDP s'est arrêtée sans réponse"}
		}
	}()

	select {
	case s := <-first:
		if s.Status == "ready" {
			conn.Width, conn.Height = s.Width, s.Height
			return conn, nil
		}
		conn.Close()
		return nil, statusError(s)
	case <-time.After(45 * time.Second):
		conn.Close()
		return nil, errors.New("délai dépassé pendant la connexion RDP")
	case <-ctx.Done():
		conn.Close()
		return nil, ctx.Err()
	}
}
