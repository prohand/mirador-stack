// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bytes"
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
	"strings"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/sauvegarde"
)

// routesPages : pages de supervision et de réglage.
func (c *Console) routesPages() {
	m := c.mux
	m.HandleFunc("GET /fragments/accueil", c.lecture(c.fragmentAccueil))
	m.HandleFunc("GET /machines", c.lecture(c.pageMachines))
	m.HandleFunc("POST /machines/jeton", c.admin(c.postJeton))
	m.HandleFunc("GET /machines/{nom}", c.lecture(c.pageMachine))
	m.HandleFunc("POST /machines/{nom}", c.admin(c.postMachine))
	m.HandleFunc("GET /mises-a-jour", c.lecture(c.pageMisesAJour))
	m.HandleFunc("POST /mises-a-jour", c.admin(c.postMisesAJour))
	m.HandleFunc("POST /mises-a-jour/publier", c.admin(c.postPublier))
	m.HandleFunc("GET /tableaux", c.lecture(c.pageTableaux))
	m.HandleFunc("GET /tableaux/{nom}", c.lecture(c.pageTableau))
	m.HandleFunc("GET /donnees/courbe", c.lecture(c.donneesCourbe))
	m.HandleFunc("GET /donnees/volume-logs", c.lecture(c.donneesVolumeLogs))
	c.routesAlertes()
	c.routesLogs()
	c.routesReglages()
}

// --- Vue d'ensemble --------------------------------------------------------------

type resumeAlerte struct {
	ID                                    int64
	Regle, Gravite, Machine, Resume, Etat string
	Depuis                                int64
	Acquittee                             bool
	Sourdine                              string
}

type donneesAccueil struct {
	LienLinux                    bool // tableau « Serveurs Linux » installé
	Machines, Muettes, EnAttente int
	Critiques, Avertissements    int
	Infos, Actives               int // Actives : alertes actives hors sourdine
	Alertes                      []resumeAlerte
	Canaux                       []notif.EtatCanal
	Mode                         string
	DerniereSauvegarde           sauvegarde.Execution
	SauvegardeOK                 bool
	Metriques, Logs              int64
	Parc                         []ligneParc
	Principal, Secours           notif.EtatCanal
	Plafond                      int64 // plafond total de stockage (réglage « Conservation »)
	PctStockage                  int
	Depuis                       int64
}

func (c *Console) pageAccueil(q *req) {
	c.rendre(q, "accueil", "Vue d'ensemble", "accueil", c.donneesAccueil(q))
}

// fragmentAccueil : même contenu, sans le cadre (rechargé toutes les 60 s).
func (c *Console) fragmentAccueil(q *req) {
	c.pages.executer(q.w, http.StatusOK, "accueil-fragment", Page{CSRF: q.s.CSRF, Admin: q.admin(), D: c.donneesAccueil(q)})
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
	d.Alertes = c.alertesEnCours("")
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
		d.Plafond = cons.MaxBrut + cons.MaxAgrege + cons.MaxLogs
		if d.Plafond > 0 {
			d.PctStockage = min(100, int(100*(d.Metriques+d.Logs)/d.Plafond))
		}
	}
	d.Actives = d.Critiques + d.Avertissements + d.Infos
	d.Parc = c.parc(q, d.Alertes)
	return d
}

// alertesEnCours : alertes en attente ou actives (machine = filtre).
func (c *Console) alertesEnCours(machine string) []resumeAlerte {
	if c.opt.Alertes == nil {
		return nil
	}
	var out []resumeAlerte
	for _, a := range c.opt.Alertes.EnCours() {
		if machine != "" && a.Machine() != machine {
			continue
		}
		r := resumeAlerte{ID: a.ID, Regle: a.Regle, Gravite: a.Gravite, Machine: a.Machine(), Resume: a.Resume,
			Etat: a.Etat, Depuis: a.Debut.UnixMilli(), Acquittee: !a.AcquitteeLe.IsZero()}
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
	Muette bool
}

type donneesMachines struct {
	EnAttente, Approuvees, Autres []vueMachine
	Ingestion                     string         // adresse d'ingestion (Réglages › Interface), vide = à renseigner
	Jeton                         string         // jeton tout juste créé (affiché une seule fois)
	OptionEnvoi                   string         // « --sans-logs », « --sans-metriques » ou vide
	Listes                        ListesMachines // sites, types et groupes proposés (Réglages)
}

func (c *Console) pageMachines(q *req) {
	c.rendre(q, "machines", "Machines", "machines", c.donneesMachines(q))
}

func (c *Console) donneesMachines(q *req) donneesMachines {
	ms, err := flotte.ListerMachines(q.r.Context(), c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	var d donneesMachines
	for _, m := range ms {
		v := vueMachine{InfoMachine: m, Muette: q.t.Sub(time.Unix(m.VuLe, 0)) > delaiMuette}
		switch m.Statut {
		case "en_attente":
			d.EnAttente = append(d.EnAttente, v)
		case "approuvee":
			d.Approuvees = append(d.Approuvees, v)
		default:
			d.Autres = append(d.Autres, v)
		}
	}
	d.Ingestion = lireInterface(q.r.Context(), c.db).Ingestion
	d.Listes = c.lireListes(q.r.Context())
	return d
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
					flotte.Etiquettes{Site: f("site"), Env: f("env"), Groupe: f("groupe"), Nom: strings.ToLower(f("nom"))}, q.qui(), q.t)
			}
		}
	}
	if err != nil {
		c.retour(q, "/machines", err, "")
		return
	}
	// Page affichée directement (pas de redirection) : le jeton n'est
	// jamais gardé en session ni en base en clair. Recharger la page ne
	// recrée rien (le code 2FA ne sert qu'une fois).
	d := c.donneesMachines(q)
	d.Jeton = j
	// Ce que la machine enverra (modifiable ensuite : Configuration des agents).
	switch q.r.PostFormValue("envoi") {
	case "metriques":
		d.OptionEnvoi = "--sans-logs"
	case "logs":
		d.OptionEnvoi = "--sans-metriques"
	}
	c.flash(q, "jeton d'enrôlement créé : à utiliser une seule fois, il n'est affiché qu'ici", "")
	c.rendre(q, "machines", "Machines", "machines", d)
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
	Rangees     []vueRangee
	Periode     string
	Periodes    []string
	Versions    []string
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
		Alertes: c.alertesEnCours(m.Nom), Periodes: periodesProposees}
	d.NomsAnciens, _ = flotte.AnciensNoms(ctx, c.db, m.ID)
	if t, _ := c.tableauOuCatalogue("serveurs-linux"); t != nil && m.Type == "agent" {
		v := url.Values{"instance": {m.Nom}, "periode": {q.r.URL.Query().Get("periode")}}
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
	sensible := map[string]bool{"token": true, "revoquer": true, "supprimer": true, "refuser": true, "ips": true, "cible": true}
	if sensible[action] {
		if err = c.verifierCode(q); err != nil {
			c.retour(q, vers, err, "")
			return
		}
	}
	switch action {
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
}

type cleVue struct{ Valeur, Empreinte string }

func (c *Console) pageMisesAJour(q *req) {
	ctx := q.r.Context()
	var d donneesMaj
	var e1, e2, e3, e4 error
	d.Versions, e1 = flotte.ListerVersions(ctx, c.db)
	d.Cible, e2 = flotte.CibleGlobale(ctx, c.db)
	d.Fenetre, e3 = flotte.LireFenetre(ctx, c.db)
	d.Machines, e4 = flotte.ListerMachines(ctx, c.db)
	cles, e5 := flotte.LireClesPubliques(ctx, c.db)
	for _, k := range cles {
		d.Cles = append(d.Cles, cleVue{k, majagent.EmpreinteCle(k)})
	}
	if err := errors.Join(e1, e2, e3, e4, e5); err != nil {
		c.flash(q, "", err.Error())
	}
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
}

// Requêtes de la vue d'ensemble (celles du tableau « Serveurs Linux »).
const (
	exprCPU     = `100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[5m])))`
	exprCPU6h   = `100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[10m])))`
	exprMemoire = `100 * (1 - max by (instance) (node_memory_MemAvailable_bytes) / max by (instance) (node_memory_MemTotal_bytes))`
	exprDisque  = `max by (instance) (100 * (1 - node_filesystem_avail_bytes{fstype!~"tmpfs|overlay|squashfs|ramfs|nsfs|fuse.lxcfs"} / node_filesystem_size_bytes{fstype!~"tmpfs|overlay|squashfs|ramfs|nsfs|fuse.lxcfs"}))`
)

// parc : une ligne par machine approuvée, les plus en difficulté d'abord.
func (c *Console) parc(q *req, actives []resumeAlerte) []ligneParc {
	ctx := q.r.Context()
	ms, err := flotte.ListerMachines(ctx, c.db)
	if err != nil {
		return nil
	}
	pire := map[string]string{}
	for _, a := range actives {
		if a.Etat != "active" || a.Sourdine != "" {
			continue
		}
		if a.Gravite == alertes.Critique || pire[a.Machine] == "" {
			pire[a.Machine] = a.Gravite
		}
	}
	valeurs := func(expr string) map[string]float64 {
		out := map[string]float64{}
		if ech, err := c.instant(ctx, expr, q.t); err == nil {
			for _, e := range ech {
				out[e.Etiquettes.Get("instance")] = e.Valeur
			}
		}
		return out
	}
	cpu, mem, disque := valeurs(exprCPU), valeurs(exprMemoire), valeurs(exprDisque)
	courbes := c.etincelles(ctx, q.t)
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
		switch {
		case q.t.Sub(time.Unix(m.VuLe, 0)) > delaiMuette:
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
