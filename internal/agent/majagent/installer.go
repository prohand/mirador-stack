// SPDX-License-Identifier: GPL-3.0-or-later

package majagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"mirador-stack/internal/protocole"
)

// Installateur gère le dossier des binaires (SPEC.md § 6.8) :
//
//	/var/lib/mirador-agent/bin/            (appartient à l'utilisateur de l'agent)
//	  mirador-agent-1.2.3                  binaires (0755)
//	  current -> mirador-agent-1.2.3       lien lancé par l'unité systemd
//	  precedent -> mirador-agent-1.1.0     version précédente (retour arrière)
//	  essai.json                           mise à jour en cours d'essai
//	  echec.json                           dernier échec (signalé au serveur)
//
// L'agent se met à jour sans root. Il ne peut pas toucher à l'assistant root,
// rangé ailleurs et appartenant à root : l'assistant se met à jour lui-même à
// partir de ce dossier (SPEC.md § 22.5).
type Installateur struct {
	Dossier string
	// SelfTest lance le nouveau binaire en mode contrôle ; remplaçable dans les tests.
	SelfTest   func(ctx context.Context, chemin string) error
	maintenant func() time.Time
}

// Essai : mise à jour activée, en attente de confirmation.
type Essai struct {
	Precedent string    `json:"precedent"` // nom du binaire précédent
	Nouveau   string    `json:"nouveau"`
	Debut     time.Time `json:"debut"`
}

// NewInstallateur prépare l'installateur ; configuration : fichier passé au
// -self-test du nouveau binaire.
func NewInstallateur(dossier, configuration string) *Installateur {
	return &Installateur{
		Dossier: dossier,
		SelfTest: func(ctx context.Context, chemin string) error {
			cmd := exec.CommandContext(ctx, chemin, "-self-test", "-config", configuration) // #nosec G204 -- binaire vérifié (signature + SHA-256), arguments figés
			cmd.Env = []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"}
			sortie, err := cmd.CombinedOutput()
			if err != nil {
				return fmt.Errorf("self-test du nouveau binaire en échec : %v : %s", err, strings.TrimSpace(string(sortie)))
			}
			return nil
		},
		maintenant: time.Now,
	}
}

// suffixeManifeste : manifeste signé gardé à côté de chaque binaire.
const suffixeManifeste = ".manifeste.json"

var reVersion = regexp.MustCompile(`^[0-9]{1,5}\.[0-9]{1,5}\.[0-9]{1,5}$`)

// Installer écrit le binaire vérifié, le teste, puis l'active de façon
// atomique. L'agent doit ensuite s'arrêter : systemd le relance sur la
// nouvelle version.
func (i *Installateur) Installer(ctx context.Context, m *protocole.Manifeste, binaire []byte) error {
	if !reVersion.MatchString(m.Version) {
		return fmt.Errorf("version %q invalide", m.Version)
	}
	if err := VerifierBinaire(m, binaire); err != nil {
		return err
	}
	nom := "mirador-agent-" + m.Version
	final := filepath.Join(i.Dossier, nom)

	tmp, err := os.CreateTemp(i.Dossier, ".telechargement-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(binaire); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Sync(); err != nil {
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Chmod(0o755); err != nil { // #nosec G302 -- binaire exécutable
		return errors.Join(err, tmp.Close())
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), final); err != nil {
		return err
	}
	// Manifeste signé gardé à côté du binaire : l'assistant root s'en sert
	// pour vérifier LUI-MÊME la signature avant de se mettre à jour (SPEC §
	// 22.5). Écrit sous l'identité de l'agent : sans valeur pour un faussaire
	// (la signature est revérifiée par root avec sa propre clé).
	if brut, err := json.Marshal(m); err == nil {
		_ = os.WriteFile(final+suffixeManifeste, brut, 0o644) // #nosec G306 -- manifeste public
	}

	ctxTest, annuler := context.WithTimeout(ctx, 30*time.Second)
	defer annuler()
	if err := i.SelfTest(ctxTest, final); err != nil {
		_ = os.Remove(final)
		return err
	}

	precedent, _ := os.Readlink(filepath.Join(i.Dossier, "current"))
	essai := Essai{Precedent: filepath.Base(precedent), Nouveau: nom, Debut: i.maintenant().UTC()}
	if err := i.ecrireJSON("essai.json", essai); err != nil {
		return err
	}
	// Lien fixe vers l'ancienne version : l'unité systemd de retour arrière
	// (mirador-agent-retour) lance toujours « bin/precedent -retour-arriere ».
	if essai.Precedent != "" && essai.Precedent != "." {
		lienTmp := filepath.Join(i.Dossier, "precedent.nouveau")
		_ = os.Remove(lienTmp)
		if err := os.Symlink(essai.Precedent, lienTmp); err != nil {
			return err
		}
		if err := os.Rename(lienTmp, filepath.Join(i.Dossier, "precedent")); err != nil {
			return err
		}
	}
	return i.basculer(nom)
}

// basculer remplace le lien « current » de façon atomique (lien temporaire
// puis renommage : il n'existe jamais d'instant sans lien valide).
func (i *Installateur) basculer(cible string) error {
	tmp := filepath.Join(i.Dossier, "current.nouveau")
	_ = os.Remove(tmp)
	if err := os.Symlink(cible, tmp); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(i.Dossier, "current"))
}

// EnEssai renvoie la mise à jour en cours d'essai (nil s'il n'y en a pas).
func (i *Installateur) EnEssai() (*Essai, error) {
	var e Essai
	brut, err := os.ReadFile(filepath.Join(i.Dossier, "essai.json")) // #nosec G304 -- dossier de l'agent
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(brut, &e); err != nil {
		return nil, err
	}
	return &e, nil
}

// Confirmer : le nouvel agent a réussi à joindre le serveur. L'essai est
// terminé ; seuls la version actuelle et la précédente sont gardées.
func (i *Installateur) Confirmer() error {
	e, err := i.EnEssai()
	if err != nil || e == nil {
		return err
	}
	if err := os.Remove(filepath.Join(i.Dossier, "essai.json")); err != nil {
		return err
	}
	i.nettoyer(e.Nouveau, e.Precedent)
	return nil
}

// RetourArriere remet la version précédente (le nouvel agent ne s'est pas
// signalé à temps, ou ne démarre pas) et note l'échec pour le serveur.
func (i *Installateur) RetourArriere(raison string) error {
	e, err := i.EnEssai()
	if err != nil {
		return err
	}
	if e == nil || e.Precedent == "" || e.Precedent == "." {
		return errors.New("aucune version précédente connue")
	}
	if !strings.HasPrefix(e.Precedent, "mirador-agent-") || strings.ContainsAny(e.Precedent, "/\\") {
		return fmt.Errorf("nom de version précédente invalide %q", e.Precedent)
	}
	if err := i.basculer(e.Precedent); err != nil {
		return err
	}
	_ = i.ecrireJSON("echec.json", map[string]string{
		"version": strings.TrimPrefix(e.Nouveau, "mirador-agent-"), "raison": raison,
		"date": i.maintenant().UTC().Format(time.RFC3339),
	})
	return os.Remove(filepath.Join(i.Dossier, "essai.json"))
}

// DernierEchec : version et raison du dernier retour arrière (vides s'il n'y
// en a pas). L'agent ne retente pas une version qui a déjà échoué.
func (i *Installateur) DernierEchec() (version, raison string) {
	var e map[string]string
	brut, err := os.ReadFile(filepath.Join(i.Dossier, "echec.json")) // #nosec G304 -- dossier de l'agent
	if err != nil || json.Unmarshal(brut, &e) != nil {
		return "", ""
	}
	return e["version"], e["raison"]
}

// nettoyer supprime les anciens binaires, sauf ceux à garder (et leur
// manifeste signé, dont l'assistant root a besoin : SPEC § 22.5).
func (i *Installateur) nettoyer(garder ...string) {
	entrees, err := os.ReadDir(i.Dossier)
	if err != nil {
		return
	}
	ok := map[string]bool{}
	for _, g := range garder {
		ok[g] = true
		ok[g+suffixeManifeste] = true
	}
	for _, e := range entrees {
		if strings.HasPrefix(e.Name(), "mirador-agent-") && !ok[e.Name()] {
			_ = os.Remove(filepath.Join(i.Dossier, e.Name()))
		}
	}
}

func (i *Installateur) ecrireJSON(nom string, v any) error {
	brut, err := json.Marshal(v)
	if err != nil {
		return err
	}
	tmp := filepath.Join(i.Dossier, nom+".tmp")
	if err := os.WriteFile(tmp, brut, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(i.Dossier, nom))
}
