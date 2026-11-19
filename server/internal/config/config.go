// Package config charge la configuration de Gná depuis les variables d'environnement.
package config

import (
	"encoding/base64"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	ListenAddr   string        // GNA_LISTEN (défaut ":8080")
	DatabaseURL  string        // GNA_DATABASE_URL (obligatoire)
	MasterKey    []byte        // GNA_MASTER_KEY : 32 octets en base64 (obligatoire)
	CookieSecure bool          // GNA_COOKIE_SECURE (défaut true, mettre false uniquement en dev HTTP)
	SessionTTL   time.Duration // GNA_SESSION_TTL : durée max d'une session (défaut 12h)
	IdleTimeout  time.Duration // GNA_IDLE_TIMEOUT : déconnexion après inactivité (défaut 30m)
	Issuer       string        // GNA_ISSUER : nom affiché dans l'appli TOTP (défaut "Gná")
	TrustProxy   bool          // GNA_TRUST_PROXY : lire l'IP client dans X-Forwarded-For (défaut false)
	MaxFailures  int           // GNA_MAX_FAILURES : échecs avant verrouillage (défaut 5)
	LockDuration time.Duration // GNA_LOCK_DURATION : durée du verrouillage (défaut 15m)

	DataDir        string // GNA_DATA_DIR : données locales, certificat… (défaut $STATE_DIRECTORY ou /var/lib/gna)
	TLSCert        string // GNA_TLS_CERT : certificat fourni (PEM)
	TLSKey         string // GNA_TLS_KEY  : clé privée fournie (PEM)
	TLSSelfSigned  bool   // GNA_TLS_SELF_SIGNED : générer un certificat auto-signé dans DataDir
	AllowPlainHTTP bool   // GNA_ALLOW_PLAIN_HTTP : autoriser le HTTP en clair hors localhost (déconseillé)

	RecordSessions bool // GNA_RECORD_SESSIONS : enregistrer les sessions SSH (défaut true)
	RecordInput    bool // GNA_RECORD_INPUT : enregistrer aussi les frappes clavier (défaut false)
	RecordVNC      bool // GNA_RECORD_VNC : enregistrer les sessions VNC (défaut : comme GNA_RECORD_SESSIONS)
	RetentionDays  int  // GNA_RECORDING_RETENTION_DAYS : durée de conservation (défaut 90, 0 = illimitée)

	RDPBridge string // GNA_RDP_BRIDGE : chemin de gna-rdp (défaut : à côté de gna, puis /usr/local/bin)

	Clipboard     bool // GNA_CLIPBOARD : copier-coller dans les bureaux à distance (défaut true)
	FileTransfer  bool // GNA_FILE_TRANSFER : transfert de fichiers RDP (défaut true)
	TransferMaxMB int  // GNA_TRANSFER_MAX_MB : taille maximale du dossier de transfert d'une session (défaut 1024)
}

// TransfersDir contient un dossier de transfert par session RDP en cours.
func (c *Config) TransfersDir() string { return c.DataDir + "/transfers" }

// RecordingsDir est le dossier des enregistrements chiffrés.
func (c *Config) RecordingsDir() string { return c.DataDir + "/recordings" }

// TLSEnabled indique si Gná sert lui-même du HTTPS.
func (c *Config) TLSEnabled() bool { return c.TLSSelfSigned || c.TLSCert != "" }

// DefaultFile est lu automatiquement s'il existe. GNA_CONFIG permet d'en choisir un autre.
const DefaultFile = "/etc/gna/gna.env"

// Load lit la configuration : d'abord le fichier de configuration, puis les variables
// d'environnement (qui ont priorité). Seules la base et la clé maître sont exigées.
func Load() (*Config, error) {
	path, explicit := os.LookupEnv("GNA_CONFIG")
	if !explicit {
		path = DefaultFile
	}
	if err := loadEnvFile(path, explicit); err != nil {
		return nil, err
	}

	c := &Config{
		ListenAddr:   env("GNA_LISTEN", ":8080"),
		DatabaseURL:  os.Getenv("GNA_DATABASE_URL"),
		Issuer:       env("GNA_ISSUER", "Gná"),
		CookieSecure: true,
		SessionTTL:   12 * time.Hour,
		IdleTimeout:  30 * time.Minute,
		MaxFailures:  5,
		LockDuration: 15 * time.Minute,
	}
	if c.DatabaseURL == "" {
		return nil, fmt.Errorf("GNA_DATABASE_URL est obligatoire")
	}

	key, err := base64.StdEncoding.DecodeString(os.Getenv("GNA_MASTER_KEY"))
	if err != nil || len(key) != 32 {
		return nil, fmt.Errorf("GNA_MASTER_KEY doit contenir 32 octets encodés en base64 (générer avec : gna keygen)")
	}
	c.MasterKey = key

	c.DataDir = env("GNA_DATA_DIR", env("STATE_DIRECTORY", "/var/lib/gna"))
	c.TLSCert = os.Getenv("GNA_TLS_CERT")
	c.TLSKey = os.Getenv("GNA_TLS_KEY")
	if (c.TLSCert == "") != (c.TLSKey == "") {
		return nil, fmt.Errorf("GNA_TLS_CERT et GNA_TLS_KEY doivent être définis ensemble")
	}

	var errs []error
	c.TLSSelfSigned = parseBool("GNA_TLS_SELF_SIGNED", false, &errs)
	c.AllowPlainHTTP = parseBool("GNA_ALLOW_PLAIN_HTTP", false, &errs)
	c.RecordSessions = parseBool("GNA_RECORD_SESSIONS", true, &errs)
	c.RecordInput = parseBool("GNA_RECORD_INPUT", false, &errs)
	c.RecordVNC = parseBool("GNA_RECORD_VNC", c.RecordSessions, &errs)
	c.RDPBridge = os.Getenv("GNA_RDP_BRIDGE")
	c.Clipboard = parseBool("GNA_CLIPBOARD", true, &errs)
	c.FileTransfer = parseBool("GNA_FILE_TRANSFER", true, &errs)
	c.TransferMaxMB = 1024
	if v := os.Getenv("GNA_TRANSFER_MAX_MB"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > 1<<20 {
			errs = append(errs, fmt.Errorf("GNA_TRANSFER_MAX_MB invalide : %q", v))
		} else {
			c.TransferMaxMB = n
		}
	}
	c.RetentionDays = 90
	if v := os.Getenv("GNA_RECORDING_RETENTION_DAYS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			errs = append(errs, fmt.Errorf("GNA_RECORDING_RETENTION_DAYS invalide : %q", v))
		} else {
			c.RetentionDays = n
		}
	}
	if c.TLSSelfSigned && c.TLSCert != "" {
		errs = append(errs, fmt.Errorf("choisir soit GNA_TLS_SELF_SIGNED, soit GNA_TLS_CERT/GNA_TLS_KEY, pas les deux"))
	}
	c.CookieSecure = parseBool("GNA_COOKIE_SECURE", c.CookieSecure, &errs)
	c.TrustProxy = parseBool("GNA_TRUST_PROXY", false, &errs)
	c.SessionTTL = parseDur("GNA_SESSION_TTL", c.SessionTTL, &errs)
	c.IdleTimeout = parseDur("GNA_IDLE_TIMEOUT", c.IdleTimeout, &errs)
	c.LockDuration = parseDur("GNA_LOCK_DURATION", c.LockDuration, &errs)
	if v := os.Getenv("GNA_MAX_FAILURES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 {
			errs = append(errs, fmt.Errorf("GNA_MAX_FAILURES invalide : %q", v))
		} else {
			c.MaxFailures = n
		}
	}
	if len(errs) > 0 {
		return nil, errs[0]
	}
	return c, nil
}

// loadEnvFile lit des lignes CLE=valeur (commentaires # autorisés, guillemets facultatifs).
// Une variable déjà définie dans l'environnement n'est pas écrasée.
// Le fichier contient la clé maître : il est refusé s'il est lisible par tout le monde.
func loadEnvFile(path string, required bool) error {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) && !required {
		return nil
	}
	if err != nil {
		return fmt.Errorf("fichier de configuration %s : %w", path, err)
	}
	if info.Mode().Perm()&0o004 != 0 {
		return fmt.Errorf("%s est lisible par tous les utilisateurs : corriger avec « chmod 640 %s »", path, path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("fichier de configuration %s : %w", path, err)
	}
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		k = strings.TrimSpace(strings.TrimPrefix(k, "export "))
		if !ok || k == "" {
			return fmt.Errorf("%s ligne %d : format attendu CLE=valeur", path, i+1)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '"' || v[0] == '\'') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		}
		if _, set := os.LookupEnv(k); !set {
			os.Setenv(k, v)
		}
	}
	return nil
}

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func parseBool(k string, def bool, errs *[]error) bool {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s invalide : %q", k, v))
		return def
	}
	return b
}

func parseDur(k string, def time.Duration, errs *[]error) time.Duration {
	v := os.Getenv(k)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil || d <= 0 {
		*errs = append(*errs, fmt.Errorf("%s invalide : %q (exemple : 30m, 12h)", k, v))
		return def
	}
	return d
}
