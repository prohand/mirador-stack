// SPDX-License-Identifier: GPL-3.0-or-later

package taches

import (
	"sort"
	"strconv"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Lire : une requête instantanée (alertes.Lecteur.Instant ou cache de la
// console).
type Lire func(expr string) ([]alertes.Echantillon, error)

// Detectee : tâche vue dans les métriques des agents (suivie ou non).
type Detectee struct {
	Machine, Source, Nom string
	Releve               Releve
}

// Cle : clé de la tâche (même forme que Tache.Cle).
func (d Detectee) Cle() string { return CleAgent(d.Machine, d.Source, d.Nom) }

// Detectees : dernières exécutions envoyées par les agents, par clé.
func Detectees(lire Lire) map[string]*Detectee {
	out := map[string]*Detectee{}
	prendre := func(nom string, f func(d *Detectee, v float64, e alertes.Echantillon)) {
		ech, err := lire(nom)
		if err != nil {
			return
		}
		for _, e := range ech {
			m, src, t := e.Etiquettes.Get("instance"), e.Etiquettes.Get("source"), e.Etiquettes.Get("tache")
			if m == "" || src == "" || t == "" {
				continue
			}
			k := CleAgent(m, src, t)
			d := out[k]
			if d == nil {
				d = &Detectee{Machine: m, Source: src, Nom: t}
				out[k] = d
			}
			f(d, e.Valeur, e)
		}
	}
	date := func(v float64) time.Time {
		if v <= 0 {
			return time.Time{}
		}
		return time.Unix(int64(v), 0)
	}
	prendre("mirador_tache_fin_timestamp_seconds", func(d *Detectee, v float64, _ alertes.Echantillon) { d.Releve.Fin = date(v) })
	prendre("mirador_tache_debut_timestamp_seconds", func(d *Detectee, v float64, _ alertes.Echantillon) { d.Releve.Debut = date(v) })
	prendre("mirador_tache_prochaine_timestamp_seconds", func(d *Detectee, v float64, _ alertes.Echantillon) { d.Releve.Prochaine = date(v) })
	prendre("mirador_tache_code", func(d *Detectee, v float64, _ alertes.Echantillon) { d.Releve.Code = int(v) })
	prendre("mirador_tache_message", func(d *Detectee, _ float64, e alertes.Echantillon) { d.Releve.Message = e.Etiquettes.Get("message") })
	// Détail des sauvegardes Proxmox VE (SPEC § 89).
	prendre("mirador_tache_info", func(d *Detectee, _ float64, e alertes.Echantillon) {
		d.Releve.Stockage, d.Releve.Planning, d.Releve.Commentaire = e.Etiquettes.Get("stockage"), e.Etiquettes.Get("planning"), e.Etiquettes.Get("commentaire")
	})
	vms := map[string]map[string]*VMReleve{} // clé de tâche → vm → détail
	vm := func(d *Detectee, e alertes.Echantillon) *VMReleve {
		id := e.Etiquettes.Get("vm")
		if id == "" {
			return nil
		}
		k := d.Cle()
		if vms[k] == nil {
			vms[k] = map[string]*VMReleve{}
		}
		if vms[k][id] == nil {
			vms[k][id] = &VMReleve{ID: id}
		}
		return vms[k][id]
	}
	prendre("mirador_tache_vm", func(d *Detectee, v float64, e alertes.Echantillon) {
		if x := vm(d, e); x != nil {
			x.OK, x.Type, x.Nom = v == 1, e.Etiquettes.Get("type"), e.Etiquettes.Get("nom")
		}
	})
	prendre("mirador_tache_vm_duree_secondes", func(d *Detectee, v float64, e alertes.Echantillon) {
		if x := vm(d, e); x != nil {
			x.Duree = time.Duration(v) * time.Second
		}
	})
	prendre("mirador_tache_vm_octets", func(d *Detectee, v float64, e alertes.Echantillon) {
		if x := vm(d, e); x != nil {
			x.Octets = v
		}
	})
	prendre("mirador_tache_vm_message", func(d *Detectee, _ float64, e alertes.Echantillon) {
		if x := vm(d, e); x != nil {
			x.Message = e.Etiquettes.Get("message")
		}
	})
	type erreurRangee struct {
		rang  int
		texte string
	}
	erreurs := map[string][]erreurRangee{}
	prendre("mirador_tache_erreur", func(d *Detectee, _ float64, e alertes.Echantillon) {
		r, _ := strconv.Atoi(e.Etiquettes.Get("rang"))
		erreurs[d.Cle()] = append(erreurs[d.Cle()], erreurRangee{r, e.Etiquettes.Get("message")})
	})
	for k, l := range erreurs {
		sort.Slice(l, func(i, j int) bool { return l[i].rang < l[j].rang })
		if d := out[k]; d != nil {
			for _, x := range l {
				d.Releve.Erreurs = append(d.Releve.Erreurs, x.texte)
			}
		}
	}
	for k, parVM := range vms {
		d := out[k]
		if d == nil {
			continue
		}
		for _, x := range parVM {
			d.Releve.VMs = append(d.Releve.VMs, *x)
		}
		sort.Slice(d.Releve.VMs, func(i, j int) bool {
			a, b := d.Releve.VMs[i], d.Releve.VMs[j]
			if a.OK != b.OK {
				return !a.OK // échecs d'abord
			}
			ia, ea := strconv.Atoi(a.ID)
			ib, eb := strconv.Atoi(b.ID)
			if ea == nil && eb == nil {
				return ia < ib
			}
			return a.ID < b.ID
		})
	}
	return out
}

// AvecReleves : les tâches suivies, avec leur dernière exécution (ping :
// dans la base ; agent : dans les métriques).
func AvecReleves(ts []Tache, det map[string]*Detectee) []Tache {
	for i := range ts {
		if ts[i].Source != SourcePing {
			if d := det[ts[i].Cle]; d != nil {
				ts[i].Releve = d.Releve
			} else {
				ts[i].Releve = Releve{}
			}
		}
	}
	return ts
}

// SerieInterne : une série écrite par le serveur chaque minute.
type SerieInterne struct {
	Nom        string
	Valeur     float64
	Etiquettes []string // paires nom, valeur (instance comprise)
}

// InstanceServeur : machine des tâches ping sans machine liée.
const InstanceServeur = "mirador-server"

// Series : ce que les règles TacheEnEchec / TacheEnRetard lisent (SPEC § 80) :
//   - mirador_tache_suivie{instance,source,tache,gravite} = 1 ;
//   - mirador_tache_echeance_timestamp_seconds{…} : au-delà, en retard ;
//   - pour un ping : ses propres mirador_tache_fin / _code / _debut.
func Series(ts []Tache) []SerieInterne {
	var out []SerieInterne
	for _, t := range ts {
		inst := t.Machine
		if inst == "" {
			inst = InstanceServeur
		}
		et := []string{"instance", inst, "source", t.Source, "tache", t.Nom, "gravite", t.Gravite}
		out = append(out, SerieInterne{Nom: "mirador_tache_suivie", Valeur: 1, Etiquettes: et})
		if e := Echeance(t, t.Releve); !e.IsZero() {
			out = append(out, SerieInterne{Nom: "mirador_tache_echeance_timestamp_seconds", Valeur: float64(e.Unix()), Etiquettes: et})
		}
		if t.Source == SourcePing && !t.Releve.Fin.IsZero() {
			ep := et[:6]
			out = append(out, SerieInterne{Nom: "mirador_tache_fin_timestamp_seconds", Valeur: float64(t.Releve.Fin.Unix()), Etiquettes: ep},
				SerieInterne{Nom: "mirador_tache_code", Valeur: float64(t.Releve.Code), Etiquettes: ep})
			if !t.Releve.Debut.IsZero() {
				out = append(out, SerieInterne{Nom: "mirador_tache_debut_timestamp_seconds", Valeur: float64(t.Releve.Debut.Unix()), Etiquettes: ep})
			}
		}
	}
	return out
}
