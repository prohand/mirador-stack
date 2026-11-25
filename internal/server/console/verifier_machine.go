// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"strings"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/flotte"
)

// « Vérifier maintenant » d'une machine (SPEC § 127, maquettes validées le
// 25/11) : sur la page d'une alerte et dans le bloc Services (lignes en
// échec). Le serveur réveille l'agent ; l'agent fait un relevé complet et
// relance l'assistant root (rien n'est lancé de plus que d'habitude) ; les
// alertes sont réévaluées 6 s puis 25 s après. La page montre l'attente,
// puis le résultat (relevé reçu après la demande).

// attenteVerif : délai minimal avant de montrer un résultat (relevé reçu,
// alertes réévaluées à 6 s).
const attenteVerif = 8 * time.Second

// vueVerif : bouton et état de la dernière vérification d'une machine.
type vueVerif struct {
	Machine  string
	Possible bool   // l'agent sait le faire (CapaciteVerifier)
	Pourquoi string // sinon : texte du bouton grisé
	Etat     string // "" (rien de récent), "attente", "sans-reponse", "fait"
	Il       string // « il y a 12 s »
	Recu     string // heure du relevé reçu après la demande
}

// EnAttente : la page se recharge seule (data-recharger).
func (v *vueVerif) EnAttente() bool { return v != nil && v.Etat == "attente" }

// verifMachine : nil pour une machine sans agent (envoi direct, équipement)
// ou un collecteur.
func (c *Console) verifMachine(m flotte.InfoMachine, t time.Time) *vueVerif {
	if m.Version == "" || m.Type == "collecteur" {
		return nil
	}
	v := &vueVerif{Machine: m.Nom, Possible: flotte.ACapacite(m.Capacites, protocole.CapaciteVerifier)}
	if !v.Possible {
		v.Pourquoi = "agent à mettre à jour (0.6.19 ou plus)"
	}
	d := flotte.VerificationDe(m.ID)
	if d.IsZero() || t.Sub(d) > 10*time.Minute || t.Before(d) {
		return v
	}
	v.Il = "il y a " + dureeLisible(t.Sub(d).Round(time.Second))
	if t.Sub(d) < 2*time.Second {
		v.Il = "à l'instant"
	}
	var recu int64
	if c.opt.Ingestion != nil {
		recu, _ = c.opt.Ingestion.DerniereReception(m.ID)
	}
	switch {
	case recu >= d.Unix() && t.Sub(d) >= attenteVerif:
		v.Etat = "fait"
		v.Recu = time.Unix(recu, 0).In(fuseau.Load()).Format("15:04:05")
	case t.Sub(d) < 30*time.Second:
		v.Etat = "attente"
	default:
		v.Etat = "sans-reponse"
	}
	return v
}

// resultatVerification : bandeau du résultat (Classe : ok, attention, info).
type resultatVerification struct {
	Classe, Titre, Texte string
	Recharger            bool
}

// resultatServices : résultat pour le bloc Services (services encore en
// échec après le relevé).
func resultatServices(v *vueVerif, echecs []string) *resultatVerification {
	if v == nil || v.Etat == "" {
		return nil
	}
	switch v.Etat {
	case "attente":
		return &resultatVerification{Classe: "info", Titre: "Vérification demandée " + v.Il,
			Texte: "l'agent relit ses services ; la page se met à jour seule", Recharger: true}
	case "sans-reponse":
		return &resultatVerification{Classe: "attention", Titre: "Pas de réponse de l'agent (" + v.Il + ")",
			Texte: "il se vérifiera à son rythme habituel (1 min)"}
	}
	if len(echecs) == 0 {
		return &resultatVerification{Classe: "ok", Titre: "Vérifié " + v.Il + " : plus aucun service en échec",
			Texte: "relevé reçu à " + v.Recu}
	}
	return &resultatVerification{Classe: "attention", Titre: "Vérifié " + v.Il + " : " + strings.Join(echecs, ", ") + " toujours en échec",
		Texte: "relevé reçu à " + v.Recu + " · voir ses logs pour la cause"}
}

// resultatAlerte : résultat pour la page d'une alerte (fermée ou non).
func resultatAlerte(v *vueVerif, enCours bool, quoi string) *resultatVerification {
	if v == nil || v.Etat == "" {
		return nil
	}
	switch v.Etat {
	case "attente":
		return &resultatVerification{Classe: "info", Titre: "Vérification demandée " + v.Il,
			Texte: "l'agent de " + v.Machine + " fait un relevé ; la page se met à jour seule", Recharger: true}
	case "sans-reponse":
		return &resultatVerification{Classe: "attention", Titre: "Pas de réponse de l'agent (" + v.Il + ")",
			Texte: "il se vérifiera à son rythme habituel (1 min)"}
	}
	if !enCours {
		return &resultatVerification{Classe: "ok", Titre: "Réparé : alerte fermée",
			Texte: "relevé de " + v.Machine + " reçu à " + v.Recu}
	}
	return &resultatVerification{Classe: "attention", Titre: "Toujours en cours : " + quoi,
		Texte: "relevé de " + v.Machine + " reçu à " + v.Recu + " · l'alerte se fermera seule une fois réparé"}
}
