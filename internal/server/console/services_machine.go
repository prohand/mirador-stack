// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
)

// Bloc « Services » de la page d'une machine (SPEC.md § 42.2, idée reprise
// de Beszel le 17/11 : idée seulement, aucun code repris).
//
// Tout vient des séries que l'agent envoie déjà, rien de nouveau n'est
// collecté :
//   - node_systemd_unit_state{state="failed"} : chaque service chargé, 1 s'il
//     est en échec ;
//   - node_systemd_unit_enabled_active : services démarrés au boot (1 s'il
//     tourne) ;
//   - node_systemd_service_restart_total : redémarrages.
//
// L'agent n'envoie pas l'état « actif » de chaque service (une série de plus
// par unité, écartée le 09/11) : un service qui n'est ni en échec ni lancé
// au boot est montré « À la demande » (minuterie, socket, lancé à la main).
// Coût : 3 requêtes instantanées à l'ouverture de la page.

// prefixesServicesBase : services du système, cachés (même règle que les
// propositions de surveillance, SPEC § 19.2).
var prefixesServicesBase = []string{"systemd-", "getty@", "serial-getty@", "user@", "user-runtime-dir@", "dbus", "modprobe@", "autovt@"}

func serviceDeBase(nom string) bool {
	for _, p := range prefixesServicesBase {
		if strings.HasPrefix(nom, p) {
			return true
		}
	}
	return false
}

// ligneService : un service de la machine.
type ligneService struct {
	Nom          string
	Etat, Classe string // En échec / Actif / Arrêté / À la demande
	AuDemarrage  bool
	Redemarrages int
	Surveille    bool
	LienLogs     string
	Groupes      string // filtres des onglets : « echec demarrage surveille »
}

// vueServices : le bloc replié ou ouvert.
type vueServices struct {
	Lignes                                  []ligneService
	NbEchec, NbActifs, NbArretes, NbDemande int
	NbDemarrage, NbSurveilles, NbCaches     int
	NbARegarder                             int // en échec + arrêtés (onglet « À regarder »)
	// « Vérifier maintenant » (SPEC § 127) : bouton des lignes en échec et
	// résultat de la dernière vérification.
	Verif    *vueVerif
	Resultat *resultatVerification
}

func (c *Console) servicesMachine(ctx context.Context, m flotte.InfoMachine, surv vueSurveillance, t time.Time) *vueServices {
	if c.lecteur == nil || !reValeurVariable.MatchString(m.Nom) {
		return nil
	}
	i := `instance="` + m.Nom + `"`
	lire := func(expr string) map[string]float64 {
		out := map[string]float64{}
		ech, err := c.instant(ctx, expr, t)
		if err != nil {
			return out
		}
		for _, e := range ech {
			if n := e.Etiquettes.Get("name"); n != "" && !math.IsNaN(e.Valeur) {
				out[n] = e.Valeur
			}
		}
		return out
	}
	echecs := lire(`node_systemd_unit_state{` + i + `, state="failed", name=~".+\\.service"}`)
	if len(echecs) == 0 {
		return nil // pas de systemd, ou agent qui ne l'envoie pas
	}
	demarrage := lire(`node_systemd_unit_enabled_active{` + i + `}`)
	redem := lire(`round(increase(node_systemd_service_restart_total{` + i + `}[24h]))`)
	surveilles := map[string]bool{}
	for _, s := range surv.SvcSurveilles {
		surveilles[s.Nom] = true
	}

	v := &vueServices{}
	for nom, echec := range echecs {
		if serviceDeBase(nom) && echec != 1 { // un service de base en échec reste montré
			v.NbCaches++
			continue
		}
		l := ligneService{Nom: nom, Surveille: surveilles[nom], Redemarrages: int(redem[nom])}
		tourne, auBoot := demarrage[nom]
		l.AuDemarrage = auBoot
		var groupes []string
		switch {
		case echec == 1:
			l.Etat, l.Classe = "En échec", "erreur"
			v.NbEchec++
			groupes = append(groupes, "echec")
		case auBoot && tourne == 1:
			l.Etat, l.Classe = "Actif", "ok"
			v.NbActifs++
		case auBoot:
			l.Etat, l.Classe = "Arrêté", "attention"
			v.NbArretes++
			groupes = append(groupes, "echec") // à regarder aussi
		default:
			l.Etat = "À la demande"
			v.NbDemande++
		}
		if l.AuDemarrage {
			v.NbDemarrage++
			groupes = append(groupes, "demarrage")
		}
		if l.Surveille {
			v.NbSurveilles++
			groupes = append(groupes, "surveille")
		}
		l.Groupes = strings.Join(groupes, " ")
		q := url.Values{"host": {m.Nom}, "unit": {nom}}
		l.LienLogs = "/logs?" + q.Encode()
		v.Lignes = append(v.Lignes, l)
	}
	v.NbARegarder = v.NbEchec + v.NbArretes
	v.Verif = c.verifMachine(m, t)
	var enEchec []string
	for _, l := range v.Lignes {
		if l.Classe == "erreur" {
			enEchec = append(enEchec, l.Nom)
		}
	}
	sort.Strings(enEchec)
	v.Resultat = resultatServices(v.Verif, enEchec)
	// Problèmes en premier, puis services du boot, puis par nom.
	rang := func(l ligneService) int {
		switch l.Classe {
		case "erreur":
			return 0
		case "attention":
			return 1
		}
		if l.AuDemarrage {
			return 2
		}
		return 3
	}
	sort.Slice(v.Lignes, func(a, b int) bool {
		if ra, rb := rang(v.Lignes[a]), rang(v.Lignes[b]); ra != rb {
			return ra < rb
		}
		return v.Lignes[a].Nom < v.Lignes[b].Nom
	})
	return v
}
