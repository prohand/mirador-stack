// SPDX-License-Identifier: GPL-3.0-or-later

package majserveur

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/release"
)

// Côté VPS : mirador-maj-serveur (root, lancé par systemd quand une demande
// apparaît). Déroulé de Appliquer :
//
//  1. lire la demande puis l'effacer (une demande = un essai) ;
//  2. refuser : version mal formée, pas plus récente que celle installée ;
//  3. télécharger le manifeste de l'image sur GitHub et vérifier sa
//     signature (clé publique posée sur le VPS) ;
//  4. télécharger l'image PAR SON EMPREINTE signée (docker pull image@sha256:…) ;
//  5. arrêter le serveur, copier sa base (sauvegarde), écrire la nouvelle
//     image dans le .env du compose, relancer ;
//  6. attendre que le serveur dise « je tourne en version X » (3 min) ;
//  7. sinon : retour à l'ancienne image ET à la copie de la base (la nouvelle
//     version a pu la migrer), relance, état « échec ».
//
// Seules commandes lancées : docker pull / compose stop / compose up -d /
// cp, avec des arguments fixés ici ; la demande ne fournit que la version.

// Reglages : /etc/mirador-maj-serveur.conf (CLE=valeur).
type Reglages struct {
	DossierCompose     string        // dossier du docker-compose.yml du serveur
	Service            string        // service du compose
	Conteneur          string        // nom du conteneur (docker cp)
	Image              string        // ghcr.io/prohand/mirador-server
	Depot              string        // dépôt GitHub des Releases
	ClesPubliques      []string      // clés publiques Ed25519 (base64), comme pour les agents
	DossierDemande     string        // demande.json et serveur.json (écrits par le serveur)
	DossierEtat        string        // etat.json (écrit ici, lecture seule pour le serveur)
	DossierSauvegardes string        // copies de la base, version installée, verrou
	FichierBase        string        // base du serveur, dans le conteneur
	Delai              time.Duration // attente du démarrage de la nouvelle version
}

// ReglagesDefaut : valeurs par défaut (compose de deploy/serveur).
func ReglagesDefaut() Reglages {
	return Reglages{
		DossierCompose:     "/opt/mirador",
		Service:            "mirador-server",
		Conteneur:          "mirador-server",
		Image:              "ghcr.io/prohand/mirador-server",
		Depot:              "prohand/mirador-stack",
		DossierSauvegardes: "/var/lib/mirador-maj-serveur",
		FichierBase:        "/var/lib/mirador/mirador.db",
		Delai:              3 * time.Minute,
	}
}

var (
	formatNom   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,62}$`)
	formatImage = regexp.MustCompile(`^[a-z0-9.-]+(:[0-9]+)?(/[a-z0-9._-]+)+$`)
	formatDepot = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,38}/[A-Za-z0-9._-]{1,100}$`)
)

// LireReglages : fichier CLE=valeur ; clé inconnue refusée.
func LireReglages(r io.Reader) (Reglages, error) {
	g := ReglagesDefaut()
	sc := bufio.NewScanner(r)
	n := 0
	for sc.Scan() {
		n++
		l := strings.TrimSpace(sc.Text())
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		cle, val, ok := strings.Cut(l, "=")
		if !ok {
			return g, fmt.Errorf("ligne %d : « CLE=valeur » attendu", n)
		}
		cle, val = strings.TrimSpace(cle), strings.Trim(strings.TrimSpace(val), `"'`)
		switch cle {
		case "DOSSIER_COMPOSE":
			g.DossierCompose = val
		case "SERVICE":
			g.Service = val
		case "CONTENEUR":
			g.Conteneur = val
		case "IMAGE":
			g.Image = val
		case "DEPOT":
			g.Depot = val
		case "CLES_PUBLIQUES":
			g.ClesPubliques = nil
			for _, k := range strings.Split(val, ",") {
				if k = strings.TrimSpace(k); k != "" {
					g.ClesPubliques = append(g.ClesPubliques, k)
				}
			}
		case "DOSSIER_DEMANDE":
			g.DossierDemande = val
		case "DOSSIER_ETAT":
			g.DossierEtat = val
		case "DOSSIER_SAUVEGARDES":
			g.DossierSauvegardes = val
		case "FICHIER_BASE":
			g.FichierBase = val
		case "DELAI":
			d, err := time.ParseDuration(val)
			if err != nil || d < 30*time.Second || d > 30*time.Minute {
				return g, fmt.Errorf("ligne %d : DELAI entre 30s et 30m", n)
			}
			g.Delai = d
		default:
			return g, fmt.Errorf("ligne %d : clé inconnue %q", n, cle)
		}
	}
	if err := sc.Err(); err != nil {
		return g, err
	}
	if g.DossierDemande == "" {
		g.DossierDemande = filepath.Join(g.DossierCompose, "maj")
	}
	if g.DossierEtat == "" {
		g.DossierEtat = filepath.Join(g.DossierCompose, "maj-etat")
	}
	return g, g.Valider()
}

// Valider : chemins absolus, noms et image bien formés, au moins une clé.
func (g Reglages) Valider() error {
	var errs []error
	for nom, ch := range map[string]string{"DOSSIER_COMPOSE": g.DossierCompose, "DOSSIER_DEMANDE": g.DossierDemande,
		"DOSSIER_ETAT": g.DossierEtat, "DOSSIER_SAUVEGARDES": g.DossierSauvegardes, "FICHIER_BASE": g.FichierBase} {
		if !filepath.IsAbs(ch) || filepath.Clean(ch) != ch {
			errs = append(errs, fmt.Errorf("%s : chemin absolu attendu (%q)", nom, ch))
		}
	}
	if !formatNom.MatchString(g.Service) || !formatNom.MatchString(g.Conteneur) {
		errs = append(errs, errors.New("SERVICE / CONTENEUR invalides"))
	}
	if !formatImage.MatchString(g.Image) {
		errs = append(errs, fmt.Errorf("IMAGE invalide (%q, sans étiquette ni empreinte)", g.Image))
	}
	if !formatDepot.MatchString(g.Depot) {
		errs = append(errs, fmt.Errorf("DEPOT invalide (%q)", g.Depot))
	}
	if len(g.ClesPubliques) == 0 {
		errs = append(errs, errors.New("CLES_PUBLIQUES : au moins une clé publique (la même que pour les agents)"))
	}
	return errors.Join(errs...)
}

// Docker lance une commande docker (arguments fixés par l'appelant) dans le
// dossier du compose ; renvoie la sortie (pour le journal).
type Docker interface {
	Lancer(ctx context.Context, args ...string) (string, error)
}

// Hote : le service du VPS.
type Hote struct {
	R           Reglages
	Docker      Docker
	Telecharger func(ctx context.Context, adresse string, max int64) ([]byte, error) // défaut : HTTPS GitHub
	Maintenant  func() time.Time
	Dormir      func(time.Duration)
	Journal     io.Writer

	etat Etat
}

const (
	fichierVersion = "version-installee" // dans DossierSauvegardes
	fichierEnv     = ".env"              // dans DossierCompose
	gardeCopies    = 3                   // copies de la base gardées
)

func (h *Hote) maintenant() time.Time {
	if h.Maintenant != nil {
		return h.Maintenant()
	}
	return time.Now()
}

func (h *Hote) noter(format string, a ...any) {
	t := fmt.Sprintf(format, a...)
	if h.Journal != nil {
		fmt.Fprintln(h.Journal, t)
	}
	h.etat.Etapes = append(h.etat.Etapes, Etape{Le: h.maintenant().Unix(), Texte: t})
	h.ecrireEtat()
}

func (h *Hote) ecrireEtat() {
	h.etat.Le = h.maintenant().Unix()
	if err := EcrireJSON(h.R.DossierEtat, NomEtat, h.etat, 0o644); err != nil && h.Journal != nil { // #nosec G306 -- lu par le serveur (autre utilisateur)
		fmt.Fprintln(h.Journal, "état non écrit :", err)
	}
}

func (h *Hote) finir(etat, message string) error {
	h.etat.Etat, h.etat.Message = etat, message
	h.noter("%s", message)
	if etat == EtatOK {
		return nil
	}
	return errors.New(message)
}

// VersionInstallee : version notée par ce service après la dernière mise à
// jour réussie ; à défaut, MIRADOR_VERSION du .env (posé à l'installation).
// Jamais celle que dit le serveur : un serveur piraté ne doit pas pouvoir
// faire croire à une version plus récente pour obtenir un retour arrière.
func (h *Hote) VersionInstallee() (string, error) {
	if b, err := os.ReadFile(filepath.Join(h.R.DossierSauvegardes, fichierVersion)); err == nil { // #nosec G304 -- dossier du service (root)
		v := strings.TrimSpace(string(b))
		return v, ValiderVersion(v)
	}
	env, err := lireEnv(filepath.Join(h.R.DossierCompose, fichierEnv))
	if err != nil {
		return "", err
	}
	v := valeurEnv(env, "MIRADOR_VERSION")
	if v == "" {
		return "", errors.New("version installée inconnue : MIRADOR_VERSION absent du .env du compose")
	}
	return v, ValiderVersion(v)
}

// EcrireInfo écrit les réglages du service et les problèmes trouvés dans
// le dossier de l'état (affichés par l'interface).
func (h *Hote) EcrireInfo(versionService string, problemes []string) error {
	i := InfoService{Version: versionService, DossierCompose: h.R.DossierCompose, Service: h.R.Service, Image: h.R.Image,
		Depot: h.R.Depot, Delai: h.R.Delai.String(), Problemes: problemes, Le: h.maintenant().Unix()}
	if h.R.Delai%time.Minute == 0 {
		i.Delai = fmt.Sprintf("%d min", h.R.Delai/time.Minute)
	}
	for _, k := range h.R.ClesPubliques {
		i.Cles = append(i.Cles, release.EmpreintePublique(k))
	}
	i.Installee, _ = h.VersionInstallee()
	if i.Problemes == nil {
		i.Problemes = []string{}
	}
	return EcrireJSON(h.R.DossierEtat, NomService, i, 0o644) // #nosec G306 -- lu par le serveur (autre utilisateur)
}

// Appliquer traite la demande en attente (aucune : rien à faire).
func (h *Hote) Appliquer(ctx context.Context) error {
	var d Demande
	chemin := filepath.Join(h.R.DossierDemande, NomDemande)
	err := LireJSON(chemin, &d)
	if errors.Is(err, ErrAbsent) {
		return nil
	}
	_ = os.Remove(chemin) // une demande = un essai (même illisible)
	debut := h.maintenant()
	h.etat = Etat{Etat: EtatEnCours, Version: d.Version, Debut: debut.Unix()}
	if err != nil {
		return h.finir(EtatRefuse, "demande refusée : "+err.Error())
	}
	h.noter("demande de la version %s par %s", d.Version, nettoyer(d.Par))
	if err := ValiderVersion(d.Version); err != nil {
		return h.finir(EtatRefuse, "demande refusée : "+err.Error())
	}
	avant, err := h.VersionInstallee()
	if err != nil {
		return h.finir(EtatRefuse, "demande refusée : "+err.Error())
	}
	h.etat.Avant = avant
	if Comparer(d.Version, avant) <= 0 {
		return h.finir(EtatRefuse, fmt.Sprintf("demande refusée : %s n'est pas plus récente que %s (installée)", d.Version, avant))
	}
	// 3. Manifeste signé de l'image.
	empreinte, err := h.empreinteSignee(ctx, d.Version)
	if err != nil {
		return h.finir(EtatRefuse, "image refusée : "+err.Error())
	}
	ref := h.R.Image + "@" + empreinte
	h.noter("signature vérifiée : %s", ref)
	// 4. Téléchargement par l'empreinte.
	if _, err := h.Docker.Lancer(ctx, "pull", ref); err != nil {
		return h.finir(EtatRefuse, "téléchargement de l'image impossible : "+err.Error())
	}
	h.noter("image téléchargée")
	// 5. Arrêt, copie de la base, nouvelle image, relance.
	cheminEnv := filepath.Join(h.R.DossierCompose, fichierEnv)
	envAvant, err := lireEnv(cheminEnv)
	if err != nil {
		return h.finir(EtatRefuse, "lecture du .env impossible : "+err.Error())
	}
	if _, err := h.Docker.Lancer(ctx, "compose", "stop", h.R.Service); err != nil {
		return h.finir(EtatRefuse, "arrêt du serveur impossible : "+err.Error())
	}
	h.noter("serveur arrêté")
	copie, err := h.copierBase(ctx, avant, debut)
	if err != nil {
		// Rien n'a changé : on relance tel quel.
		_, _ = h.Docker.Lancer(ctx, "compose", "up", "-d", h.R.Service)
		return h.finir(EtatEchec, "copie de la base impossible, serveur relancé sans changement : "+err.Error())
	}
	h.noter("base copiée : %s", copie)
	envApres := avecValeur(avecValeur(envAvant, "MIRADOR_IMAGE", ref), "MIRADOR_VERSION", d.Version)
	if err := ecrireEnv(cheminEnv, envApres); err != nil {
		_, _ = h.Docker.Lancer(ctx, "compose", "up", "-d", h.R.Service)
		return h.finir(EtatEchec, "écriture du .env impossible, serveur relancé sans changement : "+err.Error())
	}
	if _, err := h.Docker.Lancer(ctx, "compose", "up", "-d", h.R.Service); err == nil && h.attendre(ctx, d.Version, debut) {
		_ = EcrireFichier(h.R.DossierSauvegardes, fichierVersion, []byte(d.Version+"\n"), 0o600)
		h.ranger()
		return h.finir(EtatOK, fmt.Sprintf("version %s en service (avant : %s)", d.Version, avant))
	}
	// 7. Retour arrière.
	h.noter("la version %s n'a pas démarré en %s : retour à %s", d.Version, h.R.Delai, avant)
	_, _ = h.Docker.Lancer(ctx, "compose", "stop", h.R.Service)
	if err := h.remettreBase(ctx, copie); err != nil {
		h.noter("base NON remise (%v) : la remettre à la main depuis %s", err, copie)
	} else {
		h.noter("base d'avant remise")
	}
	if err := ecrireEnv(cheminEnv, envAvant); err != nil {
		return h.finir(EtatEchec, "ÉCHEC et .env d'avant non remis ("+err.Error()+") : intervention à la main nécessaire")
	}
	if _, err := h.Docker.Lancer(ctx, "compose", "up", "-d", h.R.Service); err != nil || !h.attendre(ctx, avant, debut) {
		return h.finir(EtatEchec, fmt.Sprintf("la version %s n'a pas démarré ; l'ancienne (%s) non plus : intervention à la main nécessaire", d.Version, avant))
	}
	return h.finir(EtatEchec, fmt.Sprintf("la version %s n'a pas démarré : retour à %s fait (base remise)", d.Version, avant))
}

// nettoyer : nom affiché (vient du serveur) : une ligne, 64 caractères.
func nettoyer(s string) string {
	s = strings.Map(func(r rune) rune {
		if r < 32 || r == 127 {
			return -1
		}
		return r
	}, s)
	if r := []rune(s); len(r) > 64 {
		s = string(r[:64])
	}
	if s == "" {
		return "?"
	}
	return s
}

func (h *Hote) empreinteSignee(ctx context.Context, version string) (string, error) {
	tel := h.Telecharger
	if tel == nil {
		tel = TelechargerGitHub
	}
	adresse := "https://github.com/" + h.R.Depot + "/releases/download/v" + version + "/mirador-server-image-" + version + ".manifeste.json"
	brut, err := tel(ctx, adresse, 8<<10)
	if err != nil {
		return "", fmt.Errorf("manifeste introuvable (%v)", err)
	}
	var m protocole.Manifeste
	dec := json.NewDecoder(strings.NewReader(string(brut)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&m); err != nil {
		return "", errors.New("manifeste illisible")
	}
	return release.VerifierImage(&m, version, h.R.ClesPubliques)
}

// copierBase : copie la base du conteneur arrêté (fichier principal exigé,
// -wal et -shm s'ils existent) dans DossierSauvegardes/avant-VERSION-DATE.
func (h *Hote) copierBase(ctx context.Context, avant string, t time.Time) (string, error) {
	dest := filepath.Join(h.R.DossierSauvegardes, "avant-"+avant+"-"+t.UTC().Format("20060102-150405"))
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return "", err
	}
	nom := filepath.Base(h.R.FichierBase)
	if _, err := h.Docker.Lancer(ctx, "cp", h.R.Conteneur+":"+h.R.FichierBase, filepath.Join(dest, nom)); err != nil {
		return "", err
	}
	for _, suf := range []string{"-wal", "-shm"} {
		_, _ = h.Docker.Lancer(ctx, "cp", h.R.Conteneur+":"+h.R.FichierBase+suf, filepath.Join(dest, nom+suf))
	}
	return dest, nil
}

// remettreBase : remet la copie dans le conteneur arrêté. Le journal WAL est
// remis tel qu'il était (vide s'il n'existait pas) : celui de la nouvelle
// version ne doit pas s'appliquer à l'ancienne base.
func (h *Hote) remettreBase(ctx context.Context, copie string) error {
	nom := filepath.Base(h.R.FichierBase)
	for _, suf := range []string{"-wal", "-shm"} {
		f := filepath.Join(copie, nom+suf)
		if _, err := os.Stat(f); errors.Is(err, os.ErrNotExist) {
			if err := os.WriteFile(f, nil, 0o600); err != nil {
				return err
			}
		}
	}
	for _, suf := range []string{"", "-wal", "-shm"} {
		if _, err := h.Docker.Lancer(ctx, "cp", filepath.Join(copie, nom+suf), h.R.Conteneur+":"+h.R.FichierBase+suf); err != nil {
			return err
		}
	}
	return nil
}

// attendre : le serveur a-t-il écrit « version X, démarré après debut » ?
func (h *Hote) attendre(ctx context.Context, version string, debut time.Time) bool {
	dormir := h.Dormir
	if dormir == nil {
		dormir = time.Sleep
	}
	fin := h.maintenant().Add(h.R.Delai)
	for {
		var p Presence
		if LireJSON(filepath.Join(h.R.DossierDemande, NomPresence), &p) == nil && p.Version == version && p.DemarreLe >= debut.Unix() {
			return true
		}
		if ctx.Err() != nil || !h.maintenant().Before(fin) {
			return false
		}
		dormir(3 * time.Second)
	}
}

// ranger : garde les gardeCopies copies de la base les plus récentes.
func (h *Hote) ranger() {
	es, err := os.ReadDir(h.R.DossierSauvegardes)
	if err != nil {
		return
	}
	var copies []string
	for _, e := range es {
		if e.IsDir() && strings.HasPrefix(e.Name(), "avant-") {
			copies = append(copies, e.Name())
		}
	}
	sort.Slice(copies, func(i, j int) bool { return copies[i][len(copies[i])-15:] < copies[j][len(copies[j])-15:] })
	for len(copies) > gardeCopies {
		_ = os.RemoveAll(filepath.Join(h.R.DossierSauvegardes, copies[0]))
		copies = copies[1:]
	}
}

// --- .env du compose ---------------------------------------------------------------

// lireEnv : lignes du .env (gardées telles quelles, commentaires compris).
func lireEnv(chemin string) ([]string, error) {
	b, err := os.ReadFile(chemin) // #nosec G304 -- dossier du compose (configuration root)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return strings.Split(strings.TrimRight(string(b), "\n"), "\n"), nil
}

func valeurEnv(lignes []string, cle string) string {
	for _, l := range lignes {
		if k, v, ok := strings.Cut(strings.TrimSpace(l), "="); ok && strings.TrimSpace(k) == cle {
			return strings.Trim(strings.TrimSpace(v), `"'`)
		}
	}
	return ""
}

// avecValeur : remplace (ou ajoute) CLE=valeur, sans toucher au reste.
func avecValeur(lignes []string, cle, val string) []string {
	out := make([]string, 0, len(lignes)+1)
	vu := false
	for _, l := range lignes {
		if k, _, ok := strings.Cut(strings.TrimSpace(l), "="); ok && strings.TrimSpace(k) == cle {
			if !vu {
				out = append(out, cle+"="+val)
				vu = true
			}
			continue
		}
		out = append(out, l)
	}
	if !vu {
		out = append(out, cle+"="+val)
	}
	return out
}

// ecrireEnv : écrit le .env d'un coup (droits 0600 : il peut contenir la clé
// d'Olvid).
func ecrireEnv(chemin string, lignes []string) error {
	return EcrireFichier(filepath.Dir(chemin), filepath.Base(chemin), []byte(strings.Join(lignes, "\n")+"\n"), 0o600)
}

// --- GitHub ------------------------------------------------------------------------

// TelechargerGitHub : HTTPS, redirections vers GitHub seulement, taille bornée.
func TelechargerGitHub(ctx context.Context, adresse string, max int64) ([]byte, error) {
	client := &http.Client{Timeout: time.Minute, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		hote := r.URL.Hostname()
		if len(via) > 3 || r.URL.Scheme != "https" || (hote != "github.com" && !strings.HasSuffix(hote, ".githubusercontent.com")) {
			return errors.New("redirection refusée : " + r.URL.Redacted())
		}
		return nil
	}}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, adresse, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "mirador-maj-serveur")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("réponse %d", resp.StatusCode)
	}
	b, err := io.ReadAll(io.LimitReader(resp.Body, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, errors.New("fichier trop gros")
	}
	return b, nil
}
