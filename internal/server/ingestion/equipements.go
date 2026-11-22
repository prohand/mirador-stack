// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"context"
	"sync"
	"time"

	"mirador-stack/internal/server/stockage"
)

// Équipements relevés par un collecteur (SPEC § 23.13) : un collecteur ne
// peut parler que pour lui-même et pour les équipements que l'admin lui a
// rattachés (table equipements_collecteur). Toute autre « instance » est
// refusée et comptée. Les étiquettes site, type et groupe de chaque
// équipement sont celles de sa fiche.

// Equipement : un équipement rattaché et ses étiquettes.
type Equipement struct{ Site, Env, Groupe string }

// dureeCacheEquipements : la liste d'un collecteur est relue au plus toutes
// les 30 s (un rattachement fait dans l'interface prend effet sous 30 s).
const dureeCacheEquipements = 30 * time.Second

type cacheEquipements struct {
	mu    sync.Mutex
	liste map[int64]entreeEquipements
}

type entreeEquipements struct {
	expire time.Time
	noms   map[string]Equipement
}

// equipementsDe : équipements rattachés au collecteur (approuvés).
func (g *Ingestion) equipementsDe(ctx context.Context, collecteur int64, maintenant time.Time) map[string]Equipement {
	g.equipements.mu.Lock()
	e, ok := g.equipements.liste[collecteur]
	g.equipements.mu.Unlock()
	if ok && maintenant.Before(e.expire) {
		return e.noms
	}
	noms := map[string]Equipement{}
	rows, err := g.opt.DB.QueryContext(ctx, `SELECT m.nom, m.site, m.env, m.groupe FROM equipements_collecteur e
		JOIN machines m ON m.id = e.machine_id WHERE e.collecteur_id = ? AND m.statut = 'approuvee'`, collecteur)
	if err != nil {
		g.opt.Journal.Warn("équipements du collecteur illisibles", "erreur", err)
		return e.noms // dernière liste connue (vide au premier essai)
	}
	defer rows.Close()
	for rows.Next() {
		var nom string
		var q Equipement
		if rows.Scan(&nom, &q.Site, &q.Env, &q.Groupe) == nil {
			noms[nom] = q
		}
	}
	g.equipements.mu.Lock()
	if g.equipements.liste == nil || len(g.equipements.liste) > 1000 {
		g.equipements.liste = map[int64]entreeEquipements{}
	}
	g.equipements.liste[collecteur] = entreeEquipements{expire: maintenant.Add(dureeCacheEquipements), noms: noms}
	g.equipements.mu.Unlock()
	return noms
}

// OublierEquipements : après un rattachement dans l'interface (pris en
// compte tout de suite).
func (g *Ingestion) OublierEquipements() {
	g.equipements.mu.Lock()
	g.equipements.liste = nil
	g.equipements.mu.Unlock()
}

// jobCollecteur : étiquette « job » des mesures propres d'un collecteur
// (mirador-collector : son état, ses adresses, son syslog, ses mesures
// internes). Aucun relevé d'équipement ne la porte (snmp, unifi, certs).
const jobCollecteur = "mirador-collector"

// etiqueterSerie impose le nom et les étiquettes d'une série. Agent : son
// nom. Collecteur : son nom, ou celui d'un équipement rattaché (étiquettes
// de sa fiche, nom renvoyé) ; sinon la série est refusée (false).
//
// Mesures propres d'un collecteur (job mirador-collector) : toujours sous
// son nom. Retour du 22/11 : un collecteur nommé « collecteur-lan » dans
// l'interface mais « obscollector » sur sa machine voyait ses propres
// mesures (adresses, écoute syslog) prises pour un équipement non
// rattaché, et jetées.
func etiqueterSerie(m Machine, eqs map[string]Equipement, s *stockage.Serie) (equipement string, ok bool) {
	inst := m.Nom
	site, env, groupe := m.Site, m.Env, m.Groupe
	if m.Type == "collecteur" {
		s.Remplacer("collecteur", m.Nom)
		if v := s.Valeur("instance"); v != "" && v != m.Nom && s.Valeur("job") != jobCollecteur {
			q, ok := eqs[v]
			if !ok {
				return "", false
			}
			inst, site, env, groupe = v, q.Site, q.Env, q.Groupe
			equipement = v
		}
	}
	s.Remplacer("instance", inst)
	if site != "" {
		s.Remplacer("site", site)
	}
	if env != "" {
		s.Remplacer("env", env)
	}
	if groupe != "" {
		s.Remplacer("groupe", groupe)
	}
	return equipement, true
}

// etiqueterLigne : même règle pour une ligne de logs (champ « host »). Une
// ligne d'un hôte non rattaché n'est pas perdue : elle est rangée sous le
// nom du collecteur (champ « hote_annonce » gardé pour comprendre).
func etiqueterLigne(m Machine, eqs map[string]Equipement, champs map[string]string) {
	hote := m.Nom
	site, env, groupe := m.Site, m.Env, m.Groupe
	if m.Type == "collecteur" {
		champs["collecteur"] = m.Nom
		if v := champs["host"]; v != "" && v != m.Nom {
			if q, ok := eqs[v]; ok {
				hote, site, env, groupe = v, q.Site, q.Env, q.Groupe
			} else {
				champs["hote_annonce"] = v
			}
		}
	}
	champs["host"] = hote
	if site != "" {
		champs["site"] = site
	}
	if env != "" {
		champs["env"] = env
	}
	if groupe != "" {
		champs["groupe"] = groupe
	}
}
