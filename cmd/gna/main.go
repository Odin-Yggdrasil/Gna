// Gná — bastion multi-protocole.
package main

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"flag"
	"fmt"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strings"
	"syscall"
	"time"

	"golang.org/x/term"

	"gna/internal/api"
	"gna/internal/auth"
	"gna/internal/config"
	"gna/internal/secure"
	"gna/internal/store"
	"gna/internal/tlsutil"
	"gna/internal/web"
)

// version est fixée à la compilation (-ldflags "-X main.version=…", voir le Makefile).
var version = "dev"

const usage = `Gná — bastion multi-protocole

Utilisation :
  gna serve                         démarre le serveur
  gna version                       affiche la version
  gna migrate                       applique les migrations de la base
  gna keygen                        génère une clé maître (GNA_MASTER_KEY)
  gna tls info                      affiche l'empreinte du certificat HTTPS
  gna tls regen                     régénère le certificat auto-signé (après un changement d'IP)
  gna user add <nom> [--admin]      crée un utilisateur (mot de passe demandé)
  gna user list                     liste les utilisateurs
  gna user passwd <nom>             change le mot de passe
  gna user reset-mfa <nom>          réinitialise la MFA (réenrôlement au prochain login)
  gna user disable <nom>            désactive un compte et ferme ses sessions
  gna user enable <nom>             réactive un compte
  gna user unlock <nom>             lève un verrouillage après trop d'échecs
`

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if len(os.Args) < 2 {
		fmt.Print(usage)
		os.Exit(2)
	}
	var err error
	switch os.Args[1] {
	case "serve":
		err = serve()
	case "migrate":
		err = withStore(func(ctx context.Context, st *store.Store, _ *config.Config) error { return nil })
		if err == nil {
			fmt.Println("Migrations appliquées.")
		}
	case "version", "--version":
		fmt.Println("gna", version)
	case "keygen":
		k := make([]byte, 32)
		if _, err = rand.Read(k); err == nil {
			fmt.Println(base64.StdEncoding.EncodeToString(k))
		}
	case "user":
		err = userCmd(os.Args[2:])
	case "tls":
		err = tlsCmd(os.Args[2:])
	case "-h", "--help", "help":
		fmt.Print(usage)
	default:
		fmt.Print(usage)
		os.Exit(2)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, "erreur :", err)
		os.Exit(1)
	}
}

// withStore charge la config, ouvre la base et applique les migrations.
func withStore(fn func(context.Context, *store.Store, *config.Config) error) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	ctx := context.Background()
	st, err := store.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return err
	}
	defer st.Close()
	if err := st.Migrate(ctx); err != nil {
		return err
	}
	return fn(ctx, st, cfg)
}

func serve() error {
	return withStore(func(ctx context.Context, st *store.Store, cfg *config.Config) error {
		box, err := secure.NewBox(cfg.MasterKey)
		if err != nil {
			return err
		}
		if n, err := st.CloseDanglingAccessSessions(ctx); err == nil && n > 0 {
			slog.Warn("sessions d'accès restées ouvertes après un arrêt, clôturées", "n", n)
		}
		tlsConf, err := prepareTLS(cfg)
		if err != nil {
			return err
		}
		srv := &http.Server{
			Addr:              cfg.ListenAddr,
			TLSConfig:         tlsConf,
			Handler:           api.New(cfg, st, auth.New(st, cfg, box), web.Static()).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       30 * time.Second,
			IdleTimeout:       120 * time.Second,
		}

		ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
		defer stop()

		if cfg.RecordSessions || cfg.RecordVNC || cfg.RetentionDays > 0 {
			go purgeRecordings(ctx, st, cfg)
		}

		// Nettoyage périodique des sessions expirées ou inactives.
		go func() {
			t := time.NewTicker(5 * time.Minute)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					if n, err := st.PurgeExpiredSessions(ctx, cfg.IdleTimeout); err != nil {
						slog.Error("purge des sessions", "err", err)
					} else if n > 0 {
						slog.Info("sessions purgées", "n", n)
					}
				}
			}
		}()

		errc := make(chan error, 1)
		go func() {
			slog.Info("Gná démarré", "version", version, "adresse", cfg.ListenAddr, "https", tlsConf != nil, "cookie_secure", cfg.CookieSecure)
			if tlsConf != nil {
				errc <- srv.ListenAndServeTLS("", "")
			} else {
				errc <- srv.ListenAndServe()
			}
		}()
		select {
		case err := <-errc:
			return err
		case <-ctx.Done():
			slog.Info("arrêt en cours…")
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return srv.Shutdown(shutdownCtx)
		}
	})
}

// purgeRecordings supprime, toutes les heures, les enregistrements plus anciens que
// la durée de conservation (GNA_RECORDING_RETENTION_DAYS, 0 = jamais).
func purgeRecordings(ctx context.Context, st *store.Store, cfg *config.Config) {
	if cfg.RetentionDays == 0 {
		return
	}
	tick := time.NewTicker(time.Hour)
	defer tick.Stop()
	for {
		expired, err := st.ExpiredRecordings(ctx, time.Now().AddDate(0, 0, -cfg.RetentionDays))
		if err != nil {
			slog.Error("recherche des enregistrements expirés", "err", err)
		}
		for id, path := range expired {
			if err := os.Remove(path); err != nil && !errors.Is(err, fs.ErrNotExist) {
				slog.Error("suppression d'un enregistrement", "chemin", path, "err", err)
				continue
			}
			if err := st.MarkRecordingPurged(ctx, id); err == nil {
				_ = st.Audit(ctx, nil, "", "recording.purged", "", map[string]any{"session": id, "retention_days": cfg.RetentionDays})
			}
		}
		if len(expired) > 0 {
			slog.Info("enregistrements expirés supprimés", "n", len(expired))
		}
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// prepareTLS renvoie la configuration HTTPS, ou nil en HTTP simple.
// Le HTTP en clair n'est accepté que sur localhost (ou derrière un reverse proxy) :
// sinon mots de passe et codes TOTP circuleraient en clair sur le réseau.
func prepareTLS(cfg *config.Config) (*tls.Config, error) {
	if !cfg.TLSEnabled() {
		if !tlsutil.IsLoopback(cfg.ListenAddr) && !cfg.TrustProxy && !cfg.AllowPlainHTTP {
			return nil, fmt.Errorf("refus d'écouter en HTTP en clair sur %s : activer GNA_TLS_SELF_SIGNED=true "+
				"(ou GNA_TLS_CERT/GNA_TLS_KEY), ou écouter sur 127.0.0.1 derrière un reverse proxy", cfg.ListenAddr)
		}
		return nil, nil
	}
	cfg.CookieSecure = true // en HTTPS, le cookie est toujours réservé au HTTPS

	certPath, keyPath := cfg.TLSCert, cfg.TLSKey
	if cfg.TLSSelfSigned {
		var created bool
		var err error
		certPath, keyPath, created, err = tlsutil.EnsureSelfSigned(cfg.DataDir)
		if err != nil {
			return nil, fmt.Errorf("certificat auto-signé : %w", err)
		}
		if created {
			slog.Info("certificat auto-signé créé", "chemin", certPath)
		}
	}
	if info, err := tlsutil.Describe(certPath); err == nil {
		slog.Info("certificat HTTPS", "empreinte_sha256", info.Fingerprint, "noms", strings.Join(info.Names, ","),
			"expire", info.NotAfter.Format("2006-01-02"))
		if time.Until(info.NotAfter) < 30*24*time.Hour {
			slog.Warn("le certificat expire bientôt", "expire", info.NotAfter.Format("2006-01-02"))
		}
	}
	return tlsutil.Config(certPath, keyPath)
}

func tlsCmd(args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	certPath := cfg.TLSCert
	if certPath == "" {
		certPath, _ = tlsutil.Paths(cfg.DataDir)
	}
	sub := ""
	if len(args) > 0 {
		sub = args[0]
	}
	switch sub {
	case "regen":
		if !cfg.TLSSelfSigned {
			return errors.New("GNA_TLS_SELF_SIGNED n'est pas activé : rien à régénérer")
		}
		if err := tlsutil.Generate(cfg.DataDir); err != nil {
			return err
		}
		fmt.Println("Certificat régénéré. Redémarrer le service : sudo systemctl restart gna")
		fallthrough
	case "info", "":
		info, err := tlsutil.Describe(certPath)
		if err != nil {
			return err
		}
		fmt.Println("Certificat :", certPath)
		fmt.Println("Empreinte SHA-256 :", info.Fingerprint)
		fmt.Println("Valable pour :", strings.Join(info.Names, ", "))
		fmt.Println("Expire le :", info.NotAfter.Local().Format("02/01/2006"))
		return nil
	default:
		return fmt.Errorf("sous-commande inconnue : %s (info ou regen)", sub)
	}
}

var usernameRe = regexp.MustCompile(`^[a-z0-9._-]{2,64}$`)

func userCmd(args []string) error {
	if len(args) == 0 {
		fmt.Print(usage)
		return errors.New("sous-commande manquante")
	}
	sub := args[0]
	if sub == "list" {
		return withStore(func(ctx context.Context, st *store.Store, _ *config.Config) error {
			users, err := st.ListUsers(ctx)
			if err != nil {
				return err
			}
			fmt.Printf("%-24s %-6s %-5s %-10s %s\n", "UTILISATEUR", "ADMIN", "MFA", "ÉTAT", "CRÉÉ LE")
			for _, u := range users {
				state := "actif"
				if u.Disabled {
					state = "désactivé"
				} else if u.LockedUntil != nil && u.LockedUntil.After(time.Now()) {
					state = "verrouillé"
				}
				fmt.Printf("%-24s %-6s %-5s %-10s %s\n", u.Username, yesNo(u.IsAdmin), yesNo(u.TOTPEnabled), state,
					u.CreatedAt.Local().Format("2006-01-02 15:04"))
			}
			return nil
		})
	}

	fs := flag.NewFlagSet("user "+sub, flag.ContinueOnError)
	isAdmin := fs.Bool("admin", false, "donner les droits d'administration")
	// Accepter le nom avant ou après les options.
	var name string
	rest := args[1:]
	if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
		name, rest = rest[0], rest[1:]
	}
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if name == "" && fs.NArg() > 0 {
		name = fs.Arg(0)
	}
	name = strings.ToLower(strings.TrimSpace(name))
	if name == "" {
		return errors.New("nom d'utilisateur manquant")
	}

	return withStore(func(ctx context.Context, st *store.Store, _ *config.Config) error {
		if sub == "add" {
			if !usernameRe.MatchString(name) {
				return errors.New("nom invalide : 2 à 64 caractères parmi a-z, 0-9, « . », « _ », « - »")
			}
			hash, err := askPassword()
			if err != nil {
				return err
			}
			id, err := st.CreateUser(ctx, name, hash, *isAdmin)
			if errors.Is(err, store.ErrDuplicate) {
				return fmt.Errorf("l'utilisateur %q existe déjà", name)
			}
			if err != nil {
				return err
			}
			_ = st.Audit(ctx, &id, name, "user.created", "cli", map[string]any{"admin": *isAdmin})
			fmt.Printf("Utilisateur %q créé. La MFA sera configurée à sa première connexion.\n", name)
			return nil
		}

		u, err := st.UserByUsername(ctx, name)
		if errors.Is(err, store.ErrNotFound) {
			return fmt.Errorf("utilisateur %q introuvable", name)
		}
		if err != nil {
			return err
		}
		switch sub {
		case "passwd":
			hash, err := askPassword()
			if err != nil {
				return err
			}
			err = st.SetPassword(ctx, u.ID, hash)
			if err == nil {
				_ = st.Audit(ctx, &u.ID, u.Username, "user.password_changed", "cli", nil)
				fmt.Println("Mot de passe changé, sessions fermées.")
			}
			return err
		case "reset-mfa":
			err = st.ResetTOTP(ctx, u.ID)
			if err == nil {
				_ = st.Audit(ctx, &u.ID, u.Username, "mfa.reset", "cli", nil)
				fmt.Println("MFA réinitialisée, sessions fermées. Réenrôlement à la prochaine connexion.")
			}
			return err
		case "disable", "enable":
			err = st.SetDisabled(ctx, u.ID, sub == "disable")
			if err == nil {
				_ = st.Audit(ctx, &u.ID, u.Username, "user."+sub+"d", "cli", nil)
				fmt.Println("Fait.")
			}
			return err
		case "unlock":
			err = st.ResetLoginFailures(ctx, u.ID)
			if err == nil {
				_ = st.Audit(ctx, &u.ID, u.Username, "account.unlocked", "cli", nil)
				fmt.Println("Compte déverrouillé.")
			}
			return err
		default:
			return fmt.Errorf("sous-commande inconnue : %s", sub)
		}
	})
}

// askPassword lit le mot de passe sans écho (ou depuis stdin s'il n'y a pas de terminal,
// pratique pour l'automatisation : echo "…" | gna user add bob).
func askPassword() (string, error) {
	var pw string
	if term.IsTerminal(int(os.Stdin.Fd())) {
		fmt.Fprint(os.Stderr, "Mot de passe : ")
		a, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		fmt.Fprint(os.Stderr, "Confirmation : ")
		b, err := term.ReadPassword(int(os.Stdin.Fd()))
		fmt.Fprintln(os.Stderr)
		if err != nil {
			return "", err
		}
		if string(a) != string(b) {
			return "", errors.New("les mots de passe ne correspondent pas")
		}
		pw = string(a)
	} else {
		line, err := bufio.NewReader(os.Stdin).ReadString('\n')
		if err != nil && line == "" {
			return "", errors.New("aucun mot de passe reçu sur l'entrée standard")
		}
		pw = strings.TrimRight(line, "\r\n")
	}
	if err := secure.CheckPasswordPolicy(pw); err != nil {
		return "", err
	}
	return secure.HashPassword(pw)
}

func yesNo(b bool) string {
	if b {
		return "oui"
	}
	return "non"
}
