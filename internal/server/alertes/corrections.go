// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"database/sql"
	"strings"
	"time"
)

// Corrections d'expressions connues (SPEC.md § 45).
//
// Certaines règles reprises de vmalert ont un défaut qu'on ne voit qu'à
// l'usage. Leur expression exacte est remplacée par une version corrigée :
//   - à l'import d'un fichier vmalert (convertir) ;
//   - au démarrage du serveur, pour les règles déjà en base (CorrigerRegles).
// Seule l'expression d'origine, au caractère près (espaces mis à part), est
// remplacée : une règle déjà modifiée par l'utilisateur n'est pas touchée.

// MetriqueDerniereReception : heure (secondes Unix) des dernières métriques
// reçues de chaque machine, écrite chaque minute par le serveur (interne.go).
const MetriqueDerniereReception = "mirador_machine_derniere_metrique_timestamp_seconds"

type correction struct {
	nouvelle, note string
}

// correctionsExpr : expression d'origine (espaces réduits) → correction.
var correctionsExpr = map[string]correction{
	// AgentMuet (défaut vu le 18/11, coupure du 16/11) : lag() ne voit que
	// les points des dernières 24 h. Après 24 h sans aucune donnée, la série
	// sort de la fenêtre, lag() ne renvoie plus rien et l'alerte se ferme
	// toute seule, machine toujours muette (faux « Résolu », fin de coupure
	// fausse). Correction : au-delà de 23 h, on lit l'heure des dernières
	// métriques tenue par le serveur, qui ne sort jamais de la fenêtre.
	//   - 1re partie inchangée : alerte en 1 à 2 minutes, comme avant ;
	//   - 2e partie : prend le relais avant la fin de la 1re (1 h de marge),
	//     mêmes étiquettes ({instance}) donc la même alerte continue ;
	//   - machine qui répond : 2e partie fausse (dernière réception < 2 min).
	// Coût : une série par machine, un point par minute ; rien à relire sur 24 h.
	"min by (instance) (lag(node_uname_info[1d])) > 60": {
		nouvelle: "min by (instance) (lag(node_uname_info[1d])) > 60\nor on (instance)\nmax by (instance) (time() - " +
			MetriqueDerniereReception + ") > 82800",
		note: "expression corrigée par mirador (SPEC § 45) : l'alerte ne se ferme plus seule après 24 h sans données",
	},
}

// correctionsPour : règles mirador posées sans durée « pendant » alors
// qu'il en faut une (expression d'origine → durée et note). Appliqué
// seulement si la règle n'a toujours aucune durée (pas modifiée).
var correctionsPour = map[string]struct {
	pour Duree
	note string
}{
	// MiseAJourAgentEchouee (vu le 19/11, proxmoxbackup) : un agent signale
	// aussi « vérification impossible » (HTTP 502 du proxy pendant un
	// redémarrage du serveur) ; corrigé au prochain essai, 15 min après.
	// 30 min : une coupure courte ne fait plus d'alerte ; un vrai échec
	// (retour arrière) dure, lui, jusqu'à la version suivante.
	"mirador_machine_maj_echec == 1": {Duree(30 * time.Minute),
		"durée « pendant » ajoutée par mirador (SPEC § 61) : une vérification ratée une fois ne fait plus d'alerte"},
}

// normaliserExpr : espaces et retours à la ligne réduits à un espace.
func normaliserExpr(expr string) string {
	return strings.Join(strings.Fields(expr), " ")
}

// corrigerExpr renvoie l'expression corrigée et la note, si l'expression
// est une de celles à corriger.
func corrigerExpr(expr string) (string, string, bool) {
	c, ok := correctionsExpr[normaliserExpr(expr)]
	return c.nouvelle, c.note, ok
}

// CorrigerRegles applique les corrections aux règles déjà en base
// (démarrage du serveur). Renvoie le nombre de règles corrigées ; trace
// dans le journal d'audit (Enregistrer).
func CorrigerRegles(ctx context.Context, db *sql.DB, maintenant time.Time) (int, error) {
	regles, err := ListerRegles(ctx, db)
	if err != nil {
		return 0, err
	}
	var aCorriger []Regle
	ajouterNote := func(r *Regle, note string) {
		if r.Note == "" {
			r.Note = note
		} else {
			r.Note += " ; " + note
		}
	}
	for _, r := range regles {
		corrigee := false
		if nouvelle, note, ok := corrigerExpr(r.Expr); ok {
			r.Expr = nouvelle
			ajouterNote(&r, note)
			corrigee = true
		}
		if c, ok := correctionsPour[normaliserExpr(r.Expr)]; ok && r.Pour == 0 {
			r.Pour = c.pour
			ajouterNote(&r, c.note)
			corrigee = true
		}
		if corrigee {
			aCorriger = append(aCorriger, r)
		}
	}
	if len(aCorriger) == 0 {
		return 0, nil
	}
	return Enregistrer(ctx, db, aCorriger, "mirador", maintenant)
}

// --- Règles renommées (SPEC § 65) -------------------------------------------------

// textesApplication : résumé et description des règles des applications.
var textesApplication = map[string][2]string{
	"ApplicationEnPanne": {"Application {{ $labels.service }} en panne",
		"Un élément « clé » de l'application a un problème (machine muette, conteneur arrêté ou non sain, " +
			"sonde en échec). Détail sur la page Applications."},
	"ApplicationDegradee": {"Application {{ $labels.service }} dégradée",
		"Un élément de l'application a un problème, mais l'application marche encore. Détail sur la page Applications."},
}

// reglesRenommees : ancien nom → nouveau, avec les textes d'origine (remplacés
// seulement s'ils n'ont pas été changés par l'utilisateur).
var reglesRenommees = []struct {
	ancien, nouveau               string
	resumeAvant, descriptionAvant string
}{
	{"ServiceEnPanne", "ApplicationEnPanne", "Service {{ $labels.service }} en panne",
		"Un élément « clé » du service a un problème (machine muette, conteneur arrêté ou non sain, sonde en échec). Détail sur la page Services."},
	{"ServiceDegrade", "ApplicationDegradee", "Service {{ $labels.service }} dégradé",
		"Un élément du service a un problème, mais le service marche encore. Détail sur la page Services."},
}

// RenommerRegles : règles déjà en base sous leur ancien nom (« Services »
// renommés « Applications » le 19/11) passées sous le nouveau, réglages de
// l'utilisateur gardés (activation, durée, gravité, expression). À appeler
// avant PoserDefauts (sinon la règle serait posée une seconde fois). Les
// alertes passées gardent l'ancien nom ; une alerte en cours est fermée et
// rouverte sous le nouveau nom au cycle suivant.
func RenommerRegles(ctx context.Context, db *sql.DB, maintenant time.Time) (int, error) {
	regles, err := ListerRegles(ctx, db)
	if err != nil {
		return 0, err
	}
	parNom := map[string]Regle{}
	for _, r := range regles {
		parNom[r.Nom] = r
	}
	n := 0
	for _, x := range reglesRenommees {
		r, ok := parNom[x.ancien]
		if _, deja := parNom[x.nouveau]; !ok || deja {
			continue
		}
		r.Nom = x.nouveau
		if r.Resume == x.resumeAvant {
			r.Resume = textesApplication[x.nouveau][0]
		}
		if r.Description == x.descriptionAvant {
			r.Description = textesApplication[x.nouveau][1]
		}
		note := "renommée par mirador (SPEC § 65) : ancien nom " + x.ancien
		if r.Note == "" {
			r.Note = note
		} else {
			r.Note += " ; " + note
		}
		if _, err := Enregistrer(ctx, db, []Regle{r}, "mirador", maintenant); err != nil {
			return n, err
		}
		if err := Supprimer(ctx, db, x.ancien, "mirador", maintenant); err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}
