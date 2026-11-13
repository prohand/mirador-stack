// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"syscall"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/version"
)

// FormatAssistant : numéro du comportement de l'assistant root, écrit dans
// root.json (SPEC § 22.5). Il ne change que lorsque l'assistant lui-même
// change (pas à chaque version de l'agent) : l'interface ne signale un
// assistant « trop ancien » que lorsqu'il lui manque vraiment quelque chose.
//
//	1 : avant le 12/11 (pas de numéro) ;
//	2 : compte rendu des chemins de certificats, raisons, mise à jour signée ;
//	3 : liste des invités Proxmox (SPEC § 23.7).
const FormatAssistant = 3

// FormatRequis : numéro dont l'agent a besoin. Le 3 n'apporte rien hors
// Proxmox : pas d'alerte « assistant trop ancien » pour rien.
func FormatRequis(proxmox bool) int {
	if proxmox {
		return 3
	}
	return 2
}

// Mise à jour de l'assistant root par lui-même (SPEC § 22.5), avec ces
// sécurités :
//   - seulement si les mises à jour automatiques sont activées
//     (MIRADOR_MAJ_AUTO) et pas interdites sur la machine
//     (MIRADOR_MAJ_RACINE=non, fichier local appartenant à root) ;
//   - seulement la version en service de l'agent, une fois CONFIRMÉE (plus
//     d'essai en cours) : elle a déjà été vérifiée, testée et a tourné ;
//   - jamais une version plus ancienne ou égale ;
//   - signature Ed25519 revérifiée par root avec la clé du fichier root
//     /etc/mirador-agent/cles-publiques.txt (ni l'agent ni le serveur ne
//     peuvent la changer) ;
//   - fichiers du dossier de l'agent ouverts sans suivre de lien, taille
//     bornée ; empreinte et taille vérifiées sur les octets LUS, et ce sont
//     ces octets-là qui sont écrits (pas de relecture du fichier) ;
//   - remplacement atomique, ancienne version gardée (.precedent).
// La nouvelle version sert au passage suivant de l'assistant.

var reBinaireAgent = regexp.MustCompile(`^mirador-agent-([0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5})$`)

// dossierAssistant : seul dossier où l'assistant accepte de se remplacer.
var dossierAssistant = "/usr/local/lib/mirador-agent/"

// MettreAJourAssistant : renvoie un message si l'assistant a été remplacé,
// "" s'il n'y avait rien à faire, ou une erreur (mise à jour refusée).
func MettreAJourAssistant(cfg config.Agent, cible string) (string, error) {
	if !cfg.MajAuto || !cfg.MajRacine || cfg.DossierBin == "" {
		return "", nil
	}
	// Chemin FIXE : seul le binaire de l'unité systemd est remplacé ; lancé
	// d'ailleurs (essai à la main), l'assistant ne remplace rien.
	fixe := dossierAssistant + "mirador-agent"
	if filepath.Clean(cible) != fixe {
		return "", nil
	}
	bin := cfg.DossierBin
	if _, err := os.Lstat(filepath.Join(bin, "essai.json")); err == nil {
		return "", nil // version de l'agent encore à l'essai : on attend sa confirmation
	}
	lien, err := os.Readlink(filepath.Join(bin, "current"))
	if err != nil {
		return "", nil
	}
	nom := filepath.Base(lien)
	m := reBinaireAgent.FindStringSubmatch(nom)
	if m == nil || lien != nom {
		return "", fmt.Errorf("lien « current » inattendu : %q", lien)
	}
	cmp, err := majagent.ComparerVersions(m[1], version.Version)
	if err != nil || cmp <= 0 {
		return "", err // même version, ou plus ancienne : jamais de retour arrière en root
	}
	brutM, err := lireSansLien(filepath.Join(bin, nom+".manifeste.json"), 64<<10)
	if errors.Is(err, os.ErrNotExist) {
		// Version posée sans son manifeste (agent d'avant le 13/11) : rien
		// à vérifier, donc rien à installer. Signalé pour qu'on le voie.
		return "", fmt.Errorf("manifeste signé absent pour %s : attente de la version suivante", m[1])
	}
	if err != nil {
		return "", fmt.Errorf("manifeste : %w", err)
	}
	var man protocole.Manifeste
	dec := json.NewDecoder(bytes.NewReader(brutM))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&man); err != nil {
		return "", fmt.Errorf("manifeste illisible : %w", err)
	}
	if man.Version != m[1] {
		return "", fmt.Errorf("manifeste pour %s, binaire %s", man.Version, m[1])
	}
	cles, err := majagent.ClesInstallees(cfg.FichierCles)
	if err != nil {
		return "", fmt.Errorf("clés de signature : %w", err)
	}
	if err := majagent.VerifierManifeste(&man, version.Version, cles); err != nil {
		return "", fmt.Errorf("version %s refusée : %w", man.Version, err)
	}
	binaire, err := lireSansLien(filepath.Join(bin, nom), majagent.TailleMax)
	if err != nil {
		return "", fmt.Errorf("binaire : %w", err)
	}
	if err := majagent.VerifierBinaire(&man, binaire); err != nil {
		return "", fmt.Errorf("version %s refusée : %w", man.Version, err)
	}
	if err := remplacer(binaire); err != nil {
		return "", err
	}
	return fmt.Sprintf("assistant root mis à jour : %s → %s (signature vérifiée)", version.Version, man.Version), nil
}

// lireSansLien : fichier ordinaire, sans suivre de lien, sans bloquer, borné.
func lireSansLien(chemin string, max int64) ([]byte, error) {
	f, err := os.OpenFile(chemin, os.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_NOCTTY, 0) // #nosec G304 -- dossier de l'agent, nom vérifié
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() || info.Size() > max {
		return nil, errors.New("pas un fichier ordinaire de taille raisonnable")
	}
	return io.ReadAll(io.LimitReader(f, max+1))
}

// remplacer écrit le nouveau binaire à côté, garde l'ancien, puis renomme
// (atomique : l'unité systemd trouve toujours un binaire complet).
func remplacer(binaire []byte) error {
	cible := dossierAssistant + "mirador-agent"
	tmp := cible + ".nouveau"
	_ = os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o755) // #nosec G302 G304 -- binaire root, chemin fixe
	if err != nil {
		return err
	}
	if _, err := f.Write(binaire); err != nil {
		return errors.Join(err, f.Close(), os.Remove(tmp))
	}
	if err := f.Sync(); err != nil {
		return errors.Join(err, f.Close(), os.Remove(tmp))
	}
	if err := f.Close(); err != nil {
		return errors.Join(err, os.Remove(tmp))
	}
	// Ancienne version gardée sous un second nom (lien physique : aucune
	// copie ; pour un retour à la main, « mv mirador-agent.precedent
	// mirador-agent »). Puis renommage atomique du nouveau binaire.
	precedent := cible + ".precedent"
	_ = os.Remove(precedent)
	_ = os.Link(cible, precedent)
	return os.Rename(tmp, cible)
}
