// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"fmt"
	"math"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/modeles"
)

// « En ce moment » sur la page d'une machine (SPEC § 29) : CPU, mémoire,
// disque le plus plein, swap, charge par cœur, durée depuis le démarrage ;
// couleur selon le seuil du modèle Linux de la machine (sinon ses valeurs
// par défaut) : vert, orange à 80 % du seuil, rouge au seuil.

// jaugeMachine : une jauge du résumé.
type jaugeMachine struct {
	Titre, Valeur, Detail string
	Lien                  string // panneau de détail au clic : cpu, memoire, disques (SPEC § 42.4)
	Pct                   int    // largeur de la barre (0 à 100) ; -1 = pas de barre
	Classe                string // ok, attention, erreur
}

// fsIgnoresMaintenant : systèmes de fichiers sans intérêt (comme les tableaux).
const fsIgnoresMaintenant = `fstype!~"tmpfs|overlay|squashfs|ramfs|nsfs|fuse.lxcfs"`

// seuilsLinux : seuils du modèle Linux de la machine (défauts du modèle si
// elle ne le porte pas).
func (c *Console) seuilsLinux(ctx context.Context, m flotte.InfoMachine) map[string]float64 {
	out := map[string]float64{}
	if mod := modeles.Trouver("linux"); mod != nil {
		for _, p := range mod.Parametres {
			out[p.Nom] = p.Defaut
		}
	}
	if effs, err := modeles.Effectifs(ctx, c.db, m.ID, m.Groupe); err == nil {
		for _, e := range effs {
			if e.Modele.Nom == "linux" {
				for k, v := range e.Parametres {
					out[k] = v
				}
			}
		}
	}
	return out
}

// classeSeuil : couleur d'une valeur par rapport à son seuil.
func classeSeuil(v, seuil float64) string {
	switch {
	case seuil <= 0:
		return "ok"
	case v >= seuil:
		return "erreur"
	case v >= 0.8*seuil:
		return "attention"
	}
	return "ok"
}

func pctBarre(v float64) int {
	if math.IsNaN(v) {
		return 0
	}
	return int(math.Max(0, math.Min(100, math.Round(v))))
}

// enCeMoment : jauges de la machine (vide si l'agent n'envoie rien).
func (c *Console) enCeMoment(ctx context.Context, m flotte.InfoMachine, t time.Time) []jaugeMachine {
	if c.lecteur == nil || !reValeurVariable.MatchString(m.Nom) {
		return nil
	}
	i := `instance="` + m.Nom + `"`
	une := func(expr string) (float64, bool) {
		ech, err := c.instant(ctx, expr, t)
		if err != nil || len(ech) == 0 || math.IsNaN(ech[0].Valeur) || math.IsInf(ech[0].Valeur, 0) {
			return 0, false
		}
		return ech[0].Valeur, true
	}
	s := c.seuilsLinux(ctx, m)
	var out []jaugeMachine
	if v, ok := une(`100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{` + i + `, mode="idle"}[5m])))`); ok {
		out = append(out, jaugeMachine{Titre: "CPU", Lien: "cpu", Valeur: fmt.Sprintf("%.0f %%", v), Pct: pctBarre(v), Classe: classeSeuil(v, s["cpu_pct"]),
			Detail: fmt.Sprintf("seuil %.0f %%", s["cpu_pct"])})
	}
	if v, ok := une(`100 * (1 - node_memory_MemAvailable_bytes{` + i + `} / node_memory_MemTotal_bytes{` + i + `})`); ok {
		out = append(out, jaugeMachine{Titre: "Mémoire", Lien: "memoire", Valeur: fmt.Sprintf("%.0f %%", v), Pct: pctBarre(v), Classe: classeSeuil(v, 100-s["memoire_libre_pct"]),
			Detail: fmt.Sprintf("seuil : %.0f %% libre", s["memoire_libre_pct"])})
	}
	// Disque le plus plein : point de montage et place libre.
	if ech, err := c.instant(ctx, `100 * (1 - node_filesystem_avail_bytes{`+i+`, `+fsIgnoresMaintenant+`} / node_filesystem_size_bytes{`+i+`, `+fsIgnoresMaintenant+`})`, t); err == nil && len(ech) > 0 {
		max, mp := -1.0, ""
		for _, e := range ech {
			if !math.IsNaN(e.Valeur) && e.Valeur > max {
				max, mp = e.Valeur, e.Etiquettes.Get("mountpoint")
			}
		}
		if max >= 0 {
			detail := mp
			if libre, ok := une(`node_filesystem_avail_bytes{` + i + `, mountpoint="` + echapperPromQL(mp) + `", ` + fsIgnoresMaintenant + `}`); ok {
				detail += " · " + virgule(tailleLisible(int64(libre))) + " libres"
			}
			out = append(out, jaugeMachine{Titre: "Disque le plus plein", Lien: "disques", Valeur: fmt.Sprintf("%.0f %%", max), Pct: pctBarre(max),
				Classe: classeSeuil(max, 100-s["disque_libre_pct"]), Detail: detail})
		}
	}
	if total, ok := une(`node_memory_SwapTotal_bytes{` + i + `}`); ok {
		if total <= 0 {
			out = append(out, jaugeMachine{Titre: "Swap", Valeur: "—", Pct: -1, Classe: "ok", Detail: "pas de swap"})
		} else if libre, ok := une(`node_memory_SwapFree_bytes{` + i + `}`); ok {
			v := 100 * (1 - libre/total)
			detail := "pas utilisé"
			if v >= 1 {
				detail = virgule(tailleLisible(int64(total-libre))) + " sur " + virgule(tailleLisible(int64(total)))
			}
			out = append(out, jaugeMachine{Titre: "Swap", Lien: "memoire", Valeur: fmt.Sprintf("%.0f %%", v), Pct: pctBarre(v), Classe: classeSeuil(v, 50), Detail: detail})
		}
	}
	if v, ok := une(`max by (instance) (node_load5{` + i + `}) / count by (instance) (node_cpu_seconds_total{` + i + `, mode="idle"})`); ok {
		out = append(out, jaugeMachine{Titre: "Charge par cœur", Lien: "cpu", Valeur: virgule(fmt.Sprintf("%.2f", v)), Pct: pctBarre(100 * v / math.Max(s["charge_par_coeur"], 0.01)),
			Classe: classeSeuil(v, s["charge_par_coeur"]), Detail: virgule(fmt.Sprintf("seuil %.1f", s["charge_par_coeur"]))})
	}
	if v, ok := une(`node_time_seconds{` + i + `} - node_boot_time_seconds{` + i + `}`); ok {
		detail := ""
		if m.Version != "" {
			detail = "agent " + m.Version
		}
		out = append(out, jaugeMachine{Titre: "Démarrée depuis", Valeur: dureeLisible(time.Duration(v) * time.Second), Pct: -1, Classe: "ok", Detail: detail})
	}
	return out
}
