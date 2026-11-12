// SPDX-License-Identifier: GPL-3.0-or-later

// Package config décrit la configuration de mirador-agent.
//
// Deux sources possibles, une seule structure :
//   - le fichier local /etc/mirador-agent/agent.conf (format CLE=valeur, comme
//     /etc/default), lu au démarrage ;
//   - plus tard (étape 3), la configuration distante envoyée par le serveur en
//     JSON. Elle est désactivée tant que le serveur n'existe pas.
//
// Règle de sécurité (SPEC.md § 5.2) : la configuration ne contient QUE des
// valeurs typées et bornées. Jamais de commande, de chemin d'exécutable ni de
// script. Tout champ inconnu est refusé.
package config

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/protocole"
)

// Agent regroupe tous les réglages de l'agent.
//
// Les étiquettes json servent à la future configuration distante (étape 3).
type Agent struct {
	// Identité de la machine (labels envoyés avec chaque métrique et chaque log).
	Nom  string `json:"nom"`  // label instance (métriques) et host (logs)
	Site string `json:"site"` // label site
	Env  string `json:"env"`  // label env

	// Destinations (étape 2 : VictoriaMetrics et VictoriaLogs, derrière nginx).
	URLMetriques string `json:"url_metriques"` // ex. https://ingest.example.org/api/v1/write
	URLLogs      string `json:"url_logs"`      // ex. https://ingest.example.org/insert/jsonline
	// FichierToken : chemin du fichier qui contient le token (droits 0600 ou 0640).
	// Le token lui-même n'est jamais écrit dans ce fichier de configuration.
	FichierToken string `json:"-"`
	// FichierCles : clés publiques de signature des versions, posées à
	// l'installation (root, 0644 : l'agent les lit sans pouvoir les changer).
	FichierCles string `json:"-"`

	// Serveur : adresse de mirador-server (https://…). Renseignée, elle active
	// le protocole v1 : enrôlement, configuration distante, battement, et les
	// adresses de métriques et de logs en découlent si elles sont vides.
	Serveur string `json:"-"`
	// FichierJeton : jeton d'enrôlement à usage unique, lu seulement si le
	// fichier du token n'existe pas encore.
	FichierJeton string `json:"-"`
	// MajAuto : mises à jour signées automatiques. Seulement dans le fichier
	// local (jamais réglable à distance) ; exige Serveur.
	MajAuto bool `json:"-"`
	// FichierConfigDistante : dernière configuration reçue du serveur (relue
	// au démarrage, sans attendre le réseau).
	FichierConfigDistante string `json:"-"`
	// FichierActualiser : touché par l'agent quand l'interface demande
	// « Actualiser » ; une unité systemd (mirador-agent-root.path) relance
	// alors l'assistant root. Dans le dossier de l'agent.
	FichierActualiser string `json:"-"`

	// Rythmes de collecte.
	Intervalle     time.Duration `json:"intervalle"`      // métriques rapides (défaut 30 s)
	IntervalleLent time.Duration `json:"intervalle_lent"` // températures (défaut 60 s)

	// Tampon mémoire des métriques en attente d'envoi (octets compressés).
	TamponMetriques int `json:"tampon_metriques"`

	// Exclusions (expressions régulières, mêmes valeurs par défaut qu'Alloy).
	ExclureMontages       string `json:"exclure_montages"`
	ExclureTypesFS        string `json:"exclure_types_fs"`
	ExclureDisques        string `json:"exclure_disques"`
	ExclureInterfaces     string `json:"exclure_interfaces"`
	ExclureTypesFSDistant string `json:"exclure_types_fs_distant"`

	// Collecteurs optionnels (partie 3 de l'étape 2).
	Systemd        bool     `json:"systemd"`         // état des services (D-Bus)
	SystemdIgnorer []string `json:"systemd_ignorer"` // services exclus de « activé mais arrêté »
	MisesAJour     bool     `json:"mises_a_jour"`    // apt-get --simulate toutes les 6 h
	// Docker : adresse du proxy de socket en lecture seule, vide = désactivé.
	// Forme : http://127.0.0.1:2375 ou unix:///chemin/absolu.
	Docker string `json:"docker"`

	// Surveillance choisie dans l'interface (SPEC.md § 19) : ce que la machine
	// envoie ou non. Les trois derniers sont nouveaux et éteints par défaut :
	// l'agent signale seulement qu'ils existent, l'interface propose de les
	// activer.
	Temperatures    bool `json:"temperatures"`     // capteurs de /sys (défaut oui)
	DockerMetriques bool `json:"docker_metriques"` // métriques des conteneurs (défaut oui, si Docker est configuré)
	Ventilateurs    bool `json:"ventilateurs"`     // vitesse des ventilateurs (/sys/class/hwmon)
	ZFS             bool `json:"zfs"`              // santé des pools ZFS (/proc/spl/kstat/zfs)
	Mdraid          bool `json:"mdraid"`           // RAID logiciel (/sys/block/md*)
	// Services systemd validés un par un : avec SystemdValidation, seuls les
	// services de SystemdSurveiller sont suivis ; les autres services démarrés
	// au boot sont seulement signalés (« à valider » dans l'interface).
	SystemdValidation bool     `json:"systemd_validation"`
	SystemdSurveiller []string `json:"systemd_surveiller"`
	// Résultats de l'assistant root : envoyés ou non. L'assistant lui-même ne
	// lit jamais la configuration distante (il continue de tourner).
	EnvoyerPareFeu     bool `json:"envoyer_pare_feu"`
	EnvoyerSmart       bool `json:"envoyer_smart"`
	EnvoyerCertificats bool `json:"envoyer_certificats"`

	// Éléments retirés un par un dans l'interface (SPEC.md § 21) : noms
	// exacts, en plus des expressions MIRADOR_EXCLURE_*.
	IgnorerDisques    []string `json:"ignorer_disques"`    // ex. sdb
	IgnorerInterfaces []string `json:"ignorer_interfaces"` // ex. ens19
	IgnorerMontages   []string `json:"ignorer_montages"`   // ex. /mnt/sauvegarde
	// Certificats choisis dans l'interface (SPEC.md § 21) :
	//   - CertificatsAjout : chemins en plus de MIRADOR_CERTIFICATS, lus par
	//     l'assistant root avec des garde-fous (ValiderMotifDistant) ;
	//   - CertificatsIgnorer : certificats trouvés mais pas envoyés.
	CertificatsAjout   []string `json:"certificats_ajout"`
	CertificatsIgnorer []string `json:"certificats_ignorer"`
	// CertificatsDistants : réglage LOCAL seulement (le serveur ne peut pas
	// le changer). Non = l'assistant root ignore les chemins venus du serveur.
	CertificatsDistants bool `json:"-"`

	// Logs du journal (partie 5).
	// Metriques : collecte et envoi des métriques de la machine (défaut oui).
	// Non : « logs seulement » ; l'agent n'envoie alors que ses propres
	// métriques (~20 séries : version, mémoire, file d'envoi…), pour que le
	// serveur sache qu'il est vivant.
	Metriques      bool     `json:"metriques"`
	Journal        bool     `json:"journal"`        // lecture de journald (si URLLogs est renseignée)
	TamponLogs     int      `json:"tampon_logs"`    // file d'envoi des logs (octets compressés)
	LogsDebit      float64  `json:"logs_debit"`     // lignes par seconde autorisées
	LogsRafale     float64  `json:"logs_rafale"`    // rafale autorisée
	LogsBruit      string   `json:"logs_bruit"`     // expression des lignes jetées
	Masquage       []string `json:"masquage"`       // modèles de masquage actifs
	MasquagePerso  []string `json:"masquage_perso"` // expressions personnalisées (10 max)
	FichierCurseur string   `json:"-"`              // position de lecture du journal
	LogsDocker     bool     `json:"logs_docker"`    // logs des conteneurs (si Docker est configuré)

	// Assistant root (partie 4) : ce qu'il mesure, et où il écrit.
	PareFeu     bool     `json:"pare_feu"`    // politique INPUT (iptables / nftables)
	Smart       bool     `json:"smart"`       // disques physiques uniquement
	Certificats []string `json:"certificats"` // motifs de fichiers, vide = rien
	FichierRoot string   `json:"-"`           // résultat de l'assistant (dans /run)

	// Racines de /proc et /sys : modifiables pour les tests uniquement.
	RacineProc string `json:"-"`
	RacineSys  string `json:"-"`

	// Niveau des journaux de l'agent : debug, info, warn, error.
	NiveauLog string `json:"niveau_log"`
}

// Valeurs par défaut. Elles reprennent celles d'Alloy (SPEC.md § 4.1).
const (
	defautMontages   = `^/(dev|proc|sys|run/credentials/.+|var/lib/docker/.+|var/lib/containers/.+|var/lib/lxcfs/.+)($|/)`
	defautTypesFS    = `^(autofs|binfmt_misc|bpf|cgroup2?|configfs|debugfs|devpts|devtmpfs|fuse\.lxcfs|fusectl|hugetlbfs|iso9660|mqueue|nsfs|overlay|proc|procfs|pstore|rpc_pipefs|securityfs|selinuxfs|squashfs|sysfs|tracefs)$`
	defautDisques    = `^(ram|loop|fd|dm-|sr|nbd|zd)\d+$`
	defautInterfaces = `^(veth|docker|br-|lo|virbr|tap|fwbr|fwln|fwpr)`
	// Systèmes de fichiers réseau : lus avec un délai maximal (un serveur NFS
	// injoignable bloquerait sinon la collecte).
	defautTypesFSDistant = `^(nfs4?|cifs|smb3|smbfs|fuse\.sshfs|9p|ceph|glusterfs)$`
)

// Bornes des réglages.
const (
	intervalleMin   = 10 * time.Second
	intervalleMax   = 5 * time.Minute
	tamponMin       = 1 << 20  // 1 Mio
	tamponMax       = 64 << 20 // 64 Mio
	tamponDefaut    = 8 << 20  // 8 Mio
	longueurRegexMx = 1024
)

// Defaut renvoie une configuration remplie avec les valeurs par défaut.
func Defaut() Agent {
	return Agent{
		Intervalle:            30 * time.Second,
		IntervalleLent:        60 * time.Second,
		TamponMetriques:       tamponDefaut,
		ExclureMontages:       defautMontages,
		ExclureTypesFS:        defautTypesFS,
		ExclureDisques:        defautDisques,
		ExclureInterfaces:     defautInterfaces,
		ExclureTypesFSDistant: defautTypesFSDistant,
		RacineProc:            "/proc",
		RacineSys:             "/sys",
		NiveauLog:             "info",
		Systemd:               true,
		MisesAJour:            true,
		Temperatures:          true,
		DockerMetriques:       true,
		EnvoyerPareFeu:        true,
		EnvoyerSmart:          true,
		EnvoyerCertificats:    true,
		Certificats:           certificatsDefaut(),
		CertificatsDistants:   true,
		FichierRoot:           "/run/mirador-agent/root.json",
		Metriques:             true,
		Journal:               true,
		TamponLogs:            4 << 20,
		LogsDebit:             200,
		LogsRafale:            500,
		LogsBruit:             BruitDefaut,
		Masquage:              []string{"mots_de_passe", "tokens", "cles_api", "autorisation", "cartes"},
		FichierCurseur:        "/var/lib/mirador-agent/journal.cursor",
		LogsDocker:            true,
		FichierToken:          "/etc/mirador-agent/token", // #nosec G101 -- chemin du fichier, pas un secret
		FichierCles:           "/etc/mirador-agent/cles-publiques.txt",
		FichierJeton:          "/var/lib/mirador-agent/jeton",
		FichierConfigDistante: "/var/lib/mirador-agent/config-distante.json",
		FichierActualiser:     "/var/lib/mirador-agent/actualiser-racine",
	}
}

// Noms de machine trop génériques : ils mélangeraient les métriques de
// plusieurs machines sous le même nom (même règle que obs-agent-install.sh).
var nomsGeneriques = map[string]bool{
	"ubuntu": true, "debian": true, "localhost": true, "localhost.localdomain": true,
	"ubuntu-server": true, "debian12": true, "debian13": true, "template": true,
}

var (
	formatNom   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	formatLabel = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,62}$`)
	formatUnite = regexp.MustCompile(`^[A-Za-z0-9:_.@\\-]{1,255}\.service$`)
	// Nom de disque ou d'interface (/proc/diskstats, /proc/net/dev).
	formatElement = regexp.MustCompile(`^[A-Za-z0-9_.:@+-]{1,64}$`)
)

// Valider vérifie chaque champ. Elle renvoie toutes les erreurs d'un coup,
// pour corriger le fichier en une fois.
func (a *Agent) Valider() error {
	var errs []error
	ajoute := func(format string, args ...any) { errs = append(errs, fmt.Errorf(format, args...)) }

	if !formatNom.MatchString(a.Nom) {
		ajoute("MIRADOR_NOM invalide %q : lettres, chiffres, point, tiret et souligné (63 caractères max)", a.Nom)
	} else if nomsGeneriques[strings.ToLower(a.Nom)] {
		ajoute("MIRADOR_NOM %q est trop générique : choisissez le vrai nom de la machine", a.Nom)
	}
	// Avec mirador-server (MIRADOR_SERVEUR), site et env sont facultatifs :
	// ils se choisissent dans l'interface et le serveur les impose.
	facultatif := a.Serveur != ""
	if !(facultatif && a.Site == "") && !formatLabel.MatchString(a.Site) {
		ajoute("MIRADOR_SITE invalide %q", a.Site)
	}
	if !(facultatif && a.Env == "") && !formatLabel.MatchString(a.Env) {
		ajoute("MIRADOR_ENV invalide %q", a.Env)
	}
	if a.Serveur != "" {
		if err := validerURL(a.Serveur); err != nil {
			ajoute("MIRADOR_SERVEUR : %v", err)
		}
	} else if a.MajAuto {
		ajoute("MIRADOR_MAJ_AUTO exige MIRADOR_SERVEUR")
	}
	if err := validerURL(a.URLMetriques); err != nil {
		ajoute("MIRADOR_URL_METRIQUES : %v", err)
	}
	if a.URLLogs != "" {
		if err := validerURL(a.URLLogs); err != nil {
			ajoute("MIRADOR_URL_LOGS : %v", err)
		}
	}
	if !strings.HasPrefix(a.FichierToken, "/") {
		ajoute("MIRADOR_FICHIER_TOKEN doit être un chemin absolu")
	}
	if !strings.HasPrefix(a.FichierCles, "/") {
		ajoute("MIRADOR_FICHIER_CLES doit être un chemin absolu")
	}
	if a.Intervalle < intervalleMin || a.Intervalle > intervalleMax {
		ajoute("MIRADOR_INTERVALLE %s hors bornes (%s à %s)", a.Intervalle, intervalleMin, intervalleMax)
	}
	if a.IntervalleLent < a.Intervalle || a.IntervalleLent > 10*intervalleMax {
		ajoute("MIRADOR_INTERVALLE_LENT %s hors bornes (%s à %s)", a.IntervalleLent, a.Intervalle, 10*intervalleMax)
	}
	if a.TamponMetriques < tamponMin || a.TamponMetriques > tamponMax {
		ajoute("MIRADOR_TAMPON_METRIQUES %d hors bornes (%d à %d octets)", a.TamponMetriques, tamponMin, tamponMax)
	}
	for nom, expr := range map[string]string{
		"MIRADOR_EXCLURE_MONTAGES":         a.ExclureMontages,
		"MIRADOR_EXCLURE_TYPES_FS":         a.ExclureTypesFS,
		"MIRADOR_EXCLURE_DISQUES":          a.ExclureDisques,
		"MIRADOR_EXCLURE_INTERFACES":       a.ExclureInterfaces,
		"MIRADOR_EXCLURE_TYPES_FS_DISTANT": a.ExclureTypesFSDistant,
	} {
		// Le moteur regexp de Go est en temps linéaire : pas de risque
		// d'expression « catastrophique ». On borne quand même la longueur.
		if len(expr) > longueurRegexMx {
			ajoute("%s trop longue (%d caractères max)", nom, longueurRegexMx)
		} else if _, err := regexp.Compile(expr); err != nil {
			ajoute("%s : expression invalide : %v", nom, err)
		}
	}
	if a.Docker != "" {
		if err := validerDocker(a.Docker); err != nil {
			ajoute("MIRADOR_DOCKER : %v", err)
		}
	}
	if len(a.SystemdIgnorer) > 200 {
		ajoute("MIRADOR_SYSTEMD_IGNORER : 200 services au plus")
	}
	for _, n := range a.SystemdIgnorer {
		if !formatUnite.MatchString(n) {
			ajoute("MIRADOR_SYSTEMD_IGNORER : nom de service invalide %q", n)
		}
	}
	if len(a.SystemdSurveiller) > 200 {
		ajoute("MIRADOR_SYSTEMD_SURVEILLER : 200 services au plus")
	}
	for _, n := range a.SystemdSurveiller {
		if !formatUnite.MatchString(n) {
			ajoute("MIRADOR_SYSTEMD_SURVEILLER : nom de service invalide %q", n)
		}
	}
	if len(a.Certificats) > 50 {
		ajoute("MIRADOR_CERTIFICATS : 50 motifs au plus")
	}
	for _, m := range a.Certificats {
		if err := validerMotif(m); err != nil {
			ajoute("MIRADOR_CERTIFICATS : %v", err)
		}
	}
	for nom, l := range map[string][]string{"ignorer_disques": a.IgnorerDisques, "ignorer_interfaces": a.IgnorerInterfaces} {
		if len(l) > MaxIgnores {
			ajoute("%s : %d noms au plus", nom, MaxIgnores)
		}
		for _, n := range l {
			if !formatElement.MatchString(n) {
				ajoute("%s : nom invalide %q", nom, n)
			}
		}
	}
	if len(a.IgnorerMontages) > MaxIgnores {
		ajoute("ignorer_montages : %d montages au plus", MaxIgnores)
	}
	for _, m := range a.IgnorerMontages {
		if err := validerChemin(m); err != nil {
			ajoute("ignorer_montages : %v", err)
		}
	}
	if len(a.CertificatsIgnorer) > 2*MaxIgnores {
		ajoute("certificats_ignorer : %d chemins au plus", 2*MaxIgnores)
	}
	for _, m := range a.CertificatsIgnorer {
		if err := validerChemin(m); err != nil {
			ajoute("certificats_ignorer : %v", err)
		}
	}
	if len(a.CertificatsAjout) > MaxMotifsDistants {
		ajoute("certificats_ajout : %d chemins au plus", MaxMotifsDistants)
	}
	for _, m := range a.CertificatsAjout {
		if err := ValiderMotifDistant(m); err != nil {
			ajoute("certificats_ajout : %v", err)
		}
	}
	if a.TamponLogs < tamponMin || a.TamponLogs > tamponMax {
		ajoute("MIRADOR_TAMPON_LOGS %d hors bornes (%d à %d octets)", a.TamponLogs, tamponMin, tamponMax)
	}
	if a.LogsDebit < 1 || a.LogsDebit > 10000 || a.LogsRafale < a.LogsDebit || a.LogsRafale > 100000 {
		ajoute("MIRADOR_LOGS_DEBIT (1 à 10000) et MIRADOR_LOGS_RAFALE (≥ débit, 100000 max) hors bornes")
	}
	if len(a.LogsBruit) > longueurRegexMx {
		ajoute("MIRADOR_LOGS_BRUIT trop longue")
	} else if _, err := regexp.Compile(a.LogsBruit); err != nil {
		ajoute("MIRADOR_LOGS_BRUIT : %v", err)
	}
	if _, err := logs.NewMasqueur(a.Masquage, a.MasquagePerso); err != nil {
		ajoute("masquage : %v", err)
	}
	if !strings.HasPrefix(a.FichierCurseur, "/") {
		ajoute("MIRADOR_FICHIER_CURSEUR doit être un chemin absolu")
	}
	if !strings.HasPrefix(a.FichierRoot, "/") {
		ajoute("MIRADOR_FICHIER_ROOT doit être un chemin absolu")
	}
	switch a.NiveauLog {
	case "debug", "info", "warn", "error":
	default:
		ajoute("MIRADOR_NIVEAU_LOG %q : debug, info, warn ou error", a.NiveauLog)
	}
	return errors.Join(errs...)
}

// validerURL accepte https, et http seulement vers la machine elle-même
// (tests, ou VictoriaMetrics local).
func validerURL(brute string) error {
	u, err := url.Parse(brute)
	if err != nil || u.Host == "" {
		return fmt.Errorf("adresse invalide %q", brute)
	}
	if u.User != nil {
		return errors.New("pas d'identifiant dans l'adresse : utilisez le fichier de token")
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		h := u.Hostname()
		if h == "127.0.0.1" || h == "::1" || h == "localhost" {
			return nil
		}
		return errors.New("http accepté uniquement vers 127.0.0.1 : utilisez https")
	default:
		return fmt.Errorf("schéma %q refusé (https attendu)", u.Scheme)
	}
}

// BruitDefaut : lignes de journal jetées par défaut (mêmes motifs qu'Alloy,
// SPEC.md § 4.3).
const BruitDefaut = `(?i)(session (opened|closed) for user|pam_unix\(cron|Started Session|Starting Session|Removed slice|Created slice)`

// certificatsDefaut : mêmes chemins qu'obs-certs.sh (SPEC.md § 4.2).
// Volontairement absents : /etc/pve/local/pve-ssl.pem (certificat interne
// entre nœuds) et /etc/pve/pve-root-ca.pem (CA interne).
func certificatsDefaut() []string {
	return []string{
		"/etc/letsencrypt/live/*/cert.pem",
		"/etc/ssl/local/*.crt",
		"/etc/ssl/local/*.pem",
		"/etc/pve/local/pveproxy-ssl.pem",
		"/etc/proxmox-backup/proxy.pem",
	}
}

// validerMotif : chemin absolu, sans « .. », motif glob valide.
func validerMotif(m string) error {
	if !strings.HasPrefix(m, "/") || strings.Contains(m, "..") || len(m) > 512 {
		return fmt.Errorf("motif %q : chemin absolu sans « .. » attendu", m)
	}
	if _, err := filepath.Match(m, "/"); err != nil {
		return fmt.Errorf("motif %q invalide : %v", m, err)
	}
	return nil
}

// Bornes des listes choisies dans l'interface.
const (
	MaxIgnores        = 100 // disques, interfaces, montages (200 certificats)
	MaxMotifsDistants = 20  // chemins de certificats ajoutés depuis l'interface
)

// validerChemin : chemin absolu exact (montage, certificat), sans « .. » ni
// caractère de contrôle.
func validerChemin(c string) error {
	if !strings.HasPrefix(c, "/") || len(c) > 512 || strings.Contains(c, "/../") || strings.HasSuffix(c, "/..") {
		return fmt.Errorf("chemin %q : chemin absolu sans « .. » attendu", c)
	}
	for _, r := range c {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("chemin %q : caractère de contrôle", c)
		}
	}
	return nil
}

// Dossiers où l'assistant root accepte de chercher un certificat demandé par
// le serveur (SPEC.md § 21.3). Pas /proc, /sys, /dev, /run, /tmp, /home,
// /root : rien à y chercher, et l'unité root masque déjà /home et /root.
// /usr/share/ca-certificates/ (ajout du 12/11, SPEC § 22.4) : certificats
// publics des autorités, cibles des liens de /etc/ssl/certs sur Debian.
var prefixesDistants = []string{"/etc/", "/opt/", "/srv/", "/var/lib/", "/usr/local/", "/usr/share/ca-certificates/"}

// Extensions acceptées pour un chemin demandé par le serveur.
var extensionsDistantes = []string{".pem", ".crt", ".cer", ".cert"}

// ValiderMotifDistant : règles d'un chemin de certificat venu du serveur,
// plus strictes que celles du fichier local, car c'est root qui lit le
// fichier. Vérifié par l'agent à la réception ET par l'assistant root avant
// chaque lecture.
//   - chemin absolu sous un dossier autorisé, sans « .. » ;
//   - nom de fichier en .pem, .crt, .cer ou .cert (jamais « * » seul) ;
//   - pas de clé privée visée (« key » ou « priv » dans le nom) ;
//   - 3 jokers au plus (*, ?, [).
func ValiderMotifDistant(m string) error {
	if err := validerMotif(m); err != nil {
		return err
	}
	if err := validerChemin(m); err != nil {
		return err
	}
	if strings.Contains(m, "//") || strings.Contains(m, "/./") {
		return fmt.Errorf("motif %q : chemin non normalisé", m)
	}
	bon := false
	for _, p := range prefixesDistants {
		if strings.HasPrefix(m, p) {
			bon = true
		}
	}
	if !bon {
		return fmt.Errorf("motif %q : dossier non autorisé (%s)", m, strings.Join(prefixesDistants, ", "))
	}
	nom := strings.ToLower(m[strings.LastIndexByte(m, '/')+1:])
	bon = false
	for _, e := range extensionsDistantes {
		if strings.HasSuffix(nom, e) {
			bon = true
		}
	}
	if !bon { // l'extension est écrite en toutes lettres : pas de joker possible dedans
		return fmt.Errorf("motif %q : le nom doit finir par .pem, .crt, .cer ou .cert", m)
	}
	if strings.Contains(nom, "key") || strings.Contains(nom, "priv") {
		return fmt.Errorf("motif %q : ressemble à une clé privée", m)
	}
	if strings.Count(m, "*")+strings.Count(m, "?")+strings.Count(m, "[") > 3 {
		return fmt.Errorf("motif %q : 3 jokers au plus", m)
	}
	return nil
}

// DossierDistantAutorise : le fichier réel (liens suivis) est-il dans un
// dossier où l'assistant root accepte de lire pour le serveur ?
func DossierDistantAutorise(c string) bool {
	for _, p := range prefixesDistants {
		if strings.HasPrefix(c, p) {
			return true
		}
	}
	return false
}

// FichierAccepteDistant : contrôle d'un fichier trouvé par un motif distant
// (le joker a pu donner un nom de clé privée, ex. « *.pem » → privkey.pem).
func FichierAccepteDistant(chemin string) bool {
	nom := strings.ToLower(chemin[strings.LastIndexByte(chemin, '/')+1:])
	return !strings.Contains(nom, "key") && !strings.Contains(nom, "priv")
}

// validerDocker n'accepte que le proxy local : http vers la machine elle-même
// ou socket Unix. Jamais d'adresse distante (le proxy n'est pas chiffré).
func validerDocker(brute string) error {
	u, err := url.Parse(brute)
	if err != nil {
		return fmt.Errorf("adresse invalide %q", brute)
	}
	switch u.Scheme {
	case "http":
		h := u.Hostname()
		if (h != "127.0.0.1" && h != "::1" && h != "localhost") || u.Port() == "" || u.User != nil ||
			(u.Path != "" && u.Path != "/") {
			return errors.New("http://127.0.0.1:PORT attendu (proxy local)")
		}
		return nil
	case "unix":
		if u.Host != "" || !strings.HasPrefix(u.Path, "/") {
			return errors.New("unix:///chemin/absolu attendu")
		}
		return nil
	}
	return fmt.Errorf("schéma %q refusé (http ou unix)", u.Scheme)
}

// LireFichier lit le fichier local CLE=valeur et renvoie une configuration
// validée (valeurs par défaut pour les clés absentes).
func LireFichier(chemin string) (Agent, error) {
	f, err := os.Open(chemin) // #nosec G304 -- chemin donné par l'administrateur (option -config)
	if err != nil {
		return Agent{}, err
	}
	defer f.Close()
	return Lire(f)
}

// Lire analyse le contenu CLE=valeur.
//
// Format : une clé par ligne, « # » pour les commentaires, guillemets
// facultatifs autour de la valeur. Une clé inconnue est une erreur.
func Lire(r io.Reader) (Agent, error) {
	a := Defaut()
	sc := bufio.NewScanner(r)
	numero := 0
	vues := map[string]bool{}
	for sc.Scan() {
		numero++
		ligne := strings.TrimSpace(sc.Text())
		if ligne == "" || strings.HasPrefix(ligne, "#") {
			continue
		}
		cle, valeur, ok := strings.Cut(ligne, "=")
		if !ok {
			return Agent{}, fmt.Errorf("ligne %d : « CLE=valeur » attendu", numero)
		}
		cle = strings.TrimSpace(cle)
		valeur = strings.Trim(strings.TrimSpace(valeur), `"'`)
		if vues[cle] {
			return Agent{}, fmt.Errorf("ligne %d : %s en double", numero, cle)
		}
		vues[cle] = true
		if err := a.affecter(cle, valeur); err != nil {
			return Agent{}, fmt.Errorf("ligne %d : %w", numero, err)
		}
	}
	if err := sc.Err(); err != nil {
		return Agent{}, err
	}
	// Avec mirador-server, les adresses découlent de MIRADOR_SERVEUR.
	if a.Serveur != "" {
		if a.URLMetriques == "" {
			a.URLMetriques = a.Serveur + protocole.CheminMetriques
		}
		if a.URLLogs == "" {
			a.URLLogs = a.Serveur + protocole.CheminLogs
		}
	}
	if err := a.Valider(); err != nil {
		return Agent{}, err
	}
	return a, nil
}

// affecter range une valeur dans le bon champ.
func (a *Agent) affecter(cle, valeur string) error {
	var err error
	switch cle {
	case "MIRADOR_NOM":
		a.Nom = valeur
	case "MIRADOR_SITE":
		a.Site = valeur
	case "MIRADOR_ENV":
		a.Env = valeur
	case "MIRADOR_URL_METRIQUES":
		a.URLMetriques = valeur
	case "MIRADOR_URL_LOGS":
		a.URLLogs = valeur
	case "MIRADOR_FICHIER_TOKEN":
		a.FichierToken = valeur
	case "MIRADOR_FICHIER_CLES":
		a.FichierCles = valeur
	case "MIRADOR_SERVEUR":
		a.Serveur = strings.TrimRight(valeur, "/")
	case "MIRADOR_FICHIER_JETON":
		a.FichierJeton = valeur
	case "MIRADOR_MAJ_AUTO":
		a.MajAuto, err = ouiNon(valeur)
	case "MIRADOR_FICHIER_CONFIG_DISTANTE":
		a.FichierConfigDistante = valeur
	case "MIRADOR_FICHIER_ACTUALISER":
		a.FichierActualiser = valeur
	case "MIRADOR_INTERVALLE":
		a.Intervalle, err = time.ParseDuration(valeur)
	case "MIRADOR_INTERVALLE_LENT":
		a.IntervalleLent, err = time.ParseDuration(valeur)
	case "MIRADOR_TAMPON_METRIQUES":
		a.TamponMetriques, err = strconv.Atoi(valeur)
	case "MIRADOR_EXCLURE_MONTAGES":
		a.ExclureMontages = valeur
	case "MIRADOR_EXCLURE_TYPES_FS":
		a.ExclureTypesFS = valeur
	case "MIRADOR_EXCLURE_DISQUES":
		a.ExclureDisques = valeur
	case "MIRADOR_EXCLURE_INTERFACES":
		a.ExclureInterfaces = valeur
	case "MIRADOR_EXCLURE_TYPES_FS_DISTANT":
		a.ExclureTypesFSDistant = valeur
	case "MIRADOR_NIVEAU_LOG":
		a.NiveauLog = valeur
	case "MIRADOR_SYSTEMD":
		a.Systemd, err = ouiNon(valeur)
	case "MIRADOR_SYSTEMD_IGNORER":
		a.SystemdIgnorer = liste(valeur)
	case "MIRADOR_MISES_A_JOUR":
		a.MisesAJour, err = ouiNon(valeur)
	case "MIRADOR_TEMPERATURES":
		a.Temperatures, err = ouiNon(valeur)
	case "MIRADOR_DOCKER_METRIQUES":
		a.DockerMetriques, err = ouiNon(valeur)
	case "MIRADOR_VENTILATEURS":
		a.Ventilateurs, err = ouiNon(valeur)
	case "MIRADOR_ZFS":
		a.ZFS, err = ouiNon(valeur)
	case "MIRADOR_MDRAID":
		a.Mdraid, err = ouiNon(valeur)
	case "MIRADOR_SYSTEMD_VALIDATION":
		a.SystemdValidation, err = ouiNon(valeur)
	case "MIRADOR_SYSTEMD_SURVEILLER":
		a.SystemdSurveiller = liste(valeur)
	case "MIRADOR_ENVOYER_PAREFEU":
		a.EnvoyerPareFeu, err = ouiNon(valeur)
	case "MIRADOR_ENVOYER_SMART":
		a.EnvoyerSmart, err = ouiNon(valeur)
	case "MIRADOR_ENVOYER_CERTIFICATS":
		a.EnvoyerCertificats, err = ouiNon(valeur)
	case "MIRADOR_DOCKER":
		a.Docker = valeur
	case "MIRADOR_PAREFEU":
		a.PareFeu, err = ouiNon(valeur)
	case "MIRADOR_SMART":
		a.Smart, err = ouiNon(valeur)
	case "MIRADOR_CERTIFICATS":
		a.Certificats = liste(valeur)
	case "MIRADOR_CERTIFICATS_DISTANTS":
		a.CertificatsDistants, err = ouiNon(valeur)
	case "MIRADOR_IGNORER_DISQUES":
		a.IgnorerDisques = liste(valeur)
	case "MIRADOR_IGNORER_INTERFACES":
		a.IgnorerInterfaces = liste(valeur)
	case "MIRADOR_IGNORER_MONTAGES":
		a.IgnorerMontages = liste(valeur)
	case "MIRADOR_FICHIER_ROOT":
		a.FichierRoot = valeur
	case "MIRADOR_METRIQUES":
		a.Metriques, err = ouiNon(valeur)
	case "MIRADOR_JOURNAL":
		a.Journal, err = ouiNon(valeur)
	case "MIRADOR_TAMPON_LOGS":
		a.TamponLogs, err = strconv.Atoi(valeur)
	case "MIRADOR_LOGS_DEBIT":
		a.LogsDebit, err = strconv.ParseFloat(valeur, 64)
	case "MIRADOR_LOGS_RAFALE":
		a.LogsRafale, err = strconv.ParseFloat(valeur, 64)
	case "MIRADOR_LOGS_BRUIT":
		a.LogsBruit = valeur
	case "MIRADOR_MASQUAGE":
		a.Masquage = liste(valeur)
	case "MIRADOR_FICHIER_CURSEUR":
		a.FichierCurseur = valeur
	case "MIRADOR_LOGS_DOCKER":
		a.LogsDocker, err = ouiNon(valeur)
	default:
		// Expressions de masquage personnalisées : une par clé
		// (MIRADOR_MASQUAGE_PERSO_1 à _10), car une expression peut contenir
		// des virgules.
		if n, ok := strings.CutPrefix(cle, "MIRADOR_MASQUAGE_PERSO_"); ok {
			if i, e := strconv.Atoi(n); e == nil && i >= 1 && i <= logs.MaxPerso {
				a.MasquagePerso = append(a.MasquagePerso, valeur)
				return nil
			}
		}
		return fmt.Errorf("clé inconnue %q", cle)
	}
	if err != nil {
		return fmt.Errorf("%s : %w", cle, err)
	}
	return nil
}

// ouiNon lit un booléen écrit en français ou en anglais.
func ouiNon(v string) (bool, error) {
	switch strings.ToLower(v) {
	case "oui", "yes", "true", "1":
		return true, nil
	case "non", "no", "false", "0":
		return false, nil
	}
	return false, fmt.Errorf("%q : oui ou non attendu", v)
}

// liste découpe « a, b, c » (vide = liste vide).
func liste(v string) []string {
	var out []string
	for _, e := range strings.Split(v, ",") {
		if e = strings.TrimSpace(e); e != "" {
			out = append(out, e)
		}
	}
	return out
}

// LireToken lit le token dans son fichier et refuse un fichier lisible par
// tout le monde (le token donne le droit d'écrire des données).
func LireToken(chemin string) (string, error) {
	info, err := os.Stat(chemin)
	if err != nil {
		return "", err
	}
	if info.Mode().Perm()&0o004 != 0 {
		return "", fmt.Errorf("%s est lisible par tous : chmod 0640 ou 0600", chemin)
	}
	brut, err := os.ReadFile(chemin) // #nosec G304 -- chemin issu de la configuration validée
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(brut))
	if len(token) < 16 || strings.ContainsAny(token, " \t\r\n") {
		return "", fmt.Errorf("%s : token absent, trop court ou mal formé", chemin)
	}
	return token, nil
}

// Duree : durée écrite en texte dans le JSON (« 30s », « 5m »), pour la
// configuration distante.
type Duree time.Duration

// UnmarshalJSON lit « "30s" ». Un nombre seul est refusé (unité obligatoire).
func (d *Duree) UnmarshalJSON(b []byte) error {
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return fmt.Errorf("durée : texte attendu (ex. \"30s\")")
	}
	v, err := time.ParseDuration(s)
	if err != nil {
		return err
	}
	*d = Duree(v)
	return nil
}

// MarshalJSON écrit la durée en texte (« 1m0s »), relisible par UnmarshalJSON.
func (d Duree) MarshalJSON() ([]byte, error) { return json.Marshal(time.Duration(d).String()) }
