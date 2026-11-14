// SPDX-License-Identifier: GPL-3.0-or-later

package modeles

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"mirador-stack/internal/agent/config"
)

// Listes des modèles (partie B, SPEC § 24.10) : l'admin saisit une valeur
// par ligne (adresse, hôte:port, fichier…), chaque ligne devient un check
// de l'agent, étiqueté avec le nom du modèle.
//
// Format d'une ligne : la cible, puis pour une adresse http(s) :
//   - « non-verifie »   : certificat maison accepté (sa date de fin reste mesurée) ;
//   - « contient=TEXTE » : texte attendu dans la page (jusqu'à la fin de la ligne).

// MaxLignes : lignes au plus par liste (l'agent prend 50 checks au plus en tout).
const MaxLignes = config.MaxChecks

// Liste : une liste du modèle par son nom (nil si absente).
func (m *Modele) Liste(nom string) *ListeParam {
	for i := range m.Listes {
		if m.Listes[i].Nom == nom {
			return &m.Listes[i]
		}
	}
	return nil
}

// LireLignes : texte saisi (une valeur par ligne) → lignes propres et
// vérifiées. Les lignes vides et celles qui commencent par « # » sont
// ignorées.
func (l *ListeParam) LireLignes(texte string) ([]string, error) {
	var out []string
	vus := map[string]bool{}
	for i, brut := range strings.Split(strings.ReplaceAll(texte, "\r\n", "\n"), "\n") {
		ligne := strings.Join(strings.Fields(brut), " ")
		if ligne == "" || strings.HasPrefix(ligne, "#") {
			continue
		}
		if _, err := l.Check(ligne, "verif"); err != nil {
			return nil, fmt.Errorf("%s, ligne %d : %w", l.Titre, i+1, err)
		}
		if vus[ligne] {
			continue
		}
		vus[ligne] = true
		out = append(out, ligne)
	}
	if len(out) > MaxLignes {
		return nil, fmt.Errorf("%s : %d lignes au plus", l.Titre, MaxLignes)
	}
	return out, nil
}

// Check : une ligne de la liste → check de l'agent (vérifié par le code de
// l'agent).
func (l *ListeParam) Check(ligne, modele string) (config.Check, error) {
	cible, reste, _ := strings.Cut(ligne, " ")
	c := config.Check{Type: l.Type, Cible: cible, Modele: modele}
	for reste != "" {
		if l.Type != "http" {
			return c, errors.New("une seule valeur par ligne")
		}
		var mot string
		mot, reste, _ = strings.Cut(reste, " ")
		switch {
		case mot == "non-verifie":
			non := false
			c.VerifierTLS = &non
		case strings.HasPrefix(mot, "contient="):
			c.Contient = strings.TrimPrefix(mot, "contient=")
			if reste != "" {
				c.Contient += " " + reste
			}
			reste = ""
			if c.Contient == "" {
				return c, errors.New("« contient= » sans texte")
			}
		default:
			return c, fmt.Errorf("« %s » inconnu (après l'adresse : non-verifie, contient=texte)", mot)
		}
	}
	c.Nom = nomCheck(cible)
	return c, c.Valider()
}

var reHorsNom = regexp.MustCompile(`[^A-Za-z0-9._:/@-]+`)

// nomCheck : nom lisible tiré de la cible (« https:// » retiré, caractères
// interdits remplacés, 64 caractères au plus).
func nomCheck(cible string) string {
	n := cible
	for _, p := range []string{"https://", "http://"} {
		n = strings.TrimPrefix(n, p)
	}
	n = strings.TrimSuffix(reHorsNom.ReplaceAllString(n, "_"), "/")
	if n == "" {
		n = "check"
	}
	if len(n) > 60 {
		n = n[:60]
	}
	return n
}

// checksEffectifs : checks de tous les modèles d'une machine (noms rendus
// uniques dans chaque modèle ; 50 au plus, les suivants sont laissés de
// côté et comptés).
func checksEffectifs(effectifs []Effectif) (checks []config.Check, ecartes int) {
	for _, e := range effectifs {
		noms := map[string]int{}
		for _, lp := range e.Modele.Listes {
			for _, ligne := range e.Listes[lp.Nom] {
				c, err := lp.Check(ligne, e.Modele.Nom)
				if err != nil {
					continue // ligne enregistrée par une version plus souple
				}
				cle := c.Type + "|" + c.Nom
				noms[cle]++
				if n := noms[cle]; n > 1 {
					c.Nom += "-" + strconv.Itoa(n)
				}
				if len(checks) >= config.MaxChecks {
					ecartes++
					continue
				}
				checks = append(checks, c)
			}
		}
	}
	return checks, ecartes
}

// NombreChecks : checks demandés par les modèles d'une machine, et ceux
// laissés de côté (au-delà de 50).
func NombreChecks(effectifs []Effectif) (int, int) {
	c, e := checksEffectifs(effectifs)
	return len(c), e
}
