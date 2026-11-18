// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"errors"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/servicesmetier"
)

// Page « Services » (SPEC § 50, idée reprise de New Relic « workloads ») :
// un service regroupe machines, conteneurs et sondes ; état, éléments,
// disponibilité sur 30 jours (éléments clés). Création et changements :
// administrateurs, code 2FA.

type carteService struct {
	servicesmetier.EtatService
	Dispo  string // « 99,93 % » sur 30 jours (éléments clés)
	Lignes string // formulaire « Modifier »
}

type donneesServices struct {
	Cartes                   []carteService
	NbPanne, NbDegrade, NbOK int
}

func (c *Console) pageServices(q *req) {
	ctx := q.r.Context()
	svcs, err := servicesmetier.Lire(ctx, c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	r, _ := notif.LireReglages(ctx, c.db)
	var enCours []alertes.Alerte
	if c.opt.Alertes != nil {
		enCours = c.opt.Alertes.EnCours()
	}
	etats := servicesmetier.Calculer(ctx, c.lecteur, enCours, r.ReglesPanne, svcs, q.t)
	debut := q.t.Add(-30 * 24 * time.Hour)
	pannes := c.pannes(ctx, r.ReglesPanne, debut, q.t)
	maint := c.maintenancesMachines(ctx, debut, q.t)
	var d donneesServices
	for _, e := range etats {
		var iv []intervalle
		for _, x := range e.Service.Composants {
			if !x.Cle {
				continue
			}
			switch x.Sorte {
			case servicesmetier.Machine:
				iv = append(iv, retirer(pannes["m:"+x.Nom], maint["m:"+x.Nom])...)
			case servicesmetier.Sonde:
				iv = append(iv, pannes["s:"+x.Nom]...)
			}
		}
		total := q.t.Unix() - debut.Unix()
		dispo := 100 * (1 - float64(dureeDans(reunir(iv), debut.Unix(), q.t.Unix()))/float64(total))
		d.Cartes = append(d.Cartes, carteService{EtatService: e, Dispo: pct(dispo) + " %", Lignes: servicesmetier.Lignes(e.Service)})
		switch e.Etat {
		case servicesmetier.Panne:
			d.NbPanne++
		case servicesmetier.Degrade:
			d.NbDegrade++
		default:
			d.NbOK++
		}
	}
	// Problèmes en premier.
	for i := 1; i < len(d.Cartes); i++ {
		for j := i; j > 0 && d.Cartes[j].Etat > d.Cartes[j-1].Etat; j-- {
			d.Cartes[j], d.Cartes[j-1] = d.Cartes[j-1], d.Cartes[j]
		}
	}
	c.rendre(q, "services", "Services", "services", d)
}

func (c *Console) postServices(q *req) {
	ctx := q.r.Context()
	if err := c.verifierCode(q); err != nil {
		c.retour(q, "/services", err, "")
		return
	}
	liste, err := servicesmetier.Lire(ctx, c.db)
	if err != nil {
		c.retour(q, "/services", err, "")
		return
	}
	f := champs{q}
	nom, ancien := f.texte("nom"), f.texte("ancien")
	garde := liste[:0]
	for _, s := range liste {
		if s.Nom != ancien || ancien == "" {
			garde = append(garde, s)
		}
	}
	liste = garde
	switch f.texte("action") {
	case "enregistrer":
		cs, err := servicesmetier.LireLignes(q.r.PostFormValue("elements"))
		if err != nil {
			c.retour(q, "/services", err, "")
			return
		}
		liste = append(liste, servicesmetier.Service{Nom: nom, Composants: cs})
		err = servicesmetier.Enregistrer(ctx, c.db, liste, q.qui(), q.ip.String(), q.t)
		c.retour(q, "/services", err, "service « "+nom+" » enregistré")
	case "supprimer":
		err = servicesmetier.Enregistrer(ctx, c.db, liste, q.qui(), q.ip.String(), q.t)
		c.retour(q, "/services", err, "service « "+ancien+" » supprimé")
	default:
		c.retour(q, "/services", errors.New("action inconnue"), "")
	}
}
