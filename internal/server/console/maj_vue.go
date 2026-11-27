// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"slices"
	"sort"
	"strings"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/server/flotte"
)

// Page « Mises à jour des agents » revue le 21/11 (maquette validée, SPEC
// § 109) : tuiles en haut, déploiement en 3 étapes (importer, essayer sur
// une machine, déployer sur tout le parc), machines par état avec filtres,
// une carte par version gardée, réglages repliés en bas.

// États d'une machine vis-à-vis de sa version cible.
const (
	majAJour   = "ajour"
	majRetard  = "retard"
	majEnCours = "encours"
	majEchec   = "echec"
)

type ligneMaj struct {
	flotte.InfoMachine
	Cible       string // cible effective (propre, sinon celle du parc)
	CiblePropre bool
	Etat        string // ajour, retard, encours, echec
	Essai       bool   // machine d'essai de la version à déployer
}

type carteVersion struct {
	Version     string
	Archs       []string
	Collecteur  bool // le collecteur est aussi publié dans cette version
	Le          int64
	Par         string
	Retour      bool // manifeste « retour autorisé »
	Machines    int
	Parc        bool // version du parc
	Supprimable bool
}

// vueMajParc : ce qui s'ajoute aux données de la page.
type vueMajParc struct {
	Derniere         string // dernière version publiée sur GitHub (sans « v »)
	DerniereDate     string
	DerniereImportee bool
	Total, AJour     int
	Retard, Echec    int
	PctAJour         int
	PctRetard        int
	PctEchec         int
	Filtre           string
	Lignes           []ligneMaj
	NomsEchecs       []string // machines en échec (« Réessayer les N échecs », SPEC § 141)
	// Déploiement : version proposée, étape en cours (0 : rien à faire,
	// 1 importer, 2 essayer, 3 déployer, 4 fait), machine d'essai.
	Deployer   string
	Etape      int
	Essai      *ligneMaj
	Candidates []string // machines proposées pour l'essai
	Cartes     []carteVersion
}

// etatMaj : état d'une machine (cible effective cible).
func etatMaj(m flotte.InfoMachine, cible string) string {
	switch {
	case m.MajEchec != "":
		return majEchec
	case cible == "" || m.Version == cible:
		return majAJour
	case m.MajEssai == cible:
		return majEnCours
	}
	return majRetard
}

// plusRecente : vrai si a est plus récente que b (b vide : vrai).
func plusRecente(a, b string) bool {
	if b == "" {
		return a != ""
	}
	n, err := majagent.ComparerVersions(a, b)
	return err == nil && n > 0
}

// construireVueMaj : à partir des données déjà lues de la page.
func construireVueMaj(d *donneesMaj, filtre string) {
	v := &d.Vue
	importees := map[string]bool{}
	for _, x := range d.Choix {
		importees[x] = true
	}
	if len(d.Proposees) > 0 {
		v.Derniere = strings.TrimPrefix(d.Proposees[0].Tag, "v")
		v.DerniereDate = d.Proposees[0].Date
		v.DerniereImportee = importees[v.Derniere]
	}
	// Version à déployer : la dernière de GitHub, sinon la plus récente
	// importée ; seulement si elle est plus récente que celle du parc.
	v.Deployer = v.Derniere
	if v.Deployer == "" {
		for _, x := range d.Choix {
			if plusRecente(x, v.Deployer) {
				v.Deployer = x
			}
		}
	}
	enService := map[string]int{}
	for _, m := range d.Machines {
		// Équipements relevés par un collecteur (jamais de version) et
		// machines en attente : pas concernés.
		if m.Statut != "approuvee" || m.Version == "" {
			continue
		}
		enService[m.Version]++
		l := ligneMaj{InfoMachine: m, Cible: d.Cible}
		if m.VersionCible != "" {
			l.Cible, l.CiblePropre = m.VersionCible, true
		}
		l.Etat = etatMaj(m, l.Cible)
		l.Essai = l.CiblePropre && v.Deployer != "" && m.VersionCible == v.Deployer && v.Deployer != d.Cible
		v.Total++
		switch l.Etat {
		case majAJour:
			v.AJour++
		case majEchec:
			v.Echec++
		default:
			v.Retard++
		}
		if l.Essai && v.Essai == nil {
			e := l
			v.Essai = &e
		}
		if m.Type != "collecteur" {
			v.Candidates = append(v.Candidates, m.Nom)
		}
		if l.Etat == majEchec {
			v.NomsEchecs = append(v.NomsEchecs, m.Nom)
		}
		if (filtre == "" || filtre == l.Etat || (filtre == majRetard && l.Etat == majEnCours)) && (d.Site == "" || strings.EqualFold(m.Site, d.Site)) {
			v.Lignes = append(v.Lignes, l)
		}
	}
	if v.Total > 0 {
		v.PctAJour = v.AJour * 100 / v.Total
		v.PctRetard = v.Retard * 100 / v.Total
		v.PctEchec = 100 - v.PctAJour - v.PctRetard // reste (arrondis) : pas d'écart
		if v.Echec == 0 {
			v.PctRetard, v.PctEchec = 100-v.PctAJour, 0
			if v.Retard == 0 {
				v.PctAJour, v.PctRetard = 100, 0
			}
		}
	}
	v.Filtre = filtre
	// Ordre : échecs, puis en retard / en cours, puis à jour ; par nom.
	rang := map[string]int{majEchec: 0, majEnCours: 1, majRetard: 1, majAJour: 2}
	sort.SliceStable(v.Lignes, func(i, j int) bool {
		if rang[v.Lignes[i].Etat] != rang[v.Lignes[j].Etat] {
			return rang[v.Lignes[i].Etat] < rang[v.Lignes[j].Etat]
		}
		return v.Lignes[i].Nom < v.Lignes[j].Nom
	})
	// Étape du déploiement.
	switch {
	case v.Deployer == "" && len(d.Choix) == 0:
		v.Etape = 1 // rien sur le serveur : commencer par importer
	case v.Deployer == "" || !plusRecente(v.Deployer, d.Cible) && v.Deployer != d.Cible:
		v.Etape = 0
	case v.Deployer == d.Cible:
		v.Etape = 4
	case !importees[v.Deployer]:
		v.Etape = 1
	case v.Essai != nil && v.Essai.Version == v.Deployer:
		v.Etape = 3
	default:
		v.Etape = 2
	}
	// Une carte par version (une ligne par produit et architecture en base).
	index := map[string]int{}
	for _, x := range d.Versions {
		i, ok := index[x.Version]
		if !ok {
			i = len(v.Cartes)
			index[x.Version] = i
			v.Cartes = append(v.Cartes, carteVersion{Version: x.Version, Le: x.PublieeLe, Par: x.PublieePar})
		}
		c := &v.Cartes[i]
		if !slices.Contains(c.Archs, x.Architecture) {
			c.Archs = append(c.Archs, x.Architecture)
		}
		c.Collecteur = c.Collecteur || x.Produit == "mirador-collector"
		c.Retour = c.Retour || x.RetourAutorise
	}
	for i := range v.Cartes {
		c := &v.Cartes[i]
		sort.Strings(c.Archs)
		c.Machines = enService[c.Version]
		c.Parc = c.Version == d.Cible
		c.Supprimable = !c.Parc && c.Machines == 0
	}
	sort.SliceStable(v.Cartes, func(i, j int) bool { return plusRecente(v.Cartes[i].Version, v.Cartes[j].Version) })
}
