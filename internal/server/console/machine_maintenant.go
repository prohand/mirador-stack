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
	// Cle : place dans le bloc (cpu, mem, net, disque, plein, swap, charge,
	// demarree) ; Seuil : seuil du modèle (%, ligne pointillée de la courbe en
	// direct, 0 : aucun) ; SeuilTexte : « seuil 90 % ».
	Cle        string
	Seuil      float64
	SeuilTexte string
}

// blocMaintenant : « En ce moment » de la page machine (retour du 20/11 :
// « En direct » et « En ce moment » réunis). Quatre cases avec courbe en
// direct (processeur, mémoire, réseau, disques), puis une ligne (disque le
// plus plein, charge par cœur, swap). Sans direct, les cases gardent les
// valeurs enregistrées (moyenne des 5 dernières minutes), sans courbe.
type blocMaintenant struct {
	Nom    string
	Direct string // "ok" : direct possible ; "ancien" : agent trop ancien ; "" : pas d'agent
	Cases  []jaugeMachine
	Bas    []jaugeMachine
}

// blocEnCeMoment range les jauges dans le bloc (nil : rien à montrer).
func blocEnCeMoment(nom, direct string, js []jaugeMachine) *blocMaintenant {
	if len(js) == 0 && direct == "" {
		return nil // rien à montrer (un agent trop ancien garde son message)
	}
	b := &blocMaintenant{Nom: nom, Direct: direct}
	par := map[string]jaugeMachine{}
	for _, j := range js {
		par[j.Cle] = j
	}
	// Les quatre cases sont toujours là quand le direct est possible (il les
	// remplit) ; sinon, seulement celles qui ont une valeur.
	vides := map[string]jaugeMachine{
		"cpu":    {Titre: "Processeur", Lien: "cpu", Valeur: "—", Pct: -1, Classe: "ok"},
		"mem":    {Titre: "Mémoire", Lien: "memoire", Valeur: "—", Pct: -1, Classe: "ok"},
		"net":    {Titre: "Réseau", Lien: "reseau", Valeur: "—", Pct: -1, Classe: "ok"},
		"disque": {Titre: "Disques (lecture / écriture)", Lien: "disques", Valeur: "—", Pct: -1, Classe: "ok"},
	}
	for _, k := range []string{"cpu", "mem", "net", "disque"} {
		j, ok := par[k]
		if !ok && direct != "ok" {
			continue
		}
		if !ok {
			j = vides[k]
		}
		j.Cle = k
		b.Cases = append(b.Cases, j)
	}
	for _, k := range []string{"plein", "charge", "swap"} {
		if j, ok := par[k]; ok {
			b.Bas = append(b.Bas, j)
		}
	}
	return b
}

// interfacesPhysiques / disquesEntiers : comme le direct (SPEC § 97) :
// cartes réseau physiques (ponts, VLAN, liens virtuels et tunnels exclus,
// sinon le trafic est compté deux fois) et disques entiers (pas les
// partitions ni les volumes).
const (
	interfacesPhysiques = `device!~"lo|veth.*|docker.*|br-.*|virbr.*|tap.*|fwbr.*|fwpr.*|fwln.*|vmbr.*|bond.*|vlan.*|.*\\..*|wg.*|tun.*|tailscale.*|cali.*|flannel.*|cni.*"`
	disquesEntiers      = `device=~"(sd|vd|xvd|hd)[a-z]+|nvme[0-9]+n[0-9]+|mmcblk[0-9]+"`
)

// debitLisible : octets/s → « 35 Mo/s ».
func debitLisible(v float64) string {
	if v < 1024 {
		return fmt.Sprintf("%.0f o/s", v)
	}
	return virgule(tailleLisible(int64(v))) + "/s"
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
		j := jaugeMachine{Cle: "cpu", Titre: "Processeur", Lien: "cpu", Valeur: fmt.Sprintf("%.0f %%", v), Pct: pctBarre(v), Classe: classeSeuil(v, s["cpu_pct"]),
			Seuil: s["cpu_pct"], SeuilTexte: fmt.Sprintf("seuil %.0f %%", s["cpu_pct"])}
		if n, ok := une(`count by (instance) (node_cpu_seconds_total{` + i + `, mode="idle"})`); ok {
			j.Detail = fmt.Sprintf("%.0f cœurs", n)
		}
		out = append(out, j)
	}
	if v, ok := une(`100 * (1 - node_memory_MemAvailable_bytes{` + i + `} / node_memory_MemTotal_bytes{` + i + `})`); ok {
		j := jaugeMachine{Cle: "mem", Titre: "Mémoire", Lien: "memoire", Valeur: fmt.Sprintf("%.0f %%", v), Pct: pctBarre(v), Classe: classeSeuil(v, 100-s["memoire_libre_pct"]),
			Seuil: 100 - s["memoire_libre_pct"], SeuilTexte: fmt.Sprintf("seuil %.0f %% (%.0f %% libre)", 100-s["memoire_libre_pct"], s["memoire_libre_pct"])}
		if tot, ok := une(`node_memory_MemTotal_bytes{` + i + `}`); ok && tot > 0 {
			j.Detail = virgule(tailleLisible(int64(tot*v/100))) + " / " + virgule(tailleLisible(int64(tot)))
		}
		out = append(out, j)
	}
	// Réseau et disques : moyenne des 5 dernières minutes (le direct, s'il
	// tourne, montre ensuite la seconde en cours). Mêmes interfaces et disques
	// que le direct : cartes physiques, disques entiers.
	if rx, ok := une(`sum by (instance) (rate(node_network_receive_bytes_total{` + i + `, ` + interfacesPhysiques + `}[5m]))`); ok {
		tx, _ := une(`sum by (instance) (rate(node_network_transmit_bytes_total{` + i + `, ` + interfacesPhysiques + `}[5m]))`)
		out = append(out, jaugeMachine{Cle: "net", Titre: "Réseau", Lien: "reseau", Valeur: "↓ " + debitLisible(rx), Detail: "↑ " + debitLisible(tx), Pct: -1, Classe: "ok"})
	}
	if lu, ok := une(`sum by (instance) (rate(node_disk_read_bytes_total{` + i + `, ` + disquesEntiers + `}[5m]))`); ok {
		ec, _ := une(`sum by (instance) (rate(node_disk_written_bytes_total{` + i + `, ` + disquesEntiers + `}[5m]))`)
		out = append(out, jaugeMachine{Cle: "disque", Titre: "Disques (lecture / écriture)", Lien: "disques", Valeur: "lu " + debitLisible(lu), Detail: "écrit " + debitLisible(ec), Pct: -1, Classe: "ok"})
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
			out = append(out, jaugeMachine{Cle: "plein", Titre: "Disque le plus plein", Lien: "disques", Valeur: fmt.Sprintf("%.0f %%", max), Pct: pctBarre(max),
				Classe: classeSeuil(max, 100-s["disque_libre_pct"]), Detail: detail})
		}
	}
	if total, ok := une(`node_memory_SwapTotal_bytes{` + i + `}`); ok {
		if total <= 0 {
			out = append(out, jaugeMachine{Cle: "swap", Titre: "Swap", Valeur: "—", Pct: -1, Classe: "ok", Detail: "pas de swap"})
		} else if libre, ok := une(`node_memory_SwapFree_bytes{` + i + `}`); ok {
			v := 100 * (1 - libre/total)
			detail := "pas utilisé"
			if v >= 1 {
				detail = virgule(tailleLisible(int64(total-libre))) + " sur " + virgule(tailleLisible(int64(total)))
			}
			out = append(out, jaugeMachine{Cle: "swap", Titre: "Swap", Lien: "memoire", Valeur: fmt.Sprintf("%.0f %%", v), Pct: pctBarre(v), Classe: classeSeuil(v, 50), Detail: detail})
		}
	}
	if v, ok := une(`max by (instance) (node_load5{` + i + `}) / count by (instance) (node_cpu_seconds_total{` + i + `, mode="idle"})`); ok {
		out = append(out, jaugeMachine{Cle: "charge", Titre: "Charge par cœur", Lien: "cpu", Valeur: virgule(fmt.Sprintf("%.2f", v)), Pct: pctBarre(100 * v / math.Max(s["charge_par_coeur"], 0.01)),
			Classe: classeSeuil(v, s["charge_par_coeur"]), Detail: virgule(fmt.Sprintf("seuil %.1f", s["charge_par_coeur"]))})
	}
	if v, ok := une(`node_time_seconds{` + i + `} - node_boot_time_seconds{` + i + `}`); ok {
		detail := ""
		if m.Version != "" {
			detail = "agent " + m.Version
		}
		out = append(out, jaugeMachine{Cle: "demarree", Titre: "Démarrée depuis", Valeur: dureeLisible(time.Duration(v) * time.Second), Pct: -1, Classe: "ok", Detail: detail})
	}
	return out
}
