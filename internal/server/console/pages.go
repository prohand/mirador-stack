// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/ingestion"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/sauvegarde"
	"mirador-stack/internal/server/servicesmetier"
)

// routesPages : pages de supervision et de réglage.
func (c *Console) routesPages() {
	m := c.mux
	m.HandleFunc("GET /fragments/accueil", c.lecture(c.fragmentAccueil))
	m.HandleFunc("GET /mur", c.lecture(c.pageMur))
	m.HandleFunc("GET /machines", c.lecture(c.pageMachines))
	m.HandleFunc("GET /conteneurs", c.lecture(c.pageConteneurs))
	m.HandleFunc("GET /sondes", c.lecture(c.pageSondes))
	m.HandleFunc("GET /taches", c.lecture(c.pageTaches))             // tâches planifiées (SPEC § 80)
	m.HandleFunc("GET /taches/erreur", c.lecture(c.pageErreurTache)) // « Voir l'erreur » (SPEC § 93)
	m.HandleFunc("POST /taches", c.admin(c.postTaches))
	// « Services » renommés « Applications » le 19/11 (SPEC § 65) : les
	// anciennes adresses mènent aux nouvelles.
	m.HandleFunc("GET /applications", c.lecture(c.pageServices))
	m.HandleFunc("GET /services", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/applications", http.StatusMovedPermanently)
	})
	m.HandleFunc("GET /elements", c.lecture(c.pageElements))
	m.HandleFunc("GET /sondes/{nom}", c.lecture(c.ficheSonde))
	m.HandleFunc("GET /applications/{nom}", c.lecture(c.ficheService))
	m.HandleFunc("GET /services/{nom}", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/applications/"+url.PathEscape(r.PathValue("nom")), http.StatusMovedPermanently)
	})
	m.HandleFunc("GET /conteneurs/{machine}/{nom}", c.lecture(c.ficheConteneur))
	m.HandleFunc("POST /etiquettes", c.admin(c.postEtiquettes))
	m.HandleFunc("POST /applications", c.admin(c.postServices))
	m.HandleFunc("POST /sondes", c.admin(c.postSondes))
	m.HandleFunc("POST /sondes/{nom}/essai", c.admin(c.postEssaiSonde))
	m.HandleFunc("GET /machines/{nom}/detail/{quoi}", c.lecture(c.detailMachine))
	m.HandleFunc("GET /machines/{nom}/direct", c.lecture(c.fluxDirect)) // relevés en direct (SPEC § 97)
	m.HandleFunc("GET /machines/ajouter", c.admin(c.pageAjouter))
	m.HandleFunc("POST /machines/jeton", c.admin(c.postJeton))
	m.HandleFunc("POST /machines/equipement", c.admin(c.postEquipement))
	m.HandleFunc("GET /machines/{nom}", c.lecture(c.pageMachine))
	m.HandleFunc("POST /machines/{nom}", c.admin(c.postMachine))
	m.HandleFunc("POST /machines/{nom}/sondes", c.admin(c.postSondesMachine))
	m.HandleFunc("GET /mises-a-jour", c.lecture(c.pageMisesAJour))
	m.HandleFunc("POST /mises-a-jour", c.admin(c.postMisesAJour))
	m.HandleFunc("POST /mises-a-jour/publier", c.admin(c.postPublier))
	m.HandleFunc("GET /tableaux", c.lecture(c.pageTableaux))
	m.HandleFunc("GET /tableaux/{nom}", c.lecture(c.pageTableau))
	m.HandleFunc("GET /donnees/courbe", c.lecture(c.donneesCourbe))
	m.HandleFunc("GET /donnees/volume-logs", c.lecture(c.donneesVolumeLogs))
	m.HandleFunc("GET /donnees/aller", c.lecture(c.donneesAller))
	c.routesAlertes()
	c.routesLogs()
	c.routesModeles()
	c.routesModelesPerso()
	c.routesTheme()
	c.routesPush()
	m.HandleFunc("POST /mode-admin/quitter", c.lecture(c.postQuitterModeAdmin))
	m.HandleFunc("POST /mode-admin/ouvrir", c.admin(c.postOuvrirModeAdmin))
	c.routesReglages()
}

// --- Vue d'ensemble --------------------------------------------------------------

type resumeAlerte struct {
	ID                                    int64
	Regle, Gravite, Machine, Resume, Etat string
	Depuis                                int64
	Acquittee                             bool
	Sourdine                              string
	// Reste : condition « en attente » (vraie, mais pas depuis assez
	// longtemps) : temps avant qu'elle devienne une alerte si elle dure.
	Reste string
	// Prise en charge (SPEC § 30), lue en base (le moteur ne la connaît
	// pas) : par qui, quand, jusqu'à quand (vide = jusqu'à la fin).
	AcquitteePar, AcquitteeLe, AcquitteeJusqua string
	// Absente : la machine n'envoie plus rien depuis ce temps (« 26 h »),
	// l'alerte reste ouverte (SPEC § 46). Vide : données à jour.
	Absente string
}

// separerMasquees : alertes à montrer d'un côté ; de l'autre, celles qui ne
// sont que la conséquence d'une alerte plus grave déjà affichée (machine
// muette qui inhibe ses autres alertes, parent en panne). Retour du 16/11 :
// « un agent muet ne devrait rien remonter d'autre » : elles vont dans un
// bloc replié, ne sont plus comptées ni mêlées aux alertes à traiter.
// Silences et maintenances (choisis à la main) restent affichés tels quels.
func separerMasquees(rs []resumeAlerte) (visibles, masquees []resumeAlerte) {
	for _, r := range rs {
		if strings.HasPrefix(r.Sourdine, "inhibée par ") || strings.HasPrefix(r.Sourdine, "dépendance : ") {
			masquees = append(masquees, r)
		} else {
			visibles = append(visibles, r)
		}
	}
	return visibles, masquees
}

// separerPrises : alertes à traiter d'un côté, prises en charge de l'autre.
func separerPrises(rs []resumeAlerte) (aTraiter, prises []resumeAlerte) {
	for _, r := range rs {
		if r.Acquittee {
			prises = append(prises, r)
		} else {
			aTraiter = append(aTraiter, r)
		}
	}
	return aTraiter, prises
}

// infoPrise : prise en charge d'une alerte en cours.
type infoPrise struct {
	Par        string
	Le, Jusqua int64 // millisecondes ; Jusqua 0 = jusqu'à la fin
}

// prisesEnCharge : alertes en cours acquittées, lues en base (une requête).
func (c *Console) prisesEnCharge(ctx context.Context) map[int64]infoPrise {
	out := map[int64]infoPrise{}
	rows, err := c.db.QueryContext(ctx, `SELECT id, acquittee_par, acquittee_le, acquittee_jusqua FROM alertes WHERE etat != 'resolue' AND acquittee_le > 0`)
	if err != nil {
		return out
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var p infoPrise
		if rows.Scan(&id, &p.Par, &p.Le, &p.Jusqua) == nil {
			out[id] = p
		}
	}
	return out
}

// separerAttente : alertes déclenchées d'un côté, conditions encore « en
// attente » (pas encore des alertes, ni comptées ni notifiées) de l'autre.
func separerAttente(rs []resumeAlerte) (actives, attente []resumeAlerte) {
	for _, r := range rs {
		if r.Etat == "en_attente" {
			attente = append(attente, r)
		} else {
			actives = append(actives, r)
		}
	}
	return actives, attente
}

type donneesAccueil struct {
	LienLinux                    bool // tableau « Serveurs Linux » installé
	Machines, Muettes, EnAttente int
	Critiques, Avertissements    int
	Infos, Actives               int // Actives : alertes actives hors sourdine
	Alertes                      []resumeAlerte
	Attente                      []resumeAlerte          // conditions pas encore des alertes
	Prises                       []resumeAlerte          // prises en charge (acquittées), SPEC § 30
	Masquees                     []resumeAlerte          // conséquences d'une alerte plus grave (bloc replié)
	Sites                        []notif.SiteInjoignable // sites injoignables (SPEC § 39)
	Canaux                       []notif.EtatCanal
	Mode                         string
	DerniereSauvegarde           sauvegarde.Execution
	SauvegardeOK                 bool
	Metriques, Logs              int64
	Parc                         []ligneParc
	Nouveautes                   int // éléments détectés « à valider », tout le parc
	Principal, Secours           notif.EtatCanal
	Plafond                      int64 // plafond total de stockage (réglage « Conservation »)
	PctStockage                  int
	Depuis                       int64
	Chaleur                      []vueChaleur // carte de chaleur du parc sur 24 h (SPEC § 31)
	Vue                          string       // "" (résumé) ou "ruche" (SPEC § 51)
	Ruche                        *vueRuche
	R                            donneesRegarder // bandeau, cases, « À regarder », aperçu (SPEC § 120)
}

func (c *Console) pageAccueil(q *req) {
	c.rendre(q, "accueil", "Vue d'ensemble", "accueil", c.donneesAccueilVue(q))
}

// donneesAccueilVue : résumé ou ruche (onglet, SPEC § 51).
func (c *Console) donneesAccueilVue(q *req) donneesAccueil {
	d := c.donneesAccueil(q)
	if q.r.URL.Query().Get("vue") == "ruche" {
		d.Vue = "ruche"
		d.Ruche = c.ruche(q.r.Context(), q.r.URL.Query().Get("ruche"), d.Parc, q.t)
	}
	return d
}

// fragmentAccueil : même contenu, sans le cadre (rechargé toutes les 60 s).
func (c *Console) fragmentAccueil(q *req) {
	c.pages.executer(q.w, http.StatusOK, "accueil-fragment", Page{CSRF: q.s.CSRF, Admin: q.admin(), D: c.donneesAccueilVue(q),
		ClassesAccueil: classesAccueil(q.u)})
}

const delaiMuette = 5 * time.Minute

func (c *Console) donneesAccueil(q *req) donneesAccueil {
	ctx := q.r.Context()
	d := donneesAccueil{Depuis: c.opt.Debut.UnixMilli(), LienLinux: c.tableau("serveurs-linux") != nil}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			switch m.Statut {
			case "en_attente":
				d.EnAttente++
			case "approuvee":
				d.Machines++
				if q.t.Sub(time.Unix(m.VuLe, 0)) > delaiMuette {
					d.Muettes++
				}
			}
		}
	}
	d.Alertes, d.Attente = separerAttente(c.alertesEnCours(""))
	toutes := d.Alertes // la couleur d'une machine compte aussi ses alertes prises en charge
	d.Alertes, d.Prises = separerPrises(d.Alertes)
	d.Alertes, d.Masquees = separerMasquees(d.Alertes)
	for _, a := range d.Alertes {
		if a.Etat != "active" || a.Sourdine != "" {
			continue
		}
		switch a.Gravite {
		case alertes.Critique:
			d.Critiques++
		case alertes.Avertissement:
			d.Avertissements++
		default:
			d.Infos++
		}
	}
	if c.opt.Postier != nil {
		d.Canaux, _, _ = c.opt.Postier.Etat()
	}
	if c.opt.Aiguilleur != nil {
		d.Mode = c.opt.Aiguilleur.Mode()
		d.Sites = c.opt.Aiguilleur.SitesInjoignables()
	}
	d.DerniereSauvegarde, d.SauvegardeOK, _ = sauvegarde.DerniereReussie(ctx, c.db)
	if c.opt.Metriques != nil {
		b, a := c.opt.Metriques.Taille()
		d.Metriques = b + a
	}
	if c.opt.Logs != nil {
		d.Logs = c.opt.Logs.Taille()
	}
	for _, e := range d.Canaux {
		switch e.Role {
		case "principal":
			d.Principal = e
		case "secours":
			d.Secours = e
		}
	}
	if cons, err := LireConservation(ctx, c.db); err == nil {
		// Une partie sans plafond (0) : ni plafond ni jauge (relecture du
		// 21/11 : la place prise était comparée aux seuls plafonds posés).
		if cons.MaxBrut > 0 && cons.MaxAgrege > 0 && cons.MaxLogs > 0 {
			d.Plafond = cons.MaxBrut + cons.MaxAgrege + cons.MaxLogs
			d.PctStockage = min(100, int(100*(d.Metriques+d.Logs)/d.Plafond))
		}
	}
	d.Actives = d.Critiques + d.Avertissements + d.Infos
	d.Parc = c.parc(q, toutes)
	for _, l := range d.Parc {
		d.Nouveautes += l.Nouveautes
	}
	majEchec := map[string]string{}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		d.Chaleur = c.chaleurParc(ctx, ms, q.t)
		for _, m := range ms {
			if m.Statut == "approuvee" && m.MajEchec != "" {
				majEchec[m.Nom] = m.MajEchec
			}
		}
	}
	d.R = c.regarderAccueil(ctx, &d, majEchec, q.t)
	return d
}

// alertesEnCours : alertes en attente ou actives (machine = filtre).
func (c *Console) alertesEnCours(machine string) []resumeAlerte {
	if c.opt.Alertes == nil {
		return nil
	}
	var out []resumeAlerte
	var pour map[string]time.Duration // délais des règles, lus si une condition est en attente
	prises := c.prisesEnCharge(context.Background())
	loc := fuseau.Load()
	for _, a := range c.opt.Alertes.EnCours() {
		if machine != "" && a.Machine() != machine {
			continue
		}
		r := resumeAlerte{ID: a.ID, Regle: a.Regle, Gravite: a.Gravite, Machine: a.Machine(), Resume: a.Resume,
			Etat: a.Etat, Depuis: a.Debut.UnixMilli()}
		if !a.AbsenteDepuis.IsZero() {
			r.Absente = dureeLisible(c.maintenant().Sub(a.AbsenteDepuis))
		}
		if p, ok := prises[a.ID]; ok {
			r.Acquittee, r.AcquitteePar = true, p.Par
			r.AcquitteeLe = time.UnixMilli(p.Le).In(loc).Format("15:04")
			if p.Jusqua > 0 {
				r.AcquitteeJusqua = time.UnixMilli(p.Jusqua).In(loc).Format("02/01 15:04")
			}
		}
		if a.Etat == "en_attente" {
			if pour == nil {
				pour = map[string]time.Duration{}
				if rs, err := alertes.ListerRegles(context.Background(), c.db); err == nil {
					for _, x := range rs {
						pour[x.Nom] = time.Duration(x.Pour)
					}
				}
			}
			if reste := time.Until(a.Debut.Add(pour[a.Regle])); reste > time.Minute {
				r.Reste = "dans " + dureeLisible(reste)
			} else {
				r.Reste = "bientôt"
			}
		}
		if c.opt.Aiguilleur != nil {
			r.Sourdine = c.opt.Aiguilleur.RaisonSourdine(&a)
		}
		out = append(out, r)
	}
	rang := map[string]int{alertes.Critique: 0, alertes.Avertissement: 1, alertes.Info: 2}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Etat != out[j].Etat {
			return out[i].Etat == "active"
		}
		if rang[out[i].Gravite] != rang[out[j].Gravite] {
			return rang[out[i].Gravite] < rang[out[j].Gravite]
		}
		return out[i].Depuis < out[j].Depuis
	})
	return out
}

// --- Machines --------------------------------------------------------------------

type vueMachine struct {
	flotte.InfoMachine
	Muette   bool
	AValider int // éléments détectés « à valider » (SPEC § 19)
	// Liste (SPEC § 36) : collecteur qui relève cet équipement, et
	// équipement affiché juste sous son collecteur.
	ReleveePar string
	Sous       bool
	// Machine en attente (SPEC § 36) : nom d'hôte trop commun, et demande
	// venue d'un réseau où aucune machine approuvée n'a été vue.
	NomBanal, AdresseInconnue bool
}

// donneesMachines : page Machines (liste) ; l'ajout est sur sa propre page
// (donneesAjout, /machines/ajouter).
type donneesMachines struct {
	EnAttente, Autres []vueMachine
	TotalAttente      int  // machines en attente (toutes)
	AttenteCachees    int  // en attente au-delà des premières (lien « Voir les N autres »)
	ToutesAttente     bool // ?attente=toutes
	Listes            ListesMachines
	Approuvees        int // nombre total, sans filtre
	Sites             int // nombre de sites différents
	Filtres           filtresPage
	Compteurs         compteursFiltres
	Groupes           []groupeMachines
	Cases             casesMachines // cases du haut (SPEC § 120)
}

// donneesAjout : page « Ajouter une machine » (SPEC § 36), en étapes.
type donneesAjout struct {
	Quoi      string         // agent, collecteur, equipement
	Ingestion string         // adresse d'ingestion (Réglages › Interface), vide = à renseigner
	Listes    ListesMachines // sites, types et groupes proposés (Réglages)
	// Collecteurs approuvés (« Relevé par » d'un équipement).
	Collecteurs []string
	// Jeton tout juste créé (affiché une seule fois) et options du script
	// (« --nom », « --sans-logs », « --auto-update »…).
	Jeton, OptionEnvoi string
	// Équipement sans agent (OPNsense / Telegraf) tout juste créé : son
	// token n'est affiché qu'ici, une seule fois.
	Equipement, TokenEquipement string
	EnAttente                   int // machines en attente d'approbation
}

// quoiAjout : ce qu'on ajoute, ramené aux valeurs admises.
func quoiAjout(s string) string {
	switch s {
	case "collecteur", "equipement":
		return s
	}
	return "agent"
}

func (c *Console) pageMachines(q *req) {
	c.rendre(q, "machines", "Machines", "machines", c.donneesMachines(q))
}

func (c *Console) donneesMachines(q *req) donneesMachines {
	ctx := q.r.Context()
	ms, err := flotte.ListerMachines(ctx, c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	u := q.r.URL.Query()
	d := donneesMachines{Filtres: lireFiltresMachines(u.Get("vue"), u.Get("filtre"), u.Get("q")), ToutesAttente: u.Get("attente") == "toutes"}
	var aValider map[string]int
	if c.lecteur != nil {
		aValider = c.aValider(ctx, q.t)
	}
	releves, _ := flotte.RelevesParCollecteur(ctx, c.db)
	var approuvees []vueMachine
	sites := map[string]bool{}
	for _, m := range ms {
		v := vueMachine{InfoMachine: m, Muette: q.t.Sub(time.Unix(m.VuLe, 0)) > delaiMuette, AValider: aValider[m.Nom],
			ReleveePar: releves[m.Nom]}
		switch m.Statut {
		case "en_attente":
			d.EnAttente = append(d.EnAttente, v)
		case "approuvee":
			approuvees = append(approuvees, v)
			if m.Site != "" {
				sites[m.Site] = true
			}
		default:
			d.Autres = append(d.Autres, v)
		}
	}
	// En attente : la plus ancienne demande d'abord, avec ses deux signaux.
	connus := reseauxConnus(approuvees)
	slices.SortStableFunc(d.EnAttente, func(a, b vueMachine) int { return cmp.Compare(a.CreeLe, b.CreeLe) })
	for i := range d.EnAttente {
		m := &d.EnAttente[i]
		m.NomBanal = !m.NomDuJeton && nomBanal(m.Nom)
		m.AdresseInconnue = adresseInconnue(m.IPEnrolement, connus)
	}
	d.TotalAttente = len(d.EnAttente)
	if !d.ToutesAttente && len(d.EnAttente) > attenteVisibles {
		d.AttenteCachees = len(d.EnAttente) - attenteVisibles
		d.EnAttente = d.EnAttente[:attenteVisibles]
	}
	d.Approuvees, d.Sites = len(approuvees), len(sites)
	d.Compteurs = d.Filtres.compter(approuvees)
	d.Groupes = regrouperMachines(approuvees, d.Filtres)
	d.Listes = c.lireListes(ctx)
	// Tuiles et cases (SPEC § 120) : mesures de la vue d'ensemble.
	mesures := map[string]*ligneParc{}
	if c.lecteur != nil {
		actives, _ := separerAttente(c.alertesEnCours(""))
		for _, l := range c.parc(q, actives) {
			mesures[l.Nom] = &l
		}
	}
	for i := range d.Groupes {
		d.Groupes[i].Tuiles = tuilesDe(d.Groupes[i].Machines, mesures)
	}
	d.Cases = calculerCases(approuvees, mesures, d.TotalAttente)
	return d
}

// pageAjouter : ajout d'une machine, d'un collecteur ou d'un équipement
// (SPEC § 36). Réservée aux administrateurs (elle crée des jetons).
func (c *Console) pageAjouter(q *req) {
	c.rendre(q, "machines-ajouter", "Ajouter une machine", "machines", c.donneesAjout(q, q.r.URL.Query().Get("quoi")))
}

func (c *Console) donneesAjout(q *req, quoi string) donneesAjout {
	ctx := q.r.Context()
	d := donneesAjout{Quoi: quoiAjout(quoi)}
	d.Ingestion = lireInterface(ctx, c.db).Ingestion
	d.Listes = c.lireListes(ctx)
	d.Collecteurs, _ = flotte.Collecteurs(ctx, c.db)
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			if m.Statut == "en_attente" {
				d.EnAttente++
			}
		}
	}
	return d
}

// postEquipement : équipement sans agent qui envoie au format InfluxDB
// (OPNsense avec Telegraf, SPEC § 23.10) : machine approuvée tout de suite,
// token affiché une seule fois. Refusé pour un nom existant : on ne
// remplace jamais le token d'une machine en service par ce formulaire.
func (c *Console) postEquipement(q *req) {
	ctx := q.r.Context()
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	nom := strings.ToLower(f("nom"))
	var token string
	err := c.verifierCode(q)
	if err == nil {
		switch {
		case !flotte.FormatNom.MatchString(nom):
			err = errors.New("nom invalide (lettres, chiffres, « . », « _ », « - »)")
		default:
			if _, e := flotte.IDMachine(ctx, c.db, nom); !errors.Is(e, flotte.ErrMachineInconnue) {
				err = fmt.Errorf("la machine %q existe déjà : choisir un autre nom", nom)
			}
		}
	}
	if err == nil {
		err = c.lireListes(ctx).verifierChoix(f("site"), f("env"), f("groupe"), "", "", "")
	}
	if err == nil {
		if _, e := ingestion.LireIPs(f("ips")); e != nil {
			err = fmt.Errorf("adresses autorisées : %w", e)
		}
	}
	// Relevé par un collecteur (SPEC § 23.13) : pas de token, l'équipement
	// est rattaché au collecteur choisi.
	if par := f("collecteur"); err == nil && par != "" {
		var idC int64
		if idC, err = flotte.IDMachine(ctx, c.db, par); err == nil {
			err = flotte.CreerEquipement(ctx, c.db, nom, idC, q.qui(), q.t)
		}
		if err == nil {
			err = flotte.DefinirEtiquettes(ctx, c.db, nom, f("site"), f("env"), f("groupe"), q.qui(), q.t)
			if c.opt.Ingestion != nil {
				c.opt.Ingestion.OublierEquipements()
			}
		}
		c.retour(q, "/machines/"+url.PathEscape(nom), err, "équipement « "+nom+" » créé et rattaché au collecteur "+par)
		return
	}
	if err == nil {
		token, err = flotte.TokenManuel(ctx, c.db, nom, q.qui(), q.t)
	}
	if err == nil {
		err = errors.Join(flotte.DefinirIPs(ctx, c.db, nom, f("ips"), q.qui(), q.t),
			flotte.DefinirEtiquettes(ctx, c.db, nom, f("site"), f("env"), f("groupe"), q.qui(), q.t))
	}
	if err != nil && token == "" {
		c.retour(q, "/machines/ajouter?quoi=equipement", err, "")
		return
	}
	d := c.donneesAjout(q, "equipement")
	d.Equipement, d.TokenEquipement = nom, token
	if err != nil {
		c.flash(q, "", "équipement créé, mais : "+err.Error())
	} else {
		c.flash(q, "équipement « "+nom+" » créé : copier son token maintenant, il n'est affiché qu'ici", "")
	}
	c.rendre(q, "machines-ajouter", "Ajouter une machine", "machines", d)
}

// postJeton : jeton d'enrôlement à usage unique, affiché une seule fois.
func (c *Console) postJeton(q *req) {
	err := c.verifierCode(q)
	var j string
	if err == nil {
		var d alertes.Duree
		if d, err = alertes.LireDuree(q.r.PostFormValue("duree")); err == nil {
			f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
			if err = c.lireListes(q.r.Context()).verifierChoix(f("site"), f("env"), f("groupe"), "", "", ""); err == nil {
				j, err = flotte.CreerJetonEtiquettes(q.r.Context(), c.db, time.Duration(d),
					flotte.Etiquettes{Site: f("site"), Env: f("env"), Groupe: f("groupe"), Nom: strings.ToLower(f("nom")),
						TypeMachine: f("type_machine")}, q.qui(), q.t)
			}
		}
	}
	quoi := quoiAjout(q.r.PostFormValue("type_machine"))
	if err != nil {
		c.retour(q, "/machines/ajouter?quoi="+quoi, err, "")
		return
	}
	// Page affichée directement (pas de redirection) : le jeton n'est
	// jamais gardé en session ni en base en clair. Recharger la page ne
	// recrée rien (le code 2FA ne sert qu'une fois).
	d := c.donneesAjout(q, quoi)
	d.Jeton = j
	collecteur := d.Quoi == "collecteur"
	// Ce que la machine enverra (modifiable ensuite : Configuration des agents).
	var options []string
	// Nom choisi : repris dans la commande, pour que le journal de la machine
	// porte le même nom que l'interface (sinon : son nom d'hôte). Déjà
	// vérifié (lettres, chiffres, « . », « _ », « - ») : sûr dans la commande.
	if nom := strings.ToLower(strings.TrimSpace(q.r.PostFormValue("nom"))); nom != "" && flotte.FormatNom.MatchString(nom) {
		options = append(options, "--nom", nom)
	}
	switch q.r.PostFormValue("envoi") {
	case "metriques":
		if collecteur {
			break // le collecteur ne lit pas les logs de sa machine
		}
		options = append(options, "--sans-logs")
	case "logs":
		if collecteur {
			break
		}
		options = append(options, "--sans-metriques")
	}
	// Mises à jour automatiques signées : décidées sur la machine (option du
	// script, fichier local root), jamais par le serveur seul.
	if q.r.PostFormValue("maj_auto") != "" {
		options = append(options, "--auto-update")
	}
	d.OptionEnvoi = strings.Join(options, " ")
	c.flash(q, "jeton d'enrôlement créé : à utiliser une seule fois, il n'est affiché qu'ici", "")
	c.rendre(q, "machines-ajouter", "Ajouter une machine", "machines", d)
}

// definirEtiquettes : site et type pris dans les listes (Réglages › Sites et
// types) ; la valeur actuelle de la machine reste permise.
func (c *Console) definirEtiquettes(q *req, nom string, f func(string) string) error {
	ctx := q.r.Context()
	m, ok := c.machine(q)
	if !ok {
		return errors.New("machine inconnue")
	}
	site, env, groupe := strings.TrimSpace(f("site")), strings.TrimSpace(f("env")), strings.TrimSpace(f("groupe"))
	l := c.lireListes(ctx)
	if err := l.verifierChoix(site, env, groupe, m.Site, m.Env, m.Groupe); err != nil {
		return err
	}
	if err := flotte.DefinirEtiquettes(ctx, c.db, nom, site, env, groupe, q.qui(), q.t); err != nil {
		return err
	}
	// Site ou type proposé par la machine et gardé par l'admin : ajouté aux
	// listes (proposé ensuite pour les autres machines).
	ajout := false
	if site != "" && !slices.Contains(l.Sites, site) {
		l.Sites, ajout = append(l.Sites, site), true
	}
	if env != "" && !slices.Contains(l.Types, env) {
		l.Types, ajout = append(l.Types, env), true
	}
	if ajout {
		return c.ecrireListes(ctx, l, q)
	}
	return nil
}

type donneesMachine struct {
	Listes      ListesMachines
	NomsAnciens []string // historique rattaché à la machine
	M           vueMachine
	Alertes     []resumeAlerte
	Attente     []resumeAlerte        // conditions pas encore des alertes
	Prises      []resumeAlerte        // prises en charge (SPEC § 30)
	Masquees    []resumeAlerte        // conséquences d'une alerte plus grave (bloc replié)
	Maintenant  *blocMaintenant       // « En ce moment », en direct si possible (SPEC § 29, § 97)
	Gourmands   *vueGourmands         // conteneurs les plus gourmands (SPEC § 31)
	Services    *vueServices          // bloc « Services » (SPEC § 42.2)
	Maj         vueMaj                // bloc « Mise à jour » (SPEC § 44)
	Paquets     *vuePaquets           // bloc « Paquets à mettre à jour » (SPEC § 42.3)
	Taches      *donneesTaches        // bloc « Tâches planifiées » (SPEC § 80)
	Changements *vueChangements       // bloc « Changements » (SPEC § 47)
	Sondes      *vueSondesMachine     // bloc « Sondes » (SPEC § 60)
	Dernieres   *blocDernieresAlertes // « Dernières alertes » (SPEC § 72)
	Element     *blocElementMachine   // liens et étiquettes (SPEC § 56, § 59)
	Rangees     []vueRangee
	Periode     string
	Periodes    []string
	Versions    []string
	Surv        vueSurveillance // bloc « À surveiller sur cette machine »
	Identite    identiteMachine // bandeau sous le nom
	Agent       bool            // machine avec agent (pas un collecteur)
	// Direct (SPEC § 97) : "ok" (bloc « En direct »), "ancien" (agent trop
	// ancien : le bloc le dit), "" (pas de direct : collecteur, serveur sans direct).
	Direct    string
	Modeles   vueModeles // bloc « Modèles » (SPEC § 24)
	Ingestion string     // adresse d'ingestion (commande de réinstallation)
	// Collecteur (SPEC § 23.13) : équipements relevés et machines qu'on peut
	// lui rattacher ; pour un équipement : le collecteur qui le relève.
	Collecteur  bool
	Equipements []flotte.Equipement
	Candidats   []string
	RelevePar   string
	Syslog      vueSyslog   // bloc « Syslog TLS » d'un collecteur (SPEC § 23.14)
	SNMP        vueSNMP     // bloc « SNMP et sondes » d'un collecteur (SPEC § 23.15)
	Collecte    vueCollecte // onglet « Collecte » : tableau et cartes (SPEC § 119)
	Token       vueToken    // bloc « Token » : actif ou non, dernier envoi, dernier refus
}

// vueToken : état du token d'une machine (retour du 22/11 : « on ne voit
// pas s'il y en a un de configuré », et Telegraf d'OPNsense refusé sans que
// la page le dise). Envoi et refus : depuis le démarrage du serveur.
type vueToken struct {
	Actifs       int
	CreeLe       int64 // token actif le plus récent
	DernierEnvoi int64 // dernières métriques reçues avec son propre token (0 : aucune)
	Refus        *ingestion.Refus
}

func (c *Console) machine(q *req) (flotte.InfoMachine, bool) {
	nom := q.r.PathValue("nom")
	ms, err := flotte.ListerMachines(q.r.Context(), c.db)
	if err == nil {
		for _, m := range ms {
			if m.Nom == nom {
				return m, true
			}
		}
	}
	return flotte.InfoMachine{}, false
}

func (c *Console) pageMachine(q *req) {
	m, ok := c.machine(q)
	if !ok {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Machine introuvable", "machines", nil)
		return
	}
	ctx := q.r.Context()
	d := donneesMachine{Listes: c.lireListes(q.r.Context()), M: vueMachine{InfoMachine: m, Muette: q.t.Sub(time.Unix(m.VuLe, 0)) > delaiMuette},
		Periodes: periodesProposees}
	d.Alertes, d.Attente = separerAttente(c.alertesEnCours(m.Nom))
	d.Alertes, d.Prises = separerPrises(d.Alertes)
	d.Alertes, d.Masquees = separerMasquees(d.Alertes)
	d.NomsAnciens, _ = flotte.AnciensNoms(ctx, c.db, m.ID)
	d.Dernieres = c.dernieresAlertes(ctx, alertes.Filtre{Machine: m.Nom}, "/alertes?machine="+url.QueryEscape(m.Nom), q.t)
	if m.Type == "agent" && m.Statut == "approuvee" && c.opt.Direct != nil {
		d.Direct = "ancien"
		if flotte.ACapacite(m.Capacites, protocole.CapaciteDirect) {
			d.Direct = "ok"
		}
	}
	if d.Agent = m.Type == "agent" && m.Statut == "approuvee"; d.Agent {
		d.Maintenant = blocEnCeMoment(m.Nom, d.Direct, c.enCeMoment(ctx, m, q.t))
		d.Gourmands = c.gourmands(ctx, m, q.t)
		var ech []alertes.Echantillon
		d.Surv, ech = c.surveillance(ctx, m, q.t)
		d.Identite = c.identite(ctx, m, ech, q.t)
		d.Services = c.servicesMachine(ctx, m, d.Surv, q.t)
		d.Paquets = c.paquetsMachine(ctx, m, q.t)
		d.Taches = c.tachesMachine(q, m.Nom)
		d.Changements = c.changementsMachine(ctx, m.Nom, q.t)
		_, releveur, _ := flotte.CollecteurDe(ctx, c.db, m.ID)
		d.Sondes = c.sondesMachine(ctx, m, releveur, q.t)
		d.Element = c.elementMachine(ctx, m.Nom)
		if d.Paquets != nil {
			// Ouvert d'office quand l'alerte des mises à jour de sécurité est en cours.
			for _, a := range d.Alertes {
				if a.Regle == "MisesAJourSecuriteEnAttente" {
					d.Paquets.Ouvrir = true
				}
			}
		}
		d.Modeles = c.vueModelesMachine(ctx, m, q.t)
		d.Ingestion = lireInterface(ctx, c.db).Ingestion
	}
	if (m.Type == "agent" || m.Type == "collecteur") && m.Statut == "approuvee" {
		d.Maj = c.majMachine(ctx, m, q.t)
	}
	if m.Type == "collecteur" {
		d.Collecteur = true
		d.Equipements, _ = flotte.EquipementsDe(ctx, c.db, m.ID)
		d.Candidats = c.candidatsEquipement(ctx, m.ID)
		d.Ingestion = lireInterface(ctx, c.db).Ingestion
		d.Syslog = c.syslogAvecAvancement(ctx, m, q.t)
		d.SNMP = c.snmpDe(ctx, m)
		d.Collecte = c.collecteDe(ctx, m, d.Equipements, d.Syslog, d.SNMP, q.r.URL.Query().Get("pour"), q.r.URL.Query().Get("ajout"), q.r.URL.Query().Get("snmp"), q.t)
	} else {
		_, d.RelevePar, _ = flotte.CollecteurDe(ctx, c.db, m.ID)
	}
	d.Token = c.vueTokenDe(ctx, m)
	// Tableau de la page : celui de l'équipement s'il est relevé par un
	// collecteur (NAS Synology en SNMP, contrôleur UniFi), sinon les courbes
	// Linux de l'agent. Retour du 22/11 : la page d'un équipement montrait
	// les courbes Linux, vides, alors que ses données arrivaient.
	// Équipement sans agent (aucune version) : OPNsense par Telegraf aussi
	// (retour du 22/11 : « comment remonter les métriques de fw ? »).
	nomTableau, variable, valeur := "serveurs-linux", "instance", m.Nom
	if d.RelevePar != "" || m.Version == "" {
		nomTableau, variable, valeur = c.tableauEquipement(ctx, m.Nom, q.t)
	}
	if t, _ := c.tableauOuCatalogue(nomTableau); t != nil && m.Type == "agent" {
		v := url.Values{"periode": {q.r.URL.Query().Get("periode")}}
		if variable != "" {
			v.Set(variable, valeur)
		}
		vals, periode, duree, err := lireFiltres(t, v)
		if err == nil {
			d.Periode = periode
			garder := map[string]bool{}
			for _, r := range t.Rangees {
				if r.Titre != "Vue d'ensemble" {
					garder[r.Titre] = true
				}
			}
			d.Rangees = c.evaluerTableau(ctx, t, vals, periode, duree, q.t, garder)
			// Courbes de la machine : avec ses alertes et événements (SPEC § 31).
			for ri := range d.Rangees {
				for pi := range d.Rangees[ri].Panneaux {
					if vp := &d.Rangees[ri].Panneaux[pi]; vp.Courbe != "" {
						vp.Courbe += "&annoter=1"
					}
				}
			}
		}
	}
	if vs, err := flotte.ListerVersions(ctx, c.db); err == nil {
		vu := map[string]bool{}
		for _, v := range vs {
			if !vu[v.Version] {
				vu[v.Version] = true
				d.Versions = append(d.Versions, v.Version)
			}
		}
	}
	c.rendre(q, "machine", m.Nom, "machines", d)
}

// tableauEquipement : tableau d'un équipement relevé par un collecteur,
// choisi d'après ses mesures, et la variable qui le filtre sur lui (vide :
// pas de filtre). Sans mesure reconnue : courbes Linux (agent).
func (c *Console) tableauEquipement(ctx context.Context, nom string, t time.Time) (tableau, variable, valeur string) {
	for _, x := range []struct{ expr, tableau, variable string }{
		{`count(systemStatus{job="snmp", instance=` + strconv.Quote(nom) + `})`, "synology", "nas"},
		{`count(up{job="unifi", instance=` + strconv.Quote(nom) + `})`, "unifi", ""},
	} {
		if e, err := c.instant(ctx, x.expr, t); err == nil && len(e) > 0 {
			return x.tableau, x.variable, nom
		}
	}
	// OPNsense (Telegraf) : le tableau filtre sur l'étiquette « host » que
	// Telegraf pose (nom d'hôte du pare-feu), pas sur le nom dans mirador.
	if e, err := c.instant(ctx, `max by (host) (last_over_time(cpu_usage_idle{cpu="cpu-total", instance=`+strconv.Quote(nom)+`}[15m]))`, t); err == nil && len(e) > 0 {
		if h := e[0].Etiquettes.Get("host"); h != "" {
			return "opnsense", "host", h
		}
	}
	return "serveurs-linux", "instance", nom
}

// vueTokenDe : état du token d'une machine.
func (c *Console) vueTokenDe(ctx context.Context, m flotte.InfoMachine) vueToken {
	v := vueToken{Actifs: m.TokensActifs}
	if m.TokensActifs > 0 {
		v.CreeLe, _ = flotte.TokenCreeLe(ctx, c.db, m.ID)
	}
	if c.opt.Ingestion != nil {
		v.DernierEnvoi, _ = c.opt.Ingestion.DerniereReception(m.ID)
		if r, ok := c.opt.Ingestion.DernierRefus(m.ID); ok && r.T > v.DernierEnvoi {
			v.Refus = &r // refus plus récent que le dernier envoi réussi
		}
	}
	return v
}

// candidatsEquipement : machines approuvées qu'on peut rattacher à ce
// collecteur (ni collecteur, ni déjà rattachées à lui).
func (c *Console) candidatsEquipement(ctx context.Context, collecteur int64) []string {
	ms, err := flotte.ListerMachines(ctx, c.db)
	if err != nil {
		return nil
	}
	deja := map[string]bool{}
	if eqs, err := flotte.EquipementsDe(ctx, c.db, collecteur); err == nil {
		for _, q := range eqs {
			deja[q.Nom] = true
		}
	}
	var out []string
	for _, m := range ms {
		if m.Statut == "approuvee" && m.Type != "collecteur" && !deja[m.Nom] {
			out = append(out, m.Nom)
		}
	}
	return out
}

// postMachine : toutes les actions sur une machine.
func (c *Console) postMachine(q *req) {
	ctx := q.r.Context()
	nom := q.r.PathValue("nom")
	action := q.r.PostFormValue("action")
	vers := "/machines/" + url.PathEscape(nom)
	f := q.r.PostFormValue
	ok := "fait"
	var err error
	// Actions sensibles (brief) : code 2FA.
	// « surveillance » change la configuration de l'agent : même règle que la
	// page « Configuration des agents » (revue de sécurité du 11/11).
	sensible := map[string]bool{"token": true, "revoquer": true, "supprimer": true, "refuser": true, "ips": true, "cible": true,
		"surveillance": true, "empreinte": true,
		// Modèles : changent la configuration de l'agent et les alertes.
		"modeles": true,
		// Équipements d'un collecteur : changent ce qu'il a le droit d'envoyer.
		"rattacher": true, "detacher": true,
		// Mise à jour forcée (SPEC § 44) : installe la version assignée hors fenêtre.
		"maj_forcer": true,
		// Syslog et certificat d'un collecteur (SPEC § 23.14).
		"syslog": true, "syslog_retirer": true, "certificat_renouveler": true,
		// SNMP et sondes d'un collecteur (SPEC § 23.15).
		"snmp": true, "snmp_retirer": true, "sondes": true, "unifi": true, "unifi_retirer": true,
		"sonde_ajouter": true, "sonde_retirer": true, "emetteur_ajouter": true, "emetteur_retirer": true}
	if sensible[action] {
		if err = c.verifierCode(q); err != nil {
			c.retour(q, vers, err, "")
			return
		}
	}
	if action == "modeles" {
		m, trouvee := c.machine(q)
		if !trouvee {
			c.retour(q, "/machines", errors.New("machine inconnue"), "")
			return
		}
		n, err := c.enregistrerModelesMachine(q, m)
		c.retour(q, vers+"#modeles", err, "modèles enregistrés ("+strconv.Itoa(n)+" changement(s)) : configuration envoyée à l'agent, règles actives sous une minute")
		return
	}
	switch action {
	case "actualiser":
		// Relecture de la configuration et relance de l'assistant root
		// (SPEC § 22) : rien de modifié, pas de code 2FA.
		var id int64
		if id, err = flotte.IDMachine(ctx, c.db, nom); err == nil {
			flotte.DemanderActualisation(id, q.t)
			ok = "actualisation demandée : la machine relit sa configuration et relance l'assistant root (environ 1 min, puis recharger la page)"
		}
	case "approuver":
		// Nom, site, type et groupe : ceux du jeton (ou proposés par la
		// machine), corrigeables ici avant d'approuver.
		if nouveau := strings.ToLower(strings.TrimSpace(f("nouveau_nom"))); nouveau != "" && nouveau != nom {
			if err = flotte.Renommer(ctx, c.db, nom, nouveau, q.qui(), q.t); err == nil {
				c.lireNomsAnciens(ctx)
				c.lireNomsAnciens(ctx)
				nom, vers = nouveau, "/machines"
				q.r.SetPathValue("nom", nouveau)
			}
		}
		if err == nil && q.r.PostForm.Has("site") {
			err = c.definirEtiquettes(q, nom, f)
		}
		if err == nil {
			err = flotte.Approuver(ctx, c.db, nom, f("avec_ip") != "", q.qui(), q.t)
		}
		ok = "machine approuvée : elle reçoit son token à sa prochaine demande"
	case "refuser":
		err = flotte.Refuser(ctx, c.db, nom, q.qui(), q.t)
		ok = "machine refusée"
	case "etiquettes":
		err = c.definirEtiquettes(q, nom, f)
	case "empreinte":
		lier := f("lier") == "oui"
		err = flotte.LierEmpreinte(ctx, c.db, nom, lier, q.qui(), q.t)
		ok = "token lié à cette machine : il sera refusé s'il est présenté par une autre"
		if !lier {
			ok = "le token n'est plus lié à la machine"
		}
	case "surveillance":
		if m, trouve := c.machine(q); !trouve {
			err = flotte.ErrMachineInconnue
		} else {
			err = c.postSurveillance(q, m)
			c.oublierAValider() // compteur du menu recalculé
		}
		vers += "#surveillance"
		ok = "choix enregistrés : la machine les applique à sa prochaine demande (5 min au plus)"
	case "renommer":
		nouveau := strings.TrimSpace(f("nouveau"))
		if err = flotte.Renommer(ctx, c.db, nom, nouveau, q.qui(), q.t); err == nil {
			c.lireNomsAnciens(ctx)
			vers = "/machines/" + url.PathEscape(nouveau)
			ok = "machine renommée : les nouvelles données arrivent sous ce nom (l'historique garde l'ancien)"
		}
	case "ips":
		err = flotte.DefinirIPs(ctx, c.db, nom, f("ips"), q.qui(), q.t)
		ok = "adresses autorisées enregistrées"
	case "revoquer":
		err = flotte.Revoquer(ctx, c.db, nom, q.qui(), q.t)
		ok = "tokens révoqués : la machine ne peut plus envoyer"
	case "token":
		var tok string
		if tok, err = flotte.TokenManuel(ctx, c.db, nom, q.qui(), q.t); err == nil {
			ok = "nouveau token (affiché une seule fois ; l'ancien est révoqué) : " + tok
		}
	case "maj_forcer":
		var id int64
		vers += "#maj"
		if id, err = flotte.IDMachine(ctx, c.db, nom); err == nil {
			if err = flotte.ForcerMaj(ctx, c.db, id, q.qui(), q.t); err == nil {
				ok = "mise à jour demandée : l'agent vérifie tout de suite (moins d'une minute), télécharge la version assignée, la vérifie (signature) et redémarre : la nouvelle version s'affiche dès son premier battement. Recharger la page dans une minute."
			}
		}
	case "cible":
		var id int64
		if id, err = flotte.IDMachine(ctx, c.db, nom); err == nil {
			v := f("version")
			if v == "bloquer" {
				m, _ := c.machine(q)
				v = m.Version
				if v == "" {
					err = errors.New("version actuelle inconnue (aucun battement)")
					break
				}
				ok = "mises à jour bloquées sur cette machine (version " + v + " gardée)"
			}
			err = flotte.DefinirCible(ctx, c.db, id, v, q.qui(), q.t)
		}
	case "syslog", "syslog_retirer", "certificat_renouveler", "emetteur_ajouter", "emetteur_retirer":
		m, trouve := c.machine(q)
		if !trouve || m.Type != "collecteur" {
			err = errors.New("ce n'est pas un collecteur")
			break
		}
		vers += "#syslog"
		ok, err = c.postSyslog(q, m, action)
	case "snmp", "snmp_retirer", "sondes", "unifi", "unifi_retirer", "sonde_ajouter", "sonde_retirer":
		m, trouve := c.machine(q)
		if !trouve || m.Type != "collecteur" {
			err = errors.New("ce n'est pas un collecteur")
			break
		}
		// Ancre : la carte de la méthode (nouvelle page du 22/11).
		vers += map[string]string{"unifi": "#unifi", "unifi_retirer": "#unifi", "sondes": "#sondes-cert",
			"sonde_ajouter": "#sondes-cert", "sonde_retirer": "#sondes-cert"}[action]
		if !strings.Contains(vers, "#") {
			vers += "#snmp"
		}
		ok, err = c.postSNMP(q, m, action)
	case "rattacher", "detacher":
		// Page du collecteur : l'équipement choisi ; page de l'équipement :
		// lui-même (détacher).
		var idC, idE int64
		eq := strings.TrimSpace(f("equipement"))
		if eq == "" {
			eq = nom
		}
		if idE, err = flotte.IDMachine(ctx, c.db, eq); err != nil {
			break
		}
		vers += "#equipements"
		if action == "detacher" {
			err = flotte.DetacherEquipement(ctx, c.db, idE, q.qui(), q.t)
			ok = eq + " n'est plus relevé par un collecteur"
			break
		}
		if idC, err = flotte.IDMachine(ctx, c.db, nom); err == nil {
			err = flotte.RattacherEquipement(ctx, c.db, idC, idE, q.qui(), q.t)
			ok = eq + " rattaché : le collecteur reçoit sa nouvelle liste tout de suite"
		}
	case "supprimer":
		err = flotte.Supprimer(ctx, c.db, nom, q.qui(), q.t)
		if err == nil && f("historique") != "" && c.opt.Metriques != nil {
			err = c.opt.Metriques.Effacer(ctx, labels.MustNewMatcher(labels.MatchEqual, "instance", nom))
			ok = "machine et historique des métriques supprimés (les logs partent avec leur rétention)"
		} else if err == nil {
			ok = "machine supprimée (historique gardé)"
		}
		vers = "/machines"
	default:
		err = errors.New("action inconnue")
	}
	if err == nil && c.opt.Ingestion != nil {
		c.opt.Ingestion.Auth().Oublier() // effet immédiat sur les tokens
		// Liste des équipements par collecteur relue aussi après un
		// détachement ou une suppression (relecture du 21/11 : seul le
		// rattachement la vidait).
		if action == "rattacher" || action == "detacher" || action == "supprimer" {
			c.opt.Ingestion.OublierEquipements()
		}
	}
	c.retour(q, vers, err, ok)
}

// --- Mises à jour des agents --------------------------------------------------------

type donneesMaj struct {
	Versions []flotte.VersionPubliee
	Cible    string
	Fenetre  flotte.Fenetre
	Machines []flotte.InfoMachine
	Cles     []cleVue // clés publiques de signature (posées sur les machines à l'installation)
	Depot    string   // dépôt GitHub des versions signées
	Choix    []string // versions publiées, sans doublon (une ligne par architecture dans Versions)
	Import   vueImport
	Garder   int // versions récentes gardées (réglage, 1 à 10)
	// Dernières versions publiées sur GitHub (SPEC § 37), proposées dans
	// une liste ; ListeGitHub : message si la liste n'a pas pu être lue.
	Proposees   []versionProposee
	ListeGitHub string
	Vue         vueMajParc // page revue le 21/11 (SPEC § 109)
}

// versionProposee : une version de GitHub dans la liste d'import.
type versionProposee struct {
	Tag, Date string
	Importee  bool // déjà publiée ici
}

// versionsProposees : nombre de versions de GitHub proposées (choix du 16/11).
const versionsProposees = 5

// cacheVersionsGitHub : liste gardée 10 min (1 min après un échec) pour ne
// pas interroger GitHub à chaque affichage de la page.
type cacheVersionsGitHub struct {
	mu     sync.Mutex
	depot  string
	vs     []flotte.VersionGitHub
	err    error
	expire time.Time
}

// versionsGitHub : dernières versions du dépôt, depuis le cache si possible.
func (c *Console) versionsGitHub(ctx context.Context, depot string, t time.Time) ([]flotte.VersionGitHub, error) {
	c.versionsGH.mu.Lock()
	defer c.versionsGH.mu.Unlock()
	if c.versionsGH.depot == depot && t.Before(c.versionsGH.expire) {
		return c.versionsGH.vs, c.versionsGH.err
	}
	vs, err := flotte.DernieresVersionsGitHub(ctx, c.sourceGitHub, depot, versionsProposees)
	duree := 10 * time.Minute
	if err != nil {
		duree = time.Minute
	}
	c.versionsGH.depot, c.versionsGH.vs, c.versionsGH.err, c.versionsGH.expire = depot, vs, err, t.Add(duree)
	return vs, err
}

type cleVue struct{ Valeur, Empreinte string }

// vueImport : dernier import depuis GitHub (affiché dans la page).
type vueImport struct {
	EnCours  bool
	Tag      string
	Resultat string
	Erreur   bool
	Le       time.Time
}

// etatImport : vueImport protégée (un seul import à la fois).
type etatImport struct {
	mu sync.Mutex
	v  vueImport
}

func (e *etatImport) lire() vueImport {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.v
}

// lancerImport : import en arrière-plan (téléchargement de ~20 Mo depuis
// GitHub : trop long pour une requête derrière nginx). Faux si un import est
// déjà en cours.
func (c *Console) lancerImport(depot, tag, qui string, toutes bool) bool {
	c.importMaj.mu.Lock()
	if c.importMaj.v.EnCours {
		c.importMaj.mu.Unlock()
		return false
	}
	c.importMaj.v = vueImport{EnCours: true, Tag: tag, Le: c.maintenant()}
	c.importMaj.mu.Unlock()
	go func() {
		ctx, fin := context.WithTimeout(context.Background(), 5*time.Minute)
		defer fin()
		// Seulement les architectures du parc, sauf demande contraire (SPEC § 22.6).
		src := c.sourceGitHub
		if !toutes {
			src.Architectures, _ = flotte.ArchitecturesParc(ctx, c.db)
		}
		archs, err := flotte.ImporterGitHub(ctx, c.db, filepath.Join(c.opt.Donnees, "versions"), src,
			depot, tag, qui+" (GitHub "+depot+")", c.maintenant())
		res := ""
		if len(archs) > 0 {
			res = "importée et signature vérifiée : " + strings.Join(archs, ", ")
		}
		if err != nil {
			if res != "" {
				res += " ; "
			}
			res += err.Error()
			c.journal.Warn("import d'une version depuis GitHub", "depot", depot, "version", tag, "erreur", err)
		} else {
			c.journal.Info("version importée depuis GitHub", "depot", depot, "version", tag, "architectures", archs)
		}
		c.importMaj.mu.Lock()
		c.importMaj.v = vueImport{Tag: tag, Resultat: res, Erreur: err != nil, Le: c.maintenant()}
		c.importMaj.mu.Unlock()
	}()
	return true
}

func (c *Console) pageMisesAJour(q *req) {
	ctx := q.r.Context()
	var d donneesMaj
	var e1, e2, e3, e4 error
	d.Versions, e1 = flotte.ListerVersions(ctx, c.db)
	d.Cible, e2 = flotte.CibleGlobale(ctx, c.db)
	d.Fenetre, e3 = flotte.LireFenetre(ctx, c.db)
	d.Machines, e4 = flotte.ListerMachines(ctx, c.db)
	vu := map[string]bool{}
	for _, v := range d.Versions {
		if !vu[v.Version] {
			vu[v.Version] = true
			d.Choix = append(d.Choix, v.Version)
		}
	}
	d.Import = c.importMaj.lire()
	var e7 error
	d.Garder, e7 = flotte.LireGarderVersions(ctx, c.db)
	var e6 error
	d.Depot, e6 = flotte.LireDepot(ctx, c.db)
	cles, e5 := flotte.LireClesPubliques(ctx, c.db)
	for _, k := range cles {
		d.Cles = append(d.Cles, cleVue{k, majagent.EmpreinteCle(k)})
	}
	if err := errors.Join(e1, e2, e3, e4, e5, e6, e7); err != nil {
		c.flash(q, "", err.Error())
	}
	// Liste des versions de GitHub : seulement pour les admins (eux seuls
	// importent) ; en cas d'échec, le champ à taper reste là.
	if q.admin() && e6 == nil {
		if vs, err := c.versionsGitHub(ctx, d.Depot, q.t); err != nil {
			d.ListeGitHub = "Liste des versions de GitHub indisponible (" + err.Error() + ") : taper le numéro"
		} else {
			for _, v := range vs {
				p := versionProposee{Tag: v.Tag, Importee: vu[strings.TrimPrefix(v.Tag, "v")] || vu[v.Tag]}
				if !v.Date.IsZero() {
					p.Date = v.Date.In(fuseau.Load()).Format("02/01/2006")
				}
				d.Proposees = append(d.Proposees, p)
			}
		}
	}
	filtre := q.r.URL.Query().Get("etat")
	if !slices.Contains([]string{majAJour, majRetard, majEchec}, filtre) {
		filtre = ""
	}
	construireVueMaj(&d, filtre)
	c.rendre(q, "maj", "Mises à jour des agents", "machines", d)
}

func (c *Console) postMisesAJour(q *req) {
	ctx := q.r.Context()
	err := c.verifierCode(q)
	ok := "fait"
	if err == nil {
		switch q.r.PostFormValue("action") {
		case "cible":
			err = flotte.DefinirCible(ctx, c.db, 0, q.r.PostFormValue("version"), q.qui(), q.t)
			ok = "version assignée à toutes les machines sans version propre"
		case "essayer":
			// Étape 2 (SPEC § 109) : la version sur une seule machine.
			var id int64
			if id, err = flotte.IDMachine(ctx, c.db, q.r.PostFormValue("machine")); err == nil {
				err = flotte.DefinirCible(ctx, c.db, id, q.r.PostFormValue("version"), q.qui(), q.t)
			}
			ok = "version assignée à " + q.r.PostFormValue("machine") + " : elle se met à jour dans la fenêtre horaire (ou tout de suite avec « Mettre à jour maintenant »)"
		case "forcer":
			var id int64
			if id, err = flotte.IDMachine(ctx, c.db, q.r.PostFormValue("machine")); err == nil {
				err = flotte.ForcerMaj(ctx, c.db, id, q.qui(), q.t)
			}
			ok = "mise à jour demandée à " + q.r.PostFormValue("machine") + " : elle vérifie dans la minute"
		case "depot":
			err = flotte.DefinirDepot(ctx, c.db, q.r.PostFormValue("depot"), q.qui(), q.t)
			ok = "dépôt des versions enregistré"
		case "importer":
			var depot string
			if depot, err = flotte.LireDepot(ctx, c.db); err == nil {
				// Version tapée à la main (« autre ») avant celle de la liste.
				tag := strings.TrimSpace(q.r.PostFormValue("tag_autre"))
				if tag == "" {
					tag = strings.TrimSpace(q.r.PostFormValue("tag"))
				}
				if !c.lancerImport(depot, tag, q.qui(), q.r.PostFormValue("toutes") != "") {
					err = errors.New("un import est déjà en cours")
				}
				ok = "import lancé : recharger la page dans quelques secondes"
			}
		case "versions_gardees":
			var n int
			if n, err = strconv.Atoi(q.r.PostFormValue("nombre")); err == nil {
				err = flotte.DefinirGarderVersions(ctx, c.db, n, q.qui(), q.t)
			} else {
				err = errors.New("versions gardées : un nombre de 1 à 10")
			}
			ok = "nombre de versions gardées enregistré (appliqué au prochain nettoyage, au plus tard dans 24 h)"
		case "supprimer_version":
			v := q.r.PostFormValue("version")
			err = flotte.SupprimerVersion(ctx, c.db, filepath.Join(c.opt.Donnees, "versions"), v, q.qui(), q.t)
			ok = "version " + v + " supprimée (réimportable depuis GitHub)"
		case "cles":
			err = flotte.DefinirClesPubliques(ctx, c.db, q.r.PostFormValue("cles"), q.qui(), q.t)
			ok = "clé publique enregistrée : elle sera posée sur les machines installées à partir de maintenant"
		case "fenetre":
			fe := flotte.Fenetre{Active: q.r.PostFormValue("active") != "", Debut: q.r.PostFormValue("debut"),
				Fin: q.r.PostFormValue("fin"), Fuseau: q.r.PostFormValue("fuseau")}
			err = flotte.EcrireFenetre(ctx, c.db, fe, q.qui(), q.t)
		default:
			err = errors.New("action inconnue")
		}
	}
	c.retour(q, "/mises-a-jour", err, ok)
}

// TailleMaxPublication : binaire de l'agent (~8 Mo). Sous 32 Mio, Go garde
// tout le formulaire en mémoire : aucun fichier temporaire (l'image Docker
// n'a pas de /tmp et son système de fichiers est en lecture seule).
const TailleMaxPublication = 30 << 20

// postPublier : manifeste signé (mirador-release) + binaire. Le serveur
// revérifie la signature ; il ne signe jamais.
func (c *Console) postPublier(q *req) {
	err := func() error {
		if err := q.r.ParseMultipartForm(32 << 20); err != nil {
			return fmt.Errorf("envoi illisible (30 Mo au plus) : %w", err)
		}
		defer q.r.MultipartForm.RemoveAll() //nolint:errcheck // fichiers temporaires
		if err := c.verifierCode(q); err != nil {
			return err
		}
		lire := func(champ string, max int64) ([]byte, error) {
			fh, _, err := q.r.FormFile(champ)
			if err != nil {
				return nil, fmt.Errorf("%s manquant", champ)
			}
			defer fh.Close()
			var b bytes.Buffer
			if _, err := io.Copy(&b, io.LimitReader(fh, max+1)); err != nil {
				return nil, err
			}
			if int64(b.Len()) > max {
				return nil, fmt.Errorf("%s trop gros", champ)
			}
			return b.Bytes(), nil
		}
		brutM, err := lire("manifeste", 64<<10)
		if err != nil {
			return err
		}
		bin, err := lire("binaire", TailleMaxPublication)
		if err != nil {
			return err
		}
		var m protocole.Manifeste
		dec := json.NewDecoder(bytes.NewReader(brutM))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&m); err != nil {
			return fmt.Errorf("manifeste illisible : %w", err)
		}
		return flotte.Publier(q.r.Context(), c.db, filepath.Join(c.opt.Donnees, "versions"), &m, bin, q.qui(), q.t)
	}()
	c.retour(q, "/mises-a-jour", err, "version publiée (signature vérifiée)")
}

// --- Tableau des machines de la vue d'ensemble ------------------------------------------

// ligneParc : une machine, son état et ses chiffres du moment.
type ligneParc struct {
	Nom, Site, Env, Groupe string
	Etat, ClasseEtat       string // texte et classe du badge
	Etincelle              string // points de la petite courbe CPU (6 h)
	CPU, Memoire, Disque   string
	DisquePlein            bool
	VuLe                   int64
	rang                   int
	// Barres (0 à 100, -1 = pas de valeur) et leur couleur (ok, attention, erreur).
	PctCPU, PctMemoire, PctDisque int
	ClasseCPU, ClasseMemoire      string
	ClasseDisque                  string
	PleinDans                     string // « plein dans 12 j » : disque le plus pressé (SPEC § 31)
	PleinUrgent                   bool   // moins de 7 jours
	Alertes                       int    // alertes actives hors sourdine
	ClasseAlertes                 string // gravité la plus forte
	Nouveautes                    int    // éléments détectés « à valider »
	Muette                        bool
	// Mises à jour de paquets (apt) : -1 = pas de valeur (pas Debian/Ubuntu,
	// ou réglage « Mises à jour » retiré).
	Paquets, PaquetsSecurite int
	Redemarrage              bool // /run/reboot-required présent
}

// barre : pourcentage entier et couleur d'une barre de la vue d'ensemble
// (vert sous 70 %, orange de 70 à 85 %, rouge au-dessus).
func barre(m map[string]float64, nom string) (int, string) {
	v, ok := m[nom]
	if !ok || math.IsNaN(v) {
		return -1, ""
	}
	p := int(math.Round(math.Max(0, math.Min(100, v))))
	switch {
	case p >= 85:
		return p, "erreur"
	case p >= 70:
		return p, "attention"
	}
	return p, "ok"
}

// Requêtes de la vue d'ensemble (celles du tableau « Serveurs Linux »).
const (
	exprCPU     = `100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))`
	exprCPU6h   = `100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[10m])))`
	exprMemoire = `100 * (1 - max by (instance) (node_memory_MemAvailable_bytes) / max by (instance) (node_memory_MemTotal_bytes))`
	exprDisque  = `max by (instance) (100 * (1 - node_filesystem_avail_bytes{fstype!~"tmpfs|overlay|squashfs|ramfs|nsfs|fuse.lxcfs"} / node_filesystem_size_bytes{fstype!~"tmpfs|overlay|squashfs|ramfs|nsfs|fuse.lxcfs"}))`
	// Mises à jour de paquets : dernière valeur des 3 dernières heures
	// (l'agent les compte toutes les quelques heures seulement).
	exprPaquets         = `max by (instance) (last_over_time(apt_upgrades_pending[3h]))`
	exprPaquetsSecurite = `max by (instance) (last_over_time(apt_upgrades_security_pending[3h]))`
	exprRedemarrage     = `max by (instance) (last_over_time(node_reboot_required[15m]))`
)

// parc : une ligne par machine approuvée, les plus en difficulté d'abord.
func (c *Console) parc(q *req, actives []resumeAlerte) []ligneParc {
	return c.parcAvec(q, actives, true)
}

// parcLeger : état et CPU seulement (registre des éléments, fiches) ; une
// requête de métriques au lieu de neuf (relecture du 21/11 : chaque fiche
// recalculait toute la vue d'ensemble pour trouver un seul élément).
func (c *Console) parcLeger(q *req) []ligneParc {
	actives, _ := separerAttente(c.alertesEnCours(""))
	return c.parcAvec(q, actives, false)
}

func (c *Console) parcAvec(q *req, actives []resumeAlerte, complet bool) []ligneParc {
	ctx := q.r.Context()
	ms, err := flotte.ListerMachines(ctx, c.db)
	if err != nil {
		return nil
	}
	pire := map[string]string{}
	nombre := map[string]int{}
	for _, a := range actives {
		if a.Etat != "active" || a.Sourdine != "" {
			continue
		}
		nombre[a.Machine]++
		if a.Gravite == alertes.Critique || pire[a.Machine] == "" {
			pire[a.Machine] = a.Gravite
		}
	}
	valeurs := func(expr string) map[string]float64 {
		out := map[string]float64{}
		if !complet && expr != exprCPU {
			return out
		}
		if ech, err := c.instant(ctx, expr, q.t); err == nil {
			for _, e := range ech {
				out[e.Etiquettes.Get("instance")] = e.Valeur
			}
		}
		return out
	}
	cpu, mem, disque := valeurs(exprCPU), valeurs(exprMemoire), valeurs(exprDisque)
	paquets, securite, redemarrage := valeurs(exprPaquets), valeurs(exprPaquetsSecurite), valeurs(exprRedemarrage)
	var plein map[string]float64
	var courbes map[string]string
	var nouveautes map[string]int
	if complet {
		plein = pleinDansMachine(c.previsionsDisques(ctx, strings.Replace(filtreDisques, `instance=~"$instance", `, "", 1), q.t))
		courbes = c.etincelles(ctx, q.t)
		// Compte « à valider » gardé 60 s, le même que le menu et la page
		// Machines (relecture du 21/11 : relu à chaque rechargement).
		nouveautes = c.aValider(ctx, q.t)
	}
	pct := func(m map[string]float64, nom string) string {
		if v, ok := m[nom]; ok {
			return formater(v, "percent", &zero)
		}
		return "—"
	}
	var out []ligneParc
	for _, m := range ms {
		if m.Statut != "approuvee" {
			continue
		}
		l := ligneParc{Nom: m.Nom, Site: m.Site, Env: m.Env, Groupe: m.Groupe, VuLe: m.VuLe, Etincelle: courbes[m.Nom],
			CPU: pct(cpu, m.Nom), Memoire: pct(mem, m.Nom), Disque: pct(disque, m.Nom), DisquePlein: disque[m.Nom] >= 90}
		l.PctCPU, l.ClasseCPU = barre(cpu, m.Nom)
		l.PctMemoire, l.ClasseMemoire = barre(mem, m.Nom)
		l.PctDisque, l.ClasseDisque = barre(disque, m.Nom)
		if j, ok := plein[m.Nom]; ok {
			l.PleinDans, l.PleinUrgent = pleinDans(j), j < 7
		}
		l.Alertes, l.Nouveautes = nombre[m.Nom], nouveautes[m.Nom]
		l.Paquets, l.PaquetsSecurite = -1, 0
		if v, ok := paquets[m.Nom]; ok {
			l.Paquets, l.PaquetsSecurite = int(v), int(securite[m.Nom])
		}
		l.Redemarrage = redemarrage[m.Nom] > 0
		switch pire[m.Nom] {
		case alertes.Critique:
			l.ClasseAlertes = "erreur"
		case "":
		default:
			l.ClasseAlertes = "attention"
		}
		switch {
		case q.t.Sub(time.Unix(m.VuLe, 0)) > delaiMuette:
			l.Muette = true
			l.Etat, l.ClasseEtat, l.rang = "Muette", "", 1
		case pire[m.Nom] == alertes.Critique:
			l.Etat, l.ClasseEtat, l.rang = "Critique", "erreur", 0
		case pire[m.Nom] != "":
			l.Etat, l.ClasseEtat, l.rang = "Avertissement", "attention", 2
		default:
			l.Etat, l.ClasseEtat, l.rang = "En ligne", "ok", 3
		}
		out = append(out, l)
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].rang != out[j].rang {
			return out[i].rang < out[j].rang
		}
		return out[i].Nom < out[j].Nom
	})
	return out
}

// Gravite : état pour le tri et le filtre de la vue d'ensemble (3 critique,
// 2 muette, 1 avertissement, 0 en ligne sans alerte).
func (l ligneParc) Gravite() int { return 3 - l.rang }

var zero = 0

// etincelles : petite courbe CPU des 6 dernières heures pour chaque machine
// (une seule requête pour tout le parc, gardée 60 s).
func (c *Console) etincelles(ctx context.Context, t time.Time) map[string]string {
	c.muEtincelles.Lock()
	defer c.muEtincelles.Unlock()
	if t.Before(c.etincellesExpire) {
		return c.etincellesCache
	}
	out := map[string]string{}
	if c.lecteur != nil {
		fin := t.Truncate(10 * time.Minute)
		if courbes, _, err := c.lecteur.Plage(ctx, exprCPU6h, fin.Add(-6*time.Hour), fin, 10*time.Minute); err == nil {
			debut := fin.Add(-6 * time.Hour).UnixMilli()
			for _, cb := range courbes {
				var b strings.Builder
				for i, ts := range cb.T {
					x := float64(ts-debut) / float64(6*time.Hour/time.Millisecond) * 90
					y := 23 - math.Max(0, math.Min(100, cb.V[i]))/100*22
					if i > 0 {
						b.WriteByte(' ')
					}
					fmt.Fprintf(&b, "%.1f,%.1f", x, y)
				}
				out[cb.Etiquettes.Get("instance")] = b.String()
			}
		}
	}
	c.etincellesCache, c.etincellesExpire = out, t.Add(time.Minute)
	return out
}

// --- Recherche rapide (Ctrl+K) ------------------------------------------------------------

// destination : une ligne de la recherche rapide.
type destination struct {
	Titre string `json:"titre"`
	Genre string `json:"genre"` // Page, Machine, Tableau
	Lien  string `json:"lien"`
}

// donneesAller : pages, machines et tableaux où l'on peut aller (lus une
// fois à l'ouverture de la recherche ; le filtrage se fait dans le navigateur).
func (c *Console) donneesAller(q *req) {
	out := []destination{
		{"Vue d'ensemble", "Page", "/"}, {"Machines", "Page", "/machines"}, {"Conteneurs", "Page", "/conteneurs"},
		{"Alertes", "Page", "/alertes"},
		{"Silences et maintenances", "Page", "/sourdine"}, {"Envois", "Page", "/envois"}, {"Logs", "Page", "/logs"},
		{"Tableaux", "Page", "/tableaux"}, {"Règles", "Page", "/regles"}, {"Mon compte", "Page", "/compte"},
		{"Sondes", "Page", "/sondes"}, {"Tâches planifiées", "Page", "/taches"}, {"Applications", "Page", "/applications"}, {"Éléments", "Page", "/elements"},
		{"Incidents", "Page", "/incidents"}, {"Événements", "Page", "/evenements"}, {"Disponibilité", "Page", "/disponibilite"},
		{"Coupures", "Page", "/coupures"},
	}
	ctx := q.r.Context()
	// Sondes, services, conteneurs (SPEC § 58) : leurs fiches.
	if ss, err := flotte.LireSondes(ctx, c.db); err == nil {
		for _, s := range ss {
			out = append(out, destination{s.Nom, "Sonde", "/sondes/" + url.PathEscape(s.Nom)})
		}
	}
	if svcs, err := servicesmetier.Lire(ctx, c.db); err == nil {
		for _, s := range svcs {
			out = append(out, destination{s.Nom, "Application", "/applications/" + url.PathEscape(s.Nom)})
		}
	}
	if c.lecteur != nil {
		if ech, err := c.instant(ctx, `max by (instance, name) (node_docker_container_running)`, q.t); err == nil {
			for _, e := range ech {
				m, n := e.Etiquettes.Get("instance"), e.Etiquettes.Get("name")
				if m != "" && n != "" {
					out = append(out, destination{n + " (" + m + ")", "Conteneur", "/conteneurs/" + url.PathEscape(m) + "/" + url.PathEscape(n)})
				}
			}
		}
	}
	if q.admin() {
		out = append(out, destination{"Mises à jour des agents", "Page", "/mises-a-jour"},
			destination{"Configuration des agents", "Page", "/reglages/agents"},
			destination{"Notifications", "Page", "/reglages/notifications"},
			destination{"Conservation des données", "Page", "/reglages/conservation"},
			destination{"Sauvegardes", "Page", "/reglages/sauvegardes"},
			destination{"Utilisateurs", "Page", "/utilisateurs"}, destination{"Sécurité", "Page", "/securite"})
	}
	if ms, err := flotte.ListerMachines(ctx, c.db); err == nil {
		for _, m := range ms {
			if m.Statut == "approuvee" {
				out = append(out, destination{m.Nom, "Machine", "/machines/" + url.PathEscape(m.Nom)})
				if q.admin() {
					out = append(out, destination{"Mettre " + m.Nom + " en maintenance…", "Action",
						"/sourdine?maintenance=" + url.QueryEscape("machine:"+m.Nom) + "#maintenance"})
				}
			}
		}
	}
	for _, t := range c.listeTableaux() {
		out = append(out, destination{t.Titre, "Tableau", "/tableaux/" + url.PathEscape(t.Nom)})
	}
	q.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	q.w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(q.w).Encode(out)
}
