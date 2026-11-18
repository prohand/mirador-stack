// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"time"

	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/flotte"
)

// Bloc « Mise à jour » de la page machine (SPEC.md § 44, demande du 18/11) :
// bouton « Réessayer la mise à jour » (mise à jour en échec) ou « Mettre à
// jour maintenant » (version assignée pas encore installée). Voir
// flotte.ForcerMaj : la version reste celle assignée et signée ; la fenêtre
// horaire est ignorée et l'agent est réveillé tout de suite.
type vueMaj struct {
	Actuelle, Cible string // version de l'agent, version assignée (effective)
	Echec           string // dernier échec signalé par l'agent ("" : aucun)
	Capable         bool   // l'agent sait retenter une version en échec
	DemandeeLe      int64  // dernière demande (secondes Unix ; 0 : jamais)
	DemandeRecente  bool   // demande de moins de 24 h
	FichierEchec    string // fichier à effacer à la main (agent trop ancien)
}

// Afficher : il y a quelque chose à proposer (échec, ou version à installer).
func (v vueMaj) Afficher() bool { return v.Echec != "" || v.EnAttente() }

// EnAttente : une version est assignée mais pas encore installée.
func (v vueMaj) EnAttente() bool { return v.Cible != "" && v.Actuelle != "" && v.Cible != v.Actuelle }

func (c *Console) majMachine(ctx context.Context, m flotte.InfoMachine, t time.Time) vueMaj {
	v := vueMaj{Actuelle: m.Version, Cible: m.VersionCible, Echec: m.MajEchec,
		Capable: flotte.ACapacite(m.Capacites, protocole.CapaciteReessai)}
	if v.Cible == "" {
		v.Cible, _ = flotte.CibleGlobale(ctx, c.db)
	}
	_ = c.db.QueryRowContext(ctx, `SELECT maj_forcee_le FROM machines WHERE id = ?`, m.ID).Scan(&v.DemandeeLe)
	v.DemandeRecente = v.DemandeeLe > 0 && t.Sub(time.Unix(v.DemandeeLe, 0)) < flotte.DureeMajForcee
	v.FichierEchec = "/var/lib/mirador-agent/bin/echec.json"
	if m.Type == "collecteur" {
		v.FichierEchec = "/var/lib/mirador-collector/bin/echec.json"
	}
	return v
}
