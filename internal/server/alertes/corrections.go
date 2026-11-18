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
	for _, r := range regles {
		if nouvelle, note, ok := corrigerExpr(r.Expr); ok {
			r.Expr = nouvelle
			if r.Note == "" {
				r.Note = note
			} else {
				r.Note += " ; " + note
			}
			aCorriger = append(aCorriger, r)
		}
	}
	if len(aCorriger) == 0 {
		return 0, nil
	}
	return Enregistrer(ctx, db, aCorriger, "mirador", maintenant)
}
