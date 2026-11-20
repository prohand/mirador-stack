// SPDX-License-Identifier: GPL-3.0-or-later

package taches

import (
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
