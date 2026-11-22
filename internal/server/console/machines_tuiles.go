// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strconv"
	"strings"
)

// Page Machines, nouvelle présentation (maquette validée le 22/11, SPEC
// § 120) : quatre cases d'état, demande d'approbation en bandeau, une tuile
// par machine (jauges CPU, mémoire, disque et pastilles de problème) ; le
// tableau reste à un clic (choix gardé par le navigateur).
//
// Coût : les mesures sont celles de la vue d'ensemble (même fonction, mêmes
// requêtes, gardées dans le cache de requêtes de la console) ; rien en base.

// casesMachines : les quatre cases du haut (tout le parc, sans filtre).
type casesMachines struct {
	EnLigne, Approuvees, Muettes, EnAttente int
	PctEnLigne, PctMuettes                  int // largeurs de la barre (en %)
	NomsMuettes                             string
	ARegarder                               int
	DetailRegarder                          string
	Paquets, Securite                       int
	DetailPaquets                           string
}

// tuileMachine : une machine (ou un collecteur avec ses équipements).
type tuileMachine struct {
	vueMachine
	M           *ligneParc // mesures (nil : aucune, équipement sans agent…)
	Point       string     // ok, attention, erreur
	Icone       string
	Equipements []puceEquipement // collecteur : équipements relevés du même bloc
	// Collecteur : nombre d'équipements relevés du bloc ; pastilles gardées
	// pour ceux en difficulté seulement, 4 au plus (Autres : le reste). Les
	// équipements ont leur tuile : la tuile du collecteur ne grandit pas
	// avec le parc (question du 22/11).
	NbEquipements, Autres int
}

// pucesMax : pastilles d'équipements en difficulté dans la tuile d'un collecteur.
const pucesMax = 4

// puceEquipement : un équipement dans la tuile de son collecteur.
type puceEquipement struct {
	Nom, Point string
}

// pointMachine : couleur du point d'une machine.
func pointMachine(v vueMachine, m *ligneParc) string {
	switch {
	case v.Muette || v.VuLe == 0:
		return "erreur"
	case v.MajEchec != "":
		return "erreur"
	case m != nil && (m.Alertes > 0 || m.PleinDans != "" || m.PaquetsSecurite > 0):
		return "attention"
	}
	return "ok"
}

// iconeMachine : collecteur, équipement relevé, ou machine à agent.
func iconeMachine(v vueMachine) string {
	switch {
	case v.Collecteur():
		return "reseau"
	case strings.Contains(strings.ToLower(v.Env), "stockage"), strings.Contains(strings.ToLower(v.Env), "nas"):
		return "stockage"
	case strings.Contains(strings.ToLower(v.Env), "firewall"), strings.Contains(strings.ToLower(v.Env), "pare-feu"):
		return "bouclier"
	case v.ReleveePar != "":
		return "routeur"
	case strings.Contains(strings.ToLower(v.Env), "physique"):
		return "carte-mere"
	}
	return "ecran"
}

// tuilesDe : tuiles d'un bloc ; un équipement placé sous son collecteur
// (Sous) a sa tuile, juste après lui, et une puce dans la tuile du
// collecteur (retour du 22/11 : « il manque des machines » quand
// l'équipement n'était qu'une puce).
func tuilesDe(ms []vueMachine, mesures map[string]*ligneParc) []tuileMachine {
	var out []tuileMachine
	for _, v := range ms {
		m := mesures[v.Nom]
		if v.Sous {
			for i := len(out) - 1; i >= 0; i-- {
				if out[i].Nom == v.ReleveePar {
					t := &out[i]
					t.NbEquipements++
					if p := pointMachine(v, m); p != "ok" {
						if len(t.Equipements) < pucesMax {
							t.Equipements = append(t.Equipements, puceEquipement{Nom: v.Nom, Point: p})
						} else {
							t.Autres++
						}
					}
					break
				}
			}
		}
		out = append(out, tuileMachine{vueMachine: v, M: m, Point: pointMachine(v, m), Icone: iconeMachine(v)})
	}
	return out
}

// calculerCases : cases du haut de la page.
func calculerCases(ms []vueMachine, mesures map[string]*ligneParc, enAttente int) casesMachines {
	c := casesMachines{Approuvees: len(ms), EnAttente: enAttente}
	var muettes, regarder, paquets []string
	for _, v := range ms {
		m := mesures[v.Nom]
		if v.Muette || v.VuLe == 0 {
			c.Muettes++
			muettes = append(muettes, v.Nom)
		} else {
			c.EnLigne++
		}
		switch {
		case v.MajEchec != "":
			regarder = append(regarder, v.Nom+" : mise à jour en échec")
		case m != nil && m.PleinDans != "":
			regarder = append(regarder, v.Nom+" : disque "+m.PleinDans)
		case m != nil && m.Alertes > 0 && !v.Muette:
			regarder = append(regarder, v.Nom+" : "+accorderTexte(strconv.Itoa(m.Alertes)+" alerte(s)"))
		}
		if m != nil && m.Paquets > 0 {
			c.Paquets += m.Paquets
			c.Securite += m.PaquetsSecurite
			if m.PaquetsSecurite > 0 {
				paquets = append(paquets, v.Nom)
			}
		}
	}
	c.ARegarder = len(regarder)
	c.NomsMuettes = resumeNoms(muettes)
	c.DetailRegarder = resumeNoms(regarder)
	if c.Securite > 0 {
		c.DetailPaquets = accorderTexte("dont "+strconv.Itoa(c.Securite)+" de sécurité") + " (" + resumeNoms(paquets) + ")"
	}
	if c.Approuvees > 0 {
		c.PctEnLigne = c.EnLigne * 100 / c.Approuvees
		c.PctMuettes = 100 - c.PctEnLigne
	}
	return c
}

// resumeNoms : « a, b, c » ou « a, b, c et 4 autres ».
func resumeNoms(l []string) string {
	if len(l) <= 3 {
		return strings.Join(l, " · ")
	}
	if len(l) == 4 {
		return strings.Join(l[:3], " · ") + " et 1 autre"
	}
	return strings.Join(l[:3], " · ") + " et " + strconv.Itoa(len(l)-3) + " autres"
}

// Sous : ligne sous le nom de la tuile.
func (t tuileMachine) Sous() string {
	var p []string
	switch {
	case t.Collecteur():
		p = append(p, "collecteur")
	case t.ReleveePar != "":
		if t.Env != "" {
			p = append(p, t.Env)
		}
		p = append(p, "relevé par "+t.ReleveePar)
	default:
		for _, x := range []string{t.Env, t.Groupe} {
			if x != "" {
				p = append(p, x)
			}
		}
	}
	if t.Version != "" {
		if t.Collecteur() {
			p = append(p, t.Version)
		} else {
			p = append(p, "agent "+t.Version)
		}
	}
	return strings.Join(p, " · ")
}
