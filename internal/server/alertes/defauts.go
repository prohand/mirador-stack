// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

// Règles fournies par mirador (SPEC.md § 4.6 bis et § 17.3), posées une seule
// fois : une règle que l'utilisateur supprime ne revient pas au redémarrage.
// Les métriques mirador_* sont écrites par le serveur lui-même chaque minute
// (paquet interne), sous instance="mirador-server".

// ReglesMirador : règles par défaut.
func ReglesMirador() []Regle {
	m := func(r Regle) Regle {
		r.Origine, r.Active = "mirador", true
		if r.Intervalle == 0 {
			r.Intervalle = Duree(time.Minute)
		}
		return r
	}
	return []Regle{
		m(Regle{Nom: "OomKiller", Groupe: "hote", Intervalle: Duree(30 * time.Second),
			Expr:    `increase(node_vmstat_oom_kill[5m]) > 0`,
			Gravite: Critique,
			Resume:  "Le noyau a tué {{ $value | humanize }} processus par manque de mémoire sur {{ $labels.instance }}",
			Description: "Compteur oom_kill du noyau (/proc/vmstat). Le processus tué est écrit dans le journal " +
				"(« Out of memory: Killed process »).",
			Note: "SPEC § 4.6 bis : activée par défaut"}),
		m(Regle{Nom: "SauvegardeTropAncienne", Groupe: "mirador",
			Expr:    `time() - mirador_sauvegarde_derniere_reussie_timestamp_seconds > 26 * 3600`,
			Pour:    Duree(10 * time.Minute),
			Gravite: Avertissement,
			Resume:  "Dernière sauvegarde réussie il y a {{ $value | humanizeDuration }}",
			Description: "Aucune sauvegarde réussie depuis plus de 26 h (planifiée chaque nuit). Voir la commande " +
				"« sauvegardes » du serveur."}),
		m(Regle{Nom: "SauvegardeEnEchec", Groupe: "mirador",
			Expr:    `mirador_sauvegarde_derniere_en_echec == 1`,
			Gravite: Avertissement,
			Resume:  "La dernière sauvegarde a échoué",
			Description: "Le détail de l'erreur est dans l'historique (commande « sauvegardes »). L'alerte se termine " +
				"à la prochaine sauvegarde réussie."}),
		m(Regle{Nom: "ServeurDisqueSeRemplitVite", Groupe: "mirador", Intervalle: Duree(5 * time.Minute),
			Expr:    `predict_linear(mirador_disque_libre_octets[6h], 3 * 24 * 3600) < 0`,
			Pour:    Duree(30 * time.Minute),
			Gravite: Avertissement,
			Resume:  "Au rythme des 6 dernières heures, le disque du serveur sera plein dans moins de 3 jours",
			Description: "Le disque se remplit plus vite que prévu (brief, conservation des données). Vérifier la page " +
				"« Conservation des données » : plafonds de taille, volume de logs par jour."}),
		m(Regle{Nom: "HorlogeDecalee", Groupe: "mirador",
			Expr:    `abs(mirador_machine_decalage_horloge_secondes) > 60`,
			Pour:    Duree(10 * time.Minute),
			Gravite: Avertissement,
			Resume:  "L'horloge de {{ $labels.instance }} est décalée de {{ $value | humanizeDuration }}",
			Description: "Écart mesuré à chaque envoi de l'agent. Au-delà de 10 min, les données sont refusées. " +
				"Vérifier NTP (timedatectl) sur la machine."}),
		m(Regle{Nom: "MiseAJourAgentEchouee", Groupe: "mirador",
			Expr:    `mirador_machine_maj_echec == 1`,
			Gravite: Avertissement,
			Resume:  "La mise à jour de l'agent a échoué sur {{ $labels.instance }} (version {{ $labels.version }})",
			Description: "L'agent est revenu à sa version précédente. Cette version ne sera pas retentée sur cette " +
				"machine."}),
		m(Regle{Nom: "ServeurDisquePresquePlein", Groupe: "mirador",
			Expr:        `mirador_disque_libre_octets / mirador_disque_total_octets < 0.10`,
			Pour:        Duree(10 * time.Minute),
			Gravite:     Critique,
			Resume:      "Disque des données de mirador-server : moins de 10 % libre",
			Description: "Réduire la rétention ou le plafond de stockage, ou agrandir le disque du VPS."}),
		m(Regle{Nom: "EvaluationAlertesLente", Groupe: "mirador",
			Expr:    `mirador_alertes_duree_cycle_secondes > 10`,
			Pour:    Duree(15 * time.Minute),
			Gravite: Avertissement,
			Resume:  "L'évaluation des règles d'alerte prend {{ $value | humanize }} s par minute",
			Description: "Une règle lit trop de données (fenêtre longue sur beaucoup de séries). Voir la " +
				"commande « regles » (durée par règle)."}),
	}
}

const cleDefautsPoses = "alertes.defauts_poses"

// PoserDefauts ajoute les règles mirador jamais posées (une seule fois chacune).
func PoserDefauts(ctx context.Context, db *sql.DB, maintenant time.Time) (int, error) {
	poses := map[string]bool{}
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleDefautsPoses).Scan(&v)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	if v != "" {
		var noms []string
		if err := json.Unmarshal([]byte(v), &noms); err != nil {
			return 0, err
		}
		for _, n := range noms {
			poses[n] = true
		}
	}
	existantes, err := ListerRegles(ctx, db)
	if err != nil {
		return 0, err
	}
	deja := map[string]bool{}
	for _, r := range existantes {
		deja[r.Nom] = true
	}
	var nouvelles []Regle
	for _, r := range ReglesMirador() {
		if !poses[r.Nom] && !deja[r.Nom] {
			nouvelles = append(nouvelles, r)
		}
		poses[r.Nom] = true
	}
	n, err := Enregistrer(ctx, db, nouvelles, "mirador", maintenant)
	if err != nil {
		return 0, err
	}
	noms := make([]string, 0, len(poses))
	for k := range poses {
		noms = append(noms, k)
	}
	sort.Strings(noms)
	brut, _ := json.Marshal(noms)
	_, err = db.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, 'mirador')
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le`,
		cleDefautsPoses, string(brut), maintenant.Unix())
	return n, err
}
