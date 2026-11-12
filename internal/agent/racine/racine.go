// SPDX-License-Identifier: GPL-3.0-or-later

// Package racine est l'« assistant root » de mirador-agent (SPEC.md § 6.4).
//
// Certaines mesures demandent les droits root : politique du pare-feu, état
// SMART des disques, certificats rangés dans des dossiers réservés à root
// (/etc/letsencrypt/live, /etc/pve). L'agent principal tourne SANS root : il
// ne peut pas les faire.
//
// Fonctionnement :
//   - une copie du binaire, posée par le script d'installation dans un dossier
//     appartenant à root, est lancée par un timer systemd toutes les 15 min
//     avec l'option -collecte-root ;
//   - elle fait les mesures, avec des programmes et des arguments FIGÉS dans
//     le code, puis écrit un petit fichier JSON dans /run/mirador-agent/
//     (mémoire, pas de disque) et s'arrête ;
//   - l'agent lit ce fichier et envoie les valeurs.
//
// L'assistant n'ouvre aucune connexion réseau et n'est jamais mis à jour par
// la mise à jour automatique. De la configuration distante, il ne lit qu'UNE
// clé, les chemins de certificats ajoutés dans l'interface, avec des règles
// strictes (SPEC.md § 21.3) ; MIRADOR_CERTIFICATS_DISTANTS=non l'en empêche.
package racine

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/version"
)

// Resultat est le contenu du fichier écrit par l'assistant.
type Resultat struct {
	Version     string       `json:"version"`
	Format      int          `json:"format,omitempty"` // FormatAssistant (0 : assistant d'avant le 12/11)
	Horodatage  time.Time    `json:"horodatage"`
	PareFeu     *PareFeu     `json:"pare_feu,omitempty"`
	Smart       []Disque     `json:"smart,omitempty"`
	SmartLu     time.Time    `json:"smart_lu,omitempty"`
	Certificats *Certificats `json:"certificats,omitempty"`
	Erreurs     []string     `json:"erreurs,omitempty"`
}

// TailleMaxFichier borne la lecture du fichier par l'agent.
const TailleMaxFichier = 1 << 20

// periodeSmart : SMART n'est relu que toutes les 30 minutes (l'assistant
// tourne toutes les 15) ; entre-temps, la mesure précédente est reprise.
const periodeSmart = 30 * time.Minute

// Executer fait les mesures demandées par la configuration et écrit le
// fichier de résultat.
func Executer(cfg config.Agent, maintenant time.Time) error {
	if os.Geteuid() != 0 {
		return errors.New("l'assistant doit tourner en root (timer mirador-agent-root)")
	}
	precedent, _ := Lire(cfg.FichierRoot) // mesure SMART précédente, si elle existe
	var errDistants error
	if cfg.CertificatsDistants && cfg.Serveur != "" && cfg.FichierConfigDistante != "" {
		cfg.CertificatsAjout, _, errDistants = MotifsDistants(cfg.FichierConfigDistante)
	} else {
		cfg.CertificatsAjout = nil
	}
	r := Mesurer(cfg, maintenant, precedent, programmesReels())
	if errDistants != nil {
		r.Erreurs = append(r.Erreurs, "certificats : "+errDistants.Error())
	}
	// Mise à jour de l'assistant lui-même (SPEC § 22.5) : sert au passage
	// suivant. Le résultat est dans le journal (systemd) et dans root.json.
	if cible, err := os.Executable(); err == nil {
		msg, err := MettreAJourAssistant(cfg, cible)
		if err != nil {
			r.Erreurs = append(r.Erreurs, "mise à jour de l'assistant : "+err.Error())
			fmt.Fprintln(os.Stderr, "mise à jour de l'assistant root refusée :", err)
		} else if msg != "" {
			fmt.Fprintln(os.Stderr, msg)
		}
	}
	return Ecrire(cfg.FichierRoot, r)
}

// Mesurer fait les mesures avec les programmes donnés (remplacés dans les tests).
func Mesurer(cfg config.Agent, maintenant time.Time, precedent *Resultat, prog programmes) *Resultat {
	r := &Resultat{Version: version.Version, Format: FormatAssistant, Horodatage: maintenant.UTC()}
	if cfg.PareFeu {
		pf, err := lirePareFeu(prog)
		if err != nil {
			r.Erreurs = append(r.Erreurs, "pare-feu : "+err.Error())
		} else {
			r.PareFeu = pf
		}
	}
	if cfg.Smart {
		if precedent != nil && !precedent.SmartLu.IsZero() && maintenant.Sub(precedent.SmartLu) < periodeSmart {
			r.Smart, r.SmartLu = precedent.Smart, precedent.SmartLu
		} else {
			disques, err := lireSmart(prog)
			if err != nil {
				r.Erreurs = append(r.Erreurs, "smart : "+err.Error())
			}
			r.Smart, r.SmartLu = disques, maintenant.UTC()
		}
	}
	if len(cfg.Certificats) > 0 || len(cfg.CertificatsAjout) > 0 {
		r.Certificats = lireCertificats(cfg.Certificats, cfg.CertificatsAjout)
	}
	return r
}

// Ecrire enregistre le résultat de façon atomique (fichier temporaire puis
// renommage) : l'agent ne lit jamais un fichier à moitié écrit.
// Droits : 0640, groupe de l'agent (hérité du dossier, voir l'unité systemd).
func Ecrire(chemin string, r *Resultat) error {
	donnees, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(donnees) > TailleMaxFichier {
		return fmt.Errorf("résultat trop gros (%d octets)", len(donnees))
	}
	dir := filepath.Dir(chemin)
	tmp, err := os.CreateTemp(dir, ".root-*.json")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name()) // sans effet après le renommage
	if _, err := tmp.Write(donnees); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Chmod(0o640); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), chemin)
}

// Lire relit le fichier de résultat (côté agent, ou par l'assistant pour
// reprendre la mesure SMART précédente).
func Lire(chemin string) (*Resultat, error) {
	f, err := os.Open(chemin) // #nosec G304 -- chemin issu de la configuration validée
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > TailleMaxFichier {
		return nil, fmt.Errorf("%s trop gros", chemin)
	}
	var r Resultat
	dec := json.NewDecoder(f)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("%s illisible : %w", chemin, err)
	}
	return &r, nil
}
