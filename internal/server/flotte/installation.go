// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	installagent "mirador-stack/deploy/agent"
	installcollecteur "mirador-stack/deploy/collecteur"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/web"
)

// Installation par téléchargement (demandes du 11/11, SPEC § 18.19) : la
// machine récupère le script (public) puis le binaire de l'agent en HTTPS
// auprès de mirador-server ; le binaire exige le jeton d'enrôlement (en-tête
// Authorization envoyé par le script, jamais dans l'adresse). Sans jeton
// valable (inconnu, expiré, déjà consommé) : refus, et l'adresse est comptée
// comme suspecte (même limite que l'enrôlement).
//
// Binaire servi : la version globale de l'agent si elle est publiée (signée,
// vérifiée à la publication), sinon celui embarqué dans l'image du serveur
// (construit avec lui par la CI).
//
// Manifeste signé (revue de sécurité du 28/11, M2) : envoyé avec le binaire,
// dans l'en-tête X-Mirador-Manifeste (JSON en base64), dans la MÊME réponse
// (même jeton, et jamais le manifeste d'un autre binaire). Le script vérifie
// la signature Ed25519 avec la clé publique qu'il connaît déjà, AVANT de
// lancer le binaire : un serveur piraté ne peut plus faire installer un
// binaire non signé. Sans manifeste (binaire embarqué qui ne correspond à
// aucune version publiée) : « X-Mirador-Signe: non ».

// DossierAgentEmbarque : binaires livrés dans l'image du serveur
// (mirador-agent-linux-amd64, …-arm64, mirador-collector-linux-amd64, …).
const DossierAgentEmbarque = "/usr/share/mirador/agent"

// EnteteSHA256, EnteteVersion : empreinte et version du fichier envoyé (le
// script les vérifie).
const (
	EnteteSHA256  = "X-Mirador-SHA256"
	EnteteVersion = "X-Mirador-Version"
	// EnteteManifeste : manifeste signé du binaire envoyé (JSON en base64).
	EnteteManifeste = "X-Mirador-Manifeste"
	// EnteteSigne : « oui » si EnteteManifeste est présent, « non » sinon
	// (binaire embarqué non signé : le script refuse si une clé est connue).
	EnteteSigne = "X-Mirador-Signe"
)

// jetonInstallation : le jeton de l'en-tête Authorization est-il un jeton
// d'enrôlement valable (pas expiré, token pas encore remis) ?
func (f *Flotte) jetonInstallation(ctx context.Context, r *http.Request, maintenant time.Time) bool {
	jeton, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || len(jeton) < 20 || len(jeton) > 256 {
		return false
	}
	if !strings.HasPrefix(jeton, PrefixeJeton) {
		// Machine déjà enrôlée qui relance le script (réinstallation, clé de
		// signature, --auto-update) : son token suffit (11/11). Mêmes règles
		// que l'ingestion : token valable, non révoqué, adresse autorisée.
		if f.opt.Ingestion == nil {
			return false
		}
		_, err := f.opt.Ingestion.Auth().Verifier(ctx, jeton, web.AdresseClient(r), maintenant)
		return err == nil
	}
	e := ingestion.Empreinte(jeton)
	var n int
	err := f.opt.DB.QueryRowContext(ctx, `SELECT COUNT(*) FROM jetons_enrolement WHERE empreinte = ? AND expire_le > ? AND token_remis_le = 0`,
		e[:], maintenant.Unix()).Scan(&n)
	return err == nil && n == 1
}

// accueillirInstallation : limites par adresse puis jeton.
func (f *Flotte) accueillirInstallation(w http.ResponseWriter, r *http.Request) bool {
	maintenant := f.opt.Maintenant()
	ip := web.AdresseClient(r).String()
	cle := ingestion.CleLimite(web.AdresseClient(r))
	if !f.demandes.Prendre(cle, 1, maintenant) || !f.echecs.Prendre(cle, 0, maintenant) {
		w.Header().Set("Retry-After", "30")
		erreur(w, http.StatusTooManyRequests, "trop de demandes")
		return false
	}
	if !f.jetonInstallation(r.Context(), r, maintenant) {
		f.echecs.Prendre(cle, 1, maintenant)
		f.opt.Journal.Warn("téléchargement de l'installation refusé", "ip", ip)
		erreur(w, http.StatusUnauthorized, "jeton d'enrôlement (ou token de la machine) absent, expiré ou déjà utilisé")
		return false
	}
	return true
}

// scriptInstallation : le script est public (il est dans le dépôt, sous
// GPL, sans rien de secret) : pas de jeton pour le télécharger (demande du
// 11/11). Le BINAIRE, lui, exige le jeton (le script l'envoie). Limite par
// adresse comme l'enrôlement ; nginx ne laisse passer que les adresses
// autorisées.
func (f *Flotte) scriptInstallation(w http.ResponseWriter, r *http.Request) {
	f.servirScript(w, r, installagent.Script, "mirador-agent-install.sh")
}

// scriptInstallationCollecteur : même chose pour mirador-collector (SPEC § 23.13).
func (f *Flotte) scriptInstallationCollecteur(w http.ResponseWriter, r *http.Request) {
	f.servirScript(w, r, installcollecteur.Script, "mirador-collector-install.sh")
}

func (f *Flotte) servirScript(w http.ResponseWriter, r *http.Request, script []byte, nom string) {
	if !f.demandes.Prendre(ingestion.CleLimite(web.AdresseClient(r)), 1, f.opt.Maintenant()) {
		w.Header().Set("Retry-After", "30")
		erreur(w, http.StatusTooManyRequests, "trop de demandes")
		return
	}
	h := sha256.Sum256(script)
	w.Header().Set("Content-Type", "text/x-shellscript; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+nom+`"`)
	w.Header().Set(EnteteSHA256, hex.EncodeToString(h[:]))
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write(script)
}

// empreintes des binaires embarqués (calculées une fois par fichier).
type empreinteFichier struct {
	taille int64
	modif  time.Time
	sha    string
}

var (
	muEmpreintes sync.Mutex
	empreintes   = map[string]empreinteFichier{}
)

func empreinteDe(chemin string, info os.FileInfo) (string, error) {
	muEmpreintes.Lock()
	e, ok := empreintes[chemin]
	muEmpreintes.Unlock()
	if ok && e.taille == info.Size() && e.modif.Equal(info.ModTime()) {
		return e.sha, nil
	}
	fd, err := os.Open(chemin) // #nosec G304 G703 -- chemin fabriqué ici (dossier fixe + architecture constante)
	if err != nil {
		return "", err
	}
	defer fd.Close()
	h := sha256.New()
	if _, err := io.Copy(h, fd); err != nil {
		return "", err
	}
	s := hex.EncodeToString(h.Sum(nil))
	muEmpreintes.Lock()
	empreintes[chemin] = empreinteFichier{info.Size(), info.ModTime(), s}
	muEmpreintes.Unlock()
	return s, nil
}

// archConnue : « amd64 » ou « arm64 » (constantes), sinon vide.
func archConnue(a string) string {
	switch a {
	case "amd64":
		return "amd64"
	case "arm64":
		return "arm64"
	}
	return ""
}

// produitConnu : produit demandé (paramètre « produit ») recopié depuis une
// constante ; mirador-agent par défaut, vide si inconnu.
func produitConnu(p string) string {
	switch p {
	case "", protocole.ProduitAgent:
		return protocole.ProduitAgent
	case protocole.ProduitCollecteur:
		return protocole.ProduitCollecteur
	}
	return ""
}

// binaireInstallation : chemin, version, empreinte et manifeste signé (nil
// s'il n'y en a pas) du binaire à servir.
func (f *Flotte) binaireInstallation(ctx context.Context, produit, arch string) (chemin, version, sha string, m *protocole.Manifeste, err error) {
	if v, e := CibleGlobale(ctx, f.opt.DB); e == nil && v != "" {
		if m, e := f.manifeste(ctx, produit, v, arch); e == nil && m != nil {
			return filepath.Join(f.opt.DossierVersions, nomBinaire(produit, v, arch)), v, m.SHA256, m, nil
		}
	}
	dossier := f.opt.DossierAgent
	if dossier == "" {
		dossier = DossierAgentEmbarque
	}
	chemin = filepath.Join(dossier, produit+"-linux-"+arch)
	info, err := os.Stat(chemin) // #nosec G703 -- dossier fixe + produit et architecture constants (produitConnu, archConnue)
	if err != nil {
		return "", "", "", nil, fmt.Errorf("aucun binaire de %s : publier une version (Machines › Mises à jour des agents)", produit)
	}
	sha, err = empreinteDe(chemin, info)
	if err != nil {
		return "", "", "", nil, err
	}
	// Binaire embarqué identique à une version publiée (même empreinte) :
	// son manifeste signé part avec lui.
	if m := f.manifesteParEmpreinte(ctx, produit, arch, sha); m != nil {
		return chemin, m.Version, sha, m, nil
	}
	return chemin, "embarquée", sha, nil, nil
}

// manifesteParEmpreinte : manifeste publié (le plus récent) dont l'empreinte
// est celle du binaire, sinon nil.
func (f *Flotte) manifesteParEmpreinte(ctx context.Context, produit, arch, sha string) *protocole.Manifeste {
	var v string
	err := f.opt.DB.QueryRowContext(ctx, `SELECT version FROM versions_binaires WHERE produit = ? AND architecture = ? AND sha256 = ? ORDER BY publiee_le DESC LIMIT 1`,
		produit, arch, sha).Scan(&v)
	if err != nil {
		return nil
	}
	m, err := f.manifeste(ctx, produit, v, arch)
	if err != nil || m == nil || m.SHA256 != sha {
		return nil
	}
	return m
}

// enteteManifeste : manifeste en JSON (sans espace) puis en base64 : une
// seule ligne d'en-tête, que le script décode et lit avec grep / sed.
func enteteManifeste(m *protocole.Manifeste) (string, error) {
	brut, err := json.Marshal(m)
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(brut), nil
}

func (f *Flotte) binaireAInstaller(w http.ResponseWriter, r *http.Request) {
	if !f.accueillirInstallation(w, r) {
		return
	}
	// Architecture recopiée depuis une constante : rien de ce que le client
	// envoie n'entre dans un chemin de fichier.
	arch := archConnue(r.URL.Query().Get(protocole.ParametreArchitecture))
	produit := produitConnu(r.URL.Query().Get(protocole.ParametreProduit))
	if arch == "" || produit == "" {
		erreur(w, http.StatusBadRequest, "architecture (amd64 ou arm64) ou produit inconnu")
		return
	}
	chemin, version, sha, manifeste, err := f.binaireInstallation(r.Context(), produit, arch)
	if err != nil {
		if !errors.Is(err, sql.ErrNoRows) {
			f.opt.Journal.Warn("installation : binaire indisponible", "arch", arch, "erreur", err)
		}
		erreur(w, http.StatusNotFound, err.Error())
		return
	}
	fd, err := os.Open(chemin) // #nosec G304 G703 -- dossier fixe, version publiée, architecture constante
	if err != nil {
		erreur(w, http.StatusNotFound, "binaire absent")
		return
	}
	defer fd.Close()
	info, err := fd.Stat()
	if err != nil {
		erreur(w, http.StatusNotFound, "binaire illisible")
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(10 * time.Minute))
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Length", fmt.Sprint(info.Size()))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set(EnteteSHA256, sha)
	w.Header().Set(EnteteVersion, version)
	// Manifeste signé du MÊME binaire (même réponse, même jeton) ; sinon
	// « non » : le script refuse alors si une clé publique est connue.
	w.Header().Set(EnteteSigne, "non")
	if manifeste != nil {
		if v, err := enteteManifeste(manifeste); err == nil {
			w.Header().Set(EnteteManifeste, v)
			w.Header().Set(EnteteSigne, "oui")
		}
	}
	if _, err := io.Copy(w, fd); err != nil {
		f.opt.Journal.Warn("installation : envoi du binaire interrompu", "erreur", err)
	}
}
