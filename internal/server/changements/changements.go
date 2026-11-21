// SPDX-License-Identifier: GPL-3.0-or-later

// Package changements repère ce qui change sur les machines (SPEC § 47,
// idée reprise de New Relic « change tracking », idée seulement) :
// paquets mis à jour, redémarrage, noyau, version de l'agent, conteneur
// redémarré ou recréé. Tout vient des métriques déjà envoyées ; le
// serveur compare chaque minute avec la dernière valeur vue (gardée en base)
// et écrit un changement quand elle bouge.
package changements

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Types de changement.
const (
	Paquets     = "paquets"
	Redemarrage = "redemarrage"
	Noyau       = "noyau"
	Agent       = "agent"
	Conteneur   = "conteneur"
)

// Conservation : 90 jours.
const Conservation = 90 * 24 * time.Hour

// Changement : une ligne de la table changements.
type Changement struct {
	ID      int64
	Machine string
	Le      time.Time
	Type    string
	Detail  string
}

// Lecteur : requêtes instantanées sur les métriques.
type Lecteur interface {
	Instant(ctx context.Context, expr string, t time.Time) ([]alertes.Echantillon, error)
}

// Detecteur : comparaison chaque minute.
type Detecteur struct {
	DB      *sql.DB
	Lecteur Lecteur
	Journal *slog.Logger

	etat     map[string]string // cle → valeur (copie de changements_etat)
	charge   bool
	purgeeLe time.Time
}

// Boucle : un tour par minute jusqu'à l'arrêt.
func (d *Detecteur) Boucle(ctx context.Context) {
	if d.Journal == nil {
		d.Journal = slog.New(slog.DiscardHandler)
	}
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := d.Tour(ctx, time.Now()); err != nil {
				d.Journal.Warn("suivi des changements", "erreur", err)
			}
		}
	}
}

// nouveau : un changement à écrire.
type nouveau struct {
	Changement
	fusion bool // paquets : à ajouter au changement « paquets » récent s'il existe
}

// Tour : une comparaison à l'instant t.
func (d *Detecteur) Tour(ctx context.Context, t time.Time) error {
	if !d.charge {
		if err := d.charger(ctx); err != nil {
			return err
		}
	}
	premier := len(d.etat) == 0 // tout premier tour : on note sans rien signaler
	maj := map[string]string{}
	vues := map[string]bool{} // clés lues à ce tour (ménage des conteneurs disparus)
	var evs []nouveau
	noter := func(cle, valeur string) (avant string, connu bool) {
		vues[cle] = true
		avant, connu = d.etat[cle]
		if !connu || avant != valeur {
			maj[cle] = valeur
		}
		return avant, connu && !premier
	}
	lire := func(expr string) []alertes.Echantillon {
		e, err := d.Lecteur.Instant(ctx, expr, t)
		if err != nil {
			d.Journal.Warn("suivi des changements : lecture", "expr", expr, "erreur", err)
			return nil
		}
		return e
	}

	// Noyau d'abord : un redémarrage le cite.
	noyaux := map[string]string{} // instance → « noyau A → B »
	for _, e := range lire(`max by (instance, release) (node_uname_info)`) {
		inst, rel := e.Etiquettes.Get("instance"), e.Etiquettes.Get("release")
		if inst == "" || rel == "" {
			continue
		}
		if avant, connu := noter("noyau|"+inst, rel); connu && avant != rel {
			noyaux[inst] = "noyau " + avant + " → " + rel
		}
	}
	// Redémarrage : nouvelle heure de démarrage.
	redemarrees := map[string]bool{}
	for _, e := range lire(`max by (instance) (node_boot_time_seconds)`) {
		inst := e.Etiquettes.Get("instance")
		if inst == "" || math.IsNaN(e.Valeur) {
			continue
		}
		boot := int64(math.Round(e.Valeur))
		avant, connu := noter("boot|"+inst, strconv.FormatInt(boot, 10))
		if !connu {
			continue
		}
		if b, err := strconv.ParseInt(avant, 10, 64); err == nil && abs(boot-b) > 60 {
			detail := "Machine redémarrée"
			if n := noyaux[inst]; n != "" {
				detail += ", " + n
			}
			redemarrees[inst] = true
			evs = append(evs, nouveau{Changement: Changement{Machine: inst, Le: time.Unix(boot, 0), Type: Redemarrage, Detail: detail}})
		}
	}
	for inst, n := range noyaux { // noyau changé sans redémarrage vu (rare)
		if !redemarrees[inst] {
			evs = append(evs, nouveau{Changement: Changement{Machine: inst, Le: t, Type: Noyau, Detail: strings.ToUpper(n[:1]) + n[1:]}})
		}
	}
	// Version de l'agent : la plus récente vue.
	versions := map[string]struct {
		v string
		t float64
	}{}
	for _, e := range lire(`max by (instance, version) (timestamp(mirador_agent_info))`) {
		inst, v := e.Etiquettes.Get("instance"), e.Etiquettes.Get("version")
		if inst == "" || v == "" {
			continue
		}
		if x, ok := versions[inst]; !ok || e.Valeur > x.t {
			versions[inst] = struct {
				v string
				t float64
			}{v, e.Valeur}
		}
	}
	for inst, x := range versions {
		if avant, connu := noter("agent|"+inst, x.v); connu && avant != x.v {
			evs = append(evs, nouveau{Changement: Changement{Machine: inst, Le: t, Type: Agent,
				Detail: "mirador-agent " + avant + " → " + x.v}})
		}
	}
	evs = append(evs, d.conteneurs(lire, noter)...)
	evs = append(evs, d.paquets(lire, noter, premier)...)

	return d.ecrire(ctx, maj, evs, d.conteneursDisparus(vues), t)
}

// conteneursDisparus : clés « ct|machine|nom » plus lues, seulement pour
// les machines dont des conteneurs ont été lus à ce tour (une machine muette
// garde les siens). Relecture du 21/11 : les conteneurs jetables à nom
// aléatoire (docker run sans --name, CI) laissaient une clé chacun, pour
// toujours, en mémoire et en base.
func (d *Detecteur) conteneursDisparus(vues map[string]bool) []string {
	vivantes := map[string]bool{}
	for k := range vues {
		if r, ok := strings.CutPrefix(k, "ct|"); ok {
			inst, _, _ := strings.Cut(r, "|")
			vivantes[inst] = true
		}
	}
	var out []string
	for k := range d.etat {
		if r, ok := strings.CutPrefix(k, "ct|"); ok && !vues[k] {
			if inst, _, _ := strings.Cut(r, "|"); vivantes[inst] {
				out = append(out, k)
			}
		}
	}
	return out
}

// conteneurs : redémarré à la main ou recréé (heure de démarrage changée
// sans que le compteur de redémarrages automatiques ait bougé).
func (d *Detecteur) conteneurs(lire func(string) []alertes.Echantillon, noter func(string, string) (string, bool)) []nouveau {
	cle := func(e alertes.Echantillon) string {
		return e.Etiquettes.Get("instance") + "|" + e.Etiquettes.Get("name")
	}
	redem := map[string]int64{}
	for _, e := range lire(`max by (instance, name) (node_docker_container_restart_count)`) {
		redem[cle(e)] = int64(e.Valeur)
	}
	images := map[string]string{}
	for _, e := range lire(`max by (instance, name, image) (node_docker_container_info)`) {
		images[cle(e)] = e.Etiquettes.Get("image")
	}
	var evs []nouveau
	for _, e := range lire(`max by (instance, name) (node_docker_container_started_timestamp_seconds)`) {
		k := cle(e)
		inst, nom := e.Etiquettes.Get("instance"), e.Etiquettes.Get("name")
		if inst == "" || nom == "" || math.IsNaN(e.Valeur) {
			continue
		}
		debut := int64(e.Valeur)
		valeur := fmt.Sprintf("%d|%d|%s", debut, redem[k], images[k])
		avant, connu := noter("ct|"+k, valeur)
		if !connu || avant == valeur {
			continue
		}
		p := strings.SplitN(avant, "|", 3)
		if len(p) != 3 {
			continue
		}
		dAvant, _ := strconv.ParseInt(p[0], 10, 64)
		rAvant, _ := strconv.ParseInt(p[1], 10, 64)
		if abs(debut-dAvant) <= 1 {
			continue // compteur ou image relus, conteneur pas redémarré
		}
		if redem[k] > rAvant {
			continue // redémarrage automatique (plantage) : déjà couvert par les alertes
		}
		detail := nom + " redémarré"
		if img := images[k]; p[2] != "" && img != "" && img != p[2] {
			detail = nom + " recréé : image " + courte(p[2]) + " → " + courte(img)
		}
		evs = append(evs, nouveau{Changement: Changement{Machine: inst, Le: time.Unix(debut, 0), Type: Conteneur, Detail: detail}})
	}
	return evs
}

// courte : « ghcr.io/home-assistant/home-assistant:2026.11.2 » →
// « home-assistant:2026.11.2 ».
func courte(image string) string {
	if i := strings.LastIndex(image, "/"); i >= 0 {
		return image[i+1:]
	}
	return image
}

// paquets : paquets installés vus pour la première fois (agent : lecture de
// dpkg.log), regroupés par machine.
func (d *Detecteur) paquets(lire func(string) []alertes.Echantillon, noter func(string, string) (string, bool), premier bool) []nouveau {
	type pq struct {
		nom, avant, apres string
		le                int64
	}
	parMachine := map[string][]pq{}
	for _, e := range lire(`max by (instance, package, from, to) (apt_package_installed)`) {
		inst, nom := e.Etiquettes.Get("instance"), e.Etiquettes.Get("package")
		if inst == "" || nom == "" || math.IsNaN(e.Valeur) {
			continue
		}
		le := int64(e.Valeur)
		k := "pkg|" + inst + "|" + nom + "|" + e.Etiquettes.Get("to")
		if _, deja := d.etat[k]; deja {
			continue
		}
		noter(k, strconv.FormatInt(le, 10))
		if premier || !d.initialise(inst) {
			continue // machine vue pour la première fois : pas d'historique signalé
		}
		parMachine[inst] = append(parMachine[inst], pq{nom, e.Etiquettes.Get("from"), e.Etiquettes.Get("to"), le})
	}
	var evs []nouveau
	for inst, ps := range parMachine {
		sort.Slice(ps, func(i, j int) bool { return ps[i].le < ps[j].le || ps[i].le == ps[j].le && ps[i].nom < ps[j].nom })
		var noms []string
		for _, p := range ps {
			if p.avant == "" {
				noms = append(noms, p.nom+" "+p.apres+" (nouveau)")
			} else {
				noms = append(noms, p.nom+" "+p.avant+" → "+p.apres)
			}
		}
		evs = append(evs, nouveau{Changement: Changement{Machine: inst, Le: time.Unix(ps[0].le, 0), Type: Paquets,
			Detail: strings.Join(noms, ", ")}, fusion: true})
	}
	return evs
}

// initialise : la machine a déjà été vue par un tour précédent (sinon ses
// paquets des dernières 48 h ne sont pas signalés comme nouveaux).
func (d *Detecteur) initialise(inst string) bool {
	_, ok := d.etat["boot|"+inst]
	if !ok {
		_, ok = d.etat["agent|"+inst]
	}
	return ok
}

func abs(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
