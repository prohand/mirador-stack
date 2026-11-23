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
	infoApp
	Dispo string // « 99,93 % » sur 30 jours (éléments clés)
}

type donneesServices struct {
	Cartes                   []carteService
	NbPanne, NbDegrade, NbOK int
	FiltreSur                filtreSur
	// SPEC § 122 : disponibilité moyenne, noms par état (cases), propositions
	// Docker Compose, éditeur.
	DispoMoyenne                   string
	NomsPanne, NomsDegrade, NomsOK string
	Propositions                   []proposition
	Editeur                        editeurApp
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
	if nom := q.r.URL.Query().Get("nom"); nom != "" { // depuis une fiche (§ 68)
		garde := svcs[:0]
		for _, s := range svcs {
			if s.Nom == nom {
				garde = append(garde, s)
			}
		}
		svcs = garde
	}
	toutes := svcs
	if q.r.URL.Query().Get("nom") != "" {
		toutes, _ = servicesmetier.Lire(ctx, c.db)
	}
	etats := servicesmetier.Calculer(ctx, c.lecteurServices(), enCours, r.ReglesPanne, svcs, q.t)
	infos := c.infosApps(ctx, etats, q.t)
	debut := q.t.Add(-30 * 24 * time.Hour)
	pannes := c.pannes(ctx, r.ReglesPanne, debut, q.t)
	maint := c.maintenancesMachines(ctx, debut, q.t)
	var d donneesServices
	if nom := q.r.URL.Query().Get("nom"); nom != "" {
		d.FiltreSur = filtreSur{"l'application « " + nom + " »", "/applications"}
	}
	var somme float64
	var panne, degrade, ok []string
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
		d.Cartes = append(d.Cartes, carteService{EtatService: e, infoApp: infos[e.Nom], Dispo: pct(dispo) + " %"})
		somme += dispo
		switch e.Etat {
		case servicesmetier.Panne:
			d.NbPanne++
			panne = append(panne, e.Nom)
		case servicesmetier.Degrade:
			d.NbDegrade++
			degrade = append(degrade, e.Nom)
		default:
			d.NbOK++
			ok = append(ok, e.Nom)
		}
	}
	if len(etats) > 0 {
		d.DispoMoyenne = pct(somme/float64(len(etats))) + " %"
	}
	d.NomsPanne, d.NomsDegrade, d.NomsOK = resumeNoms(panne), resumeNoms(degrade), resumeNoms(ok)
	cts := c.conteneursConnus(ctx, q.t)
	if q.r.URL.Query().Get("nom") == "" {
		d.Propositions = propositionsCompose(cts, toutes)
	}
	d.Editeur = c.editeurDe(ctx, q, toutes, cts)
	// Problèmes en premier.
	for i := 1; i < len(d.Cartes); i++ {
		for j := i; j > 0 && d.Cartes[j].Etat > d.Cartes[j-1].Etat; j-- {
			d.Cartes[j], d.Cartes[j-1] = d.Cartes[j-1], d.Cartes[j]
		}
	}
	c.rendre(q, "services", "Applications", "services", d)
}

func (c *Console) postServices(q *req) {
	ctx := q.r.Context()
	if err := c.verifierCode(q); err != nil {
		c.retour(q, "/applications", err, "")
		return
	}
	liste, err := servicesmetier.Lire(ctx, c.db)
	if err != nil {
		c.retour(q, "/applications", err, "")
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
		cs, err := lireElementsFormulaire(q)
		if err != nil {
			c.retour(q, "/applications", err, "")
			return
		}
		liste = append(liste, servicesmetier.Service{Nom: nom, Composants: cs})
		err = servicesmetier.Enregistrer(ctx, c.db, liste, q.qui(), q.ip.String(), q.t)
		if err == nil && f.texte("statut_present") != "" {
			err = c.statutApplication(ctx, q, ancien, nom, f.texte("statut") != "", false)
		}
		c.retour(q, "/applications", err, "application « "+nom+" » enregistrée")
	case "supprimer":
		err = servicesmetier.Enregistrer(ctx, c.db, liste, q.qui(), q.ip.String(), q.t)
		if err == nil {
			err = c.statutApplication(ctx, q, ancien, ancien, false, true)
		}
		c.retour(q, "/applications", err, "application « "+ancien+" » supprimée")
	default:
		c.retour(q, "/applications", errors.New("action inconnue"), "")
	}
}

// lecteurServices : lecteur des métriques pour servicesmetier.Calculer, ou
// nil (vraie interface nulle) sans base de métriques : un pointeur nul
// glissé dans l'interface passerait le test « l == nil » et planterait.
func (c *Console) lecteurServices() servicesmetier.Lecteur {
	if c.lecteur == nil {
		return nil
	}
	return c.lecteur
}
