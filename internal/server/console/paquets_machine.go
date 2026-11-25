// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"math"
	"sort"
	"time"

	"mirador-stack/internal/server/flotte"
)

// Bloc « Paquets à mettre à jour » de la page d'une machine (SPEC.md
// § 42.3, idée reprise de Beszel le 17/11 : idée seulement). L'agent
// (à partir de la prochaine version) envoie un paquet par série apt_upgrade_package{package,
// from, to, security}, 200 au plus ; les plus anciens n'envoient que le
// nombre (apt_upgrades_pending). Coût : 4 requêtes instantanées à
// l'ouverture de la page.

type lignePaquet struct {
	Nom, Avant, Apres string
	Securite          bool
}

type vuePaquets struct {
	Lignes      []lignePaquet
	Nombre      int  // apt_upgrades_pending
	NbSecurite  int  // apt_upgrades_security_pending
	Redemarrage bool // node_reboot_required
	SansListe   bool // pas de liste reçue (voir Echec, AgentRecent)
	// Retour du 25/11 (SPEC § 127) : le message « agent trop ancien » était
	// faux. Echec : motif du dernier calcul raté (envoyé par l'agent) ;
	// AgentRecent : l'agent envoie l'heure de ses calculs (sait faire la
	// liste) ; Calcule : âge du dernier calcul réussi (« 9 h »).
	Echec       string
	AgentRecent bool
	Calcule     string
	Tronquee    bool // plus de paquets que de lignes reçues
	Ouvrir      bool // ouvert d'office (alerte de mises à jour de sécurité en cours)
}

// maxPaquetsAgent : plafond de l'agent (internal/agent/collect).
const maxPaquetsAgent = 200

func (c *Console) paquetsMachine(ctx context.Context, m flotte.InfoMachine, t time.Time) *vuePaquets {
	if c.lecteur == nil || !reValeurVariable.MatchString(m.Nom) {
		return nil
	}
	i := `{instance="` + m.Nom + `"}`
	une := func(expr string) (float64, bool) {
		ech, err := c.instant(ctx, expr, t)
		if err != nil || len(ech) == 0 || math.IsNaN(ech[0].Valeur) {
			return 0, false
		}
		return ech[0].Valeur, true
	}
	n, ok := une(`last_over_time(apt_upgrades_pending` + i + `[3h])`)
	if !ok || n <= 0 {
		return nil // pas une Debian/Ubuntu, ou paquets à jour (dit dans le bandeau)
	}
	v := &vuePaquets{Nombre: int(n)}
	if s, ok := une(`last_over_time(apt_upgrades_security_pending` + i + `[3h])`); ok {
		v.NbSecurite = int(s)
	}
	if r, ok := une(`node_reboot_required` + i); ok && r == 1 {
		v.Redemarrage = true
	}
	if ech, err := c.instant(ctx, `last_over_time(apt_upgrade_package`+i+`[10m])`, t); err == nil {
		for _, e := range ech {
			v.Lignes = append(v.Lignes, lignePaquet{Nom: e.Etiquettes.Get("package"), Avant: e.Etiquettes.Get("from"),
				Apres: e.Etiquettes.Get("to"), Securite: e.Etiquettes.Get("security") == "1"})
		}
	}
	// Sécurité d'abord, puis par nom.
	sort.Slice(v.Lignes, func(a, b int) bool {
		if v.Lignes[a].Securite != v.Lignes[b].Securite {
			return v.Lignes[a].Securite
		}
		return v.Lignes[a].Nom < v.Lignes[b].Nom
	})
	v.SansListe = len(v.Lignes) == 0
	if ech, err := c.instant(ctx, `last_over_time(mirador_agent_mises_a_jour_echec`+i+`[10m])`, t); err == nil && len(ech) > 0 {
		v.Echec = ech[0].Etiquettes.Get("raison")
	}
	if le, ok := une(`last_over_time(apt_upgrades_calcule_le` + i + `[3h])`); ok && le > 0 {
		v.AgentRecent = true
		if d := t.Sub(time.Unix(int64(le), 0)); d >= time.Minute {
			v.Calcule = dureeLisible(d.Round(time.Minute))
		}
	}
	v.Tronquee = len(v.Lignes) >= maxPaquetsAgent && v.Nombre > len(v.Lignes)
	return v
}
