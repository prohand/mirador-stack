// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/stockage/journaux"
	"mirador-stack/internal/server/taches"
)

// « Voir l'erreur » d'une tâche planifiée (SPEC § 93, maquette validée le
// 20/11) : panneau latéral avec ce qu'on sait de l'échec :
//   - timer systemd : dernières lignes du journal du service, autour de la fin ;
//   - conteneur ponctuel : dernières lignes de logs du conteneur ;
//   - sauvegarde Proxmox : erreur de chaque VM et lignes « ERROR » du journal ;
//   - signal curl : le message envoyé avec /echec ou /code.
// Les logs viennent de ce que mirador reçoit déjà (aucune commande lancée).

const (
	lignesErreurTache = 30
	avantErreurTache  = 2 * time.Minute // logs lus depuis le début de la tâche, moins ce délai
	apresErreurTache  = time.Minute
	sansDebutTache    = 30 * time.Minute // début inconnu : fenêtre avant la fin
)

type ligneJournalTache struct {
	Heure, Texte string
	Erreur       bool
}

type donneesErreurTache struct {
	Titre, Nom, SourceTexte, Machine string
	Etat                             taches.Etat
	Fin                              time.Time
	Duree                            string
	Message                          string
	VMs                              []taches.VMReleve // en échec seulement
	Erreurs                          []string
	Journal                          []ligneJournalTache
	TitreJournal                     string
	LienLogs                         string
	SansLogs                         string // pourquoi aucune ligne
}

var motsErreur = []string{"error", "erreur", "fail", "fatal", "denied", "refus", "exception", "panic", "traceback", "critical"}

func ligneEnErreur(s string) bool {
	m := strings.ToLower(s)
	for _, x := range motsErreur {
		if strings.Contains(m, x) {
			return true
		}
	}
	return false
}

// tacheParCle : tâche suivie ou détectée (« machine|source|nom », ou
// « ping:<id> »), avec sa dernière exécution.
func (c *Console) tacheParCle(q *req, cle string) (taches.Tache, bool) {
	ctx := q.r.Context()
	det := taches.Detectees(func(expr string) ([]alertes.Echantillon, error) { return c.instant(ctx, expr, q.t) })
	ts, _ := taches.Lister(ctx, c.db)
	for _, t := range taches.AvecReleves(ts, det) {
		if t.Cle == cle || (t.Source == taches.SourcePing && cle == "ping:"+strconv.FormatInt(t.ID, 10)) {
			return t, true
		}
	}
	if d := det[cle]; d != nil {
		return taches.Tache{Cle: cle, Nom: d.Nom, Source: d.Source, Machine: d.Machine, Releve: d.Releve}, true
	}
	return taches.Tache{}, false
}

func (c *Console) pageErreurTache(q *req) {
	cle := q.r.URL.Query().Get("cle")
	t, ok := c.tacheParCle(q, cle)
	if !ok || len(cle) > 300 {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Tâche introuvable", "taches", nil)
		return
	}
	r := t.Releve
	d := donneesErreurTache{Titre: t.Affiche(), Nom: t.Nom, SourceTexte: textesSources[t.Source], Machine: t.Machine,
		Etat: taches.Calculer(t, r, q.t), Fin: r.Fin, Message: r.Message, Erreurs: r.Erreurs}
	if !r.Debut.IsZero() && !r.Fin.IsZero() && !r.Fin.Before(r.Debut) {
		d.Duree = dureeCourteTache(r.Fin.Sub(r.Debut))
		if r.Fin.Sub(r.Debut) < time.Minute {
			d.Duree = strconv.Itoa(int(r.Fin.Sub(r.Debut)/time.Second)) + " s"
		}
	}
	for _, v := range r.VMs {
		if !v.OK {
			d.VMs = append(d.VMs, v)
		}
	}
	// Logs du service ou du conteneur, autour de la fin.
	champs := map[string]string{}
	switch t.Source {
	case "systemd":
		unite := strings.TrimSuffix(t.Nom, ".timer") + ".service"
		champs["unit"] = unite
		d.TitreJournal = "Dernières lignes du journal de " + unite
	case "docker":
		champs["container"] = t.Nom
		d.TitreJournal = "Dernières lignes de logs du conteneur " + t.Nom
	}
	if len(champs) > 0 && t.Machine != "" {
		champs["host"] = t.Machine
		fin := r.Fin
		if fin.IsZero() {
			fin = q.t
		}
		debut := r.Debut.Add(-avantErreurTache)
		if r.Debut.IsZero() || fin.Sub(debut) > 6*time.Hour {
			debut = fin.Add(-sansDebutTache)
		}
		fin = fin.Add(apresErreurTache)
		v := url.Values{"de": {strconv.FormatInt(debut.UnixMilli(), 10)}, "a": {strconv.FormatInt(fin.UnixMilli(), 10)}}
		for k, x := range champs {
			v.Set(k, x)
		}
		d.LienLogs = "/logs?" + v.Encode()
		d.TitreJournal += " (" + debut.In(fuseau.Load()).Format("15:04") + " → " + fin.In(fuseau.Load()).Format("15:04") + ")"
		if c.opt.Logs == nil {
			d.SansLogs = "Logs indisponibles sur ce serveur."
		} else if res, err := c.chercherLogs(q.r.Context(), journaux.Requete{Debut: debut.UnixMilli(), Fin: fin.UnixMilli(),
			Champs: champs, Limite: lignesErreurTache}); err != nil {
			d.SansLogs = "Recherche impossible : " + err.Error()
		} else {
			for i := len(res.Lignes) - 1; i >= 0; i-- { // plus anciennes d'abord
				l := res.Lignes[i]
				d.Journal = append(d.Journal, ligneJournalTache{Heure: time.UnixMilli(l.T).In(fuseau.Load()).Format("15:04:05"),
					Texte: l.Msg, Erreur: ligneEnErreur(l.Msg)})
			}
			if len(d.Journal) == 0 {
				d.SansLogs = "Aucune ligne de logs reçue pour ce moment (logs du service non collectés, ou rien écrit)."
			}
		}
	}
	if q.r.Header.Get("X-Requested-With") == "mirador" {
		c.pages.executer(q.w, http.StatusOK, "erreur-tache-fragment", Page{D: d})
		return
	}
	c.rendre(q, "erreur-tache-page", "Erreur · "+d.Titre, "taches", d)
}
