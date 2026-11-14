// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/modeles"
)

// Modèles de surveillance dans l'interface (SPEC § 24, version simplifiée du
// 14/11) :
//   - page d'une machine : une case par modèle (« ★ détecté », « du groupe »,
//     « ignoré », « auto ») et un seul bouton « Enregistrer » ; les seuils de
//     la machine sur une page à part (« Seuils… ») ;
//   - page « Modèles » : tableau modèles × groupes, application automatique
//     des modèles détectés, détails repliés (règles, services, seuils du
//     groupe).

func (c *Console) routesModeles() {
	c.mux.HandleFunc("GET /modeles", c.lecture(c.pageModeles))
	c.mux.HandleFunc("POST /modeles", c.admin(c.postModeles))
	c.mux.HandleFunc("GET /machines/{nom}/modeles/{modele}", c.lecture(c.pageSeuilsMachine))
	c.mux.HandleFunc("POST /machines/{nom}/modeles/{modele}", c.admin(c.postSeuilsMachine))
}

// vueParam : un paramètre et sa valeur effective.
type vueParam struct {
	modeles.Parametre
	Valeur    float64
	Surcharge bool // réglé pour cette portée (sinon hérité)
}

// vueListe : une liste d'un modèle (une valeur par ligne) et son contenu.
type vueListe struct {
	modeles.ListeParam
	Texte  string
	Nb     int
	Propre bool // réglée pour cette portée (sinon héritée du groupe)
}

// vuesListes : listes d'un modèle avec leurs lignes.
func vuesListes(mod *modeles.Modele, valeurs map[string][]string, propres map[string]bool) []vueListe {
	var out []vueListe
	for _, l := range mod.Listes {
		v := valeurs[l.Nom]
		out = append(out, vueListe{ListeParam: l, Texte: strings.Join(v, "\n"), Nb: len(v), Propre: propres[l.Nom]})
	}
	return out
}

// lireListesModele : listes saisies « l.<nom> » (une valeur par ligne).
// heritees : listes du groupe ; une liste identique n'est pas enregistrée
// (elle reste héritée). nil pour un groupe.
func lireListesModele(mod *modeles.Modele, f func(string) string, heritees map[string][]string) (map[string][]string, error) {
	out := map[string][]string{}
	for i := range mod.Listes {
		l := &mod.Listes[i]
		lignes, err := l.LireLignes(f("l." + l.Nom))
		if err != nil {
			return nil, err
		}
		if heritees != nil && strings.Join(lignes, "\n") == strings.Join(heritees[l.Nom], "\n") {
			continue
		}
		if heritees == nil && len(lignes) == 0 {
			continue
		}
		if lignes == nil {
			lignes = []string{} // liste vidée pour cette machine
		}
		out[l.Nom] = lignes
	}
	return out, nil
}

// listesDuGroupe : listes d'un modèle réglées pour un groupe.
func (c *Console) listesDuGroupe(ctx context.Context, groupe, modele string) map[string][]string {
	if groupe == "" {
		return map[string][]string{}
	}
	as, err := modeles.Lister(ctx, c.db, modeles.PorteeGroupe(groupe))
	if err != nil {
		return map[string][]string{}
	}
	for _, a := range as {
		if a.Modele == modele && a.Etat == modeles.EtatActif && a.Listes != nil {
			return a.Listes
		}
	}
	return map[string][]string{}
}

// vueCase : une ligne du bloc « Modèles » de la page d'une machine.
type vueCase struct {
	M        *modeles.Modele
	Coche    bool
	DuGroupe bool
	Detecte  bool // détecté, ni attribué ni ignoré
	Ignore   bool
	Auto     bool // appliqué automatiquement
	Seuils   bool // modèle appliqué qui a des seuils ou des listes réglables
	AVide    bool // modèle appliqué dont toutes les listes sont vides (rien à tester)
}

// vueModeles : bloc « Modèles » de la page d'une machine.
type vueModeles struct {
	Cases    []vueCase
	Proposes int
	Groupe   string
}

// detectes : modèle → instances où sa détection répond (cache du lecteur).
func (c *Console) detectes(ctx context.Context, t time.Time) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	for _, m := range modeles.Tous() {
		if m.Detection == "" {
			continue
		}
		ech, err := c.instant(ctx, m.Detection, t)
		if err != nil {
			continue
		}
		vus := map[string]bool{}
		for _, e := range ech {
			if i := e.Etiquettes.Get("instance"); i != "" {
				vus[i] = true
			}
		}
		out[m.Nom] = vus
	}
	return out
}

// etatModeles : modèles appliqués, ignorés et proposés d'une machine.
func (c *Console) etatModeles(ctx context.Context, m flotte.InfoMachine, det map[string]map[string]bool) (actifs map[string]modeles.Effectif, ign map[string]bool, proposes []*modeles.Modele, err error) {
	effs, err := modeles.Effectifs(ctx, c.db, m.ID, m.Groupe)
	if err != nil {
		return nil, nil, nil, err
	}
	if ign, err = modeles.Ignores(ctx, c.db, m.ID); err != nil {
		return nil, nil, nil, err
	}
	actifs = map[string]modeles.Effectif{}
	for _, e := range effs {
		actifs[e.Modele.Nom] = e
	}
	for _, mod := range modeles.Tous() {
		if _, ok := actifs[mod.Nom]; !ok && det[mod.Nom][m.Nom] && !ign[mod.Nom] {
			proposes = append(proposes, mod)
		}
	}
	return actifs, ign, proposes, nil
}

// vueModelesMachine : le bloc « Modèles » d'une machine.
func (c *Console) vueModelesMachine(ctx context.Context, m flotte.InfoMachine, t time.Time) vueModeles {
	v := vueModeles{Groupe: m.Groupe}
	actifs, ign, proposes, err := c.etatModeles(ctx, m, c.detectes(ctx, t))
	if err != nil {
		return v
	}
	propose := map[string]bool{}
	for _, p := range proposes {
		propose[p.Nom] = true
	}
	v.Proposes = len(proposes)
	for _, mod := range modeles.Tous() {
		e, coche := actifs[mod.Nom]
		vide := len(mod.Listes) > 0
		for _, l := range e.Listes {
			if len(l) > 0 {
				vide = false
			}
		}
		v.Cases = append(v.Cases, vueCase{M: mod, Coche: coche, DuGroupe: coche && e.DuGroupe, Detecte: propose[mod.Nom],
			Ignore: ign[mod.Nom], Auto: coche && e.Auto, Seuils: coche && (len(mod.Parametres) > 0 || len(mod.Listes) > 0),
			AVide: coche && vide})
	}
	return v
}

// enregistrerModelesMachine : cases cochées du bloc « Modèles ». Coché et
// absent : attribué ; décoché et appliqué : retiré (ignoré pour cette machine
// s'il vient du groupe) ; détecté et laissé décoché : ignoré (plus proposé).
func (c *Console) enregistrerModelesMachine(q *req, m flotte.InfoMachine) (int, error) {
	ctx := q.r.Context()
	if err := q.r.ParseForm(); err != nil {
		return 0, err
	}
	coches := map[string]bool{}
	for _, n := range q.r.PostForm["m"] {
		coches[n] = true
	}
	actifs, ign, proposes, err := c.etatModeles(ctx, m, c.detectes(ctx, q.t))
	if err != nil {
		return 0, err
	}
	propose := map[string]bool{}
	for _, p := range proposes {
		propose[p.Nom] = true
	}
	deGroupe := map[string]bool{}
	if m.Groupe != "" {
		if as, err := modeles.Lister(ctx, c.db, modeles.PorteeGroupe(m.Groupe)); err == nil {
			for _, a := range as {
				deGroupe[a.Modele] = a.Etat == modeles.EtatActif
			}
		}
	}
	portee := modeles.PorteeMachine(m.ID)
	changes := 0
	var errs []error
	for _, mod := range modeles.Tous() {
		_, actif := actifs[mod.Nom]
		etat := "?"
		switch {
		case coches[mod.Nom] && !actif && deGroupe[mod.Nom] && ign[mod.Nom]:
			etat = "" // retour du modèle du groupe : la ligne « ignoré » est effacée
		case coches[mod.Nom] && !actif:
			etat = modeles.EtatActif
		case !coches[mod.Nom] && actif && deGroupe[mod.Nom]:
			etat = modeles.EtatIgnore // retiré pour cette machine seulement
		case !coches[mod.Nom] && actif:
			etat = "" // ligne de la machine effacée
		case !coches[mod.Nom] && propose[mod.Nom]:
			etat = modeles.EtatIgnore // vu et laissé de côté : plus proposé
		}
		if etat == "?" {
			continue
		}
		if err := modeles.Definir(ctx, c.db, portee, mod.Nom, etat, nil, nil, q.qui(), q.t); err != nil {
			errs = append(errs, err)
			continue
		}
		changes++
	}
	c.oublierAValider()
	return changes, errors.Join(errs...)
}

// --- Seuils d'un modèle pour une machine ---------------------------------------------

type donneesSeuils struct {
	Machine  string
	M        *modeles.Modele
	Params   []vueParam
	Listes   []vueListe
	Services []string
	DuGroupe bool
	Groupe   string
	// Checks : checks demandés à l'agent par tous ses modèles, ceux laissés
	// de côté (au-delà de 50) et agent trop ancien pour les faire.
	NbChecks, Ecartes int
	SansChecks        bool
}

// seuilsMachine : machine et modèle de l'adresse ; le modèle doit être
// appliqué à la machine.
func (c *Console) seuilsMachine(q *req) (flotte.InfoMachine, *modeles.Modele, modeles.Effectif, bool) {
	m, ok := c.machine(q)
	mod := modeles.Trouver(q.r.PathValue("modele"))
	if !ok || mod == nil {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Introuvable", "machines", nil)
		return m, nil, modeles.Effectif{}, false
	}
	if effs, err := modeles.Effectifs(q.r.Context(), c.db, m.ID, m.Groupe); err == nil {
		for _, e := range effs {
			if e.Modele.Nom == mod.Nom {
				return m, mod, e, true
			}
		}
	}
	c.retour(q, "/machines/"+url.PathEscape(m.Nom)+"#modeles", errors.New("ce modèle n'est pas appliqué à cette machine"), "")
	return m, nil, modeles.Effectif{}, false
}

func (c *Console) pageSeuilsMachine(q *req) {
	m, mod, e, ok := c.seuilsMachine(q)
	if !ok {
		return
	}
	d := donneesSeuils{Machine: m.Nom, M: mod, Services: e.Services, DuGroupe: e.DuGroupe, Groupe: m.Groupe,
		Listes: vuesListes(mod, e.Listes, e.ListesMach)}
	for _, p := range mod.Parametres {
		d.Params = append(d.Params, vueParam{Parametre: p, Valeur: e.Parametres[p.Nom], Surcharge: e.Surcharges[p.Nom]})
	}
	if len(mod.Listes) > 0 {
		if effs, err := modeles.Effectifs(q.r.Context(), c.db, m.ID, m.Groupe); err == nil {
			d.NbChecks, d.Ecartes = modeles.NombreChecks(effs)
		}
		d.SansChecks = m.VuLe > 0 && !m.Checks() // agent déjà vu, trop ancien
	}
	c.rendre(q, "modele-machine", mod.Titre+" — "+m.Nom, "machines", d)
}

func (c *Console) postSeuilsMachine(q *req) {
	m, mod, _, ok := c.seuilsMachine(q)
	if !ok {
		return
	}
	vers := "/machines/" + url.PathEscape(m.Nom) + "/modeles/" + url.PathEscape(mod.Nom)
	err := c.verifierCode(q)
	var params map[string]float64
	var listes map[string][]string
	if err == nil {
		params, err = lireParametres(mod, q.r.PostFormValue)
	}
	if err == nil {
		listes, err = lireListesModele(mod, q.r.PostFormValue, c.listesDuGroupe(q.r.Context(), m.Groupe, mod.Nom))
	}
	if err == nil {
		// Modèle venu du groupe : la ligne « actif » de la machine ne sert
		// qu'à ses seuils et listes (même effet que l'héritage).
		err = modeles.Definir(q.r.Context(), c.db, modeles.PorteeMachine(m.ID), mod.Nom, modeles.EtatActif, params, listes, q.qui(), q.t)
	}
	c.retour(q, vers, err, "réglages de « "+mod.Titre+" » enregistrés pour "+m.Nom+" (pris en compte sous une minute)")
}

// lireParametres : valeurs saisies « p.<nom> » ; vide = hérité (pas de surcharge).
func lireParametres(mod *modeles.Modele, f func(string) string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, p := range mod.Parametres {
		texte := strings.TrimSpace(f("p." + p.Nom))
		if texte == "" {
			continue
		}
		v, err := mod.LireValeur(p.Nom, texte)
		if err != nil {
			return nil, err
		}
		out[p.Nom] = v
	}
	return out, nil
}

// --- Page « Modèles » ------------------------------------------------------------

// vueCatalogue : un modèle du catalogue.
type vueCatalogue struct {
	M        *modeles.Modele
	Services []string
	Suggeres []string          // services vus sur toutes ses machines (et pas partout)
	Groupes  []vueGroupeModele // groupes qui le portent (seuils du groupe)
	Cases    []vueCaseGroupe   // une case par groupe (tableau)
	NbMach   int
	NbDetec  int
}

type vueGroupeModele struct {
	Groupe string
	Params []vueParam
	Listes []vueListe
}

type vueCaseGroupe struct {
	Groupe string
	Coche  bool
}

type donneesModeles struct {
	Groupes   []string
	Catalogue []vueCatalogue
	Auto      bool
	Detectes  []vueDetectes // machines avec des modèles détectés pas encore cochés
}

// vueDetectes : modèles détectés et pas encore appliqués sur une machine.
type vueDetectes struct {
	Machine string
	Modeles []string // titres
}

func (c *Console) pageModeles(q *req) {
	ctx := q.r.Context()
	l := c.lireListes(ctx)
	ms, _ := flotte.ListerMachines(ctx, c.db)
	det := c.detectes(ctx, q.t)
	d := donneesModeles{Groupes: l.Groupes, Auto: modeles.LireAuto(ctx, c.db)}
	parGroupe := map[string]map[string]modeles.Attribution{}
	for _, g := range l.Groupes {
		parGroupe[g] = map[string]modeles.Attribution{}
		if as, err := modeles.Lister(ctx, c.db, modeles.PorteeGroupe(g)); err == nil {
			for _, a := range as {
				if a.Etat == modeles.EtatActif {
					parGroupe[g][a.Modele] = a
				}
			}
		}
	}
	effsParMachine := map[int64]map[string]bool{}
	for _, m := range ms {
		if m.Statut != "approuvee" {
			continue
		}
		effs, err := modeles.Effectifs(ctx, c.db, m.ID, m.Groupe)
		if err != nil {
			continue
		}
		effsParMachine[m.ID] = map[string]bool{}
		for _, e := range effs {
			effsParMachine[m.ID][e.Modele.Nom] = true
		}
	}
	servicesVus := c.servicesParMachine(ctx, q.t)
	for _, mod := range modeles.Tous() {
		vc := vueCatalogue{M: mod}
		vc.Services, _ = modeles.Services(ctx, c.db, mod)
		vc.Suggeres = suggererServices(mod.Nom, vc.Services, ms, effsParMachine, servicesVus)
		for _, g := range l.Groupes {
			a, porte := parGroupe[g][mod.Nom]
			vc.Cases = append(vc.Cases, vueCaseGroupe{Groupe: g, Coche: porte})
			if porte {
				vg := vueGroupeModele{Groupe: g, Listes: vuesListes(mod, a.Listes, nil)}
				for _, p := range mod.Parametres {
					v, sur := p.Defaut, false
					if x, ok := a.Parametres[p.Nom]; ok {
						v, sur = x, true
					}
					vg.Params = append(vg.Params, vueParam{Parametre: p, Valeur: v, Surcharge: sur})
				}
				vc.Groupes = append(vc.Groupes, vg)
			}
		}
		for _, m := range ms {
			if eff, ok := effsParMachine[m.ID]; ok {
				if eff[mod.Nom] {
					vc.NbMach++
				} else if det[mod.Nom][m.Nom] {
					vc.NbDetec++
				}
			}
		}
		d.Catalogue = append(d.Catalogue, vc)
	}
	for _, m := range ms {
		if m.Statut != "approuvee" || m.Type != "agent" {
			continue
		}
		if _, _, proposes, err := c.etatModeles(ctx, m, det); err == nil && len(proposes) > 0 {
			v := vueDetectes{Machine: m.Nom}
			for _, p := range proposes {
				v.Modeles = append(v.Modeles, p.Titre)
			}
			d.Detectes = append(d.Detectes, v)
		}
	}
	c.rendre(q, "modeles", "Modèles de surveillance", "machines", d)
}

// postModeles : tableau modèles × groupes, application automatique,
// services et seuils d'un groupe (code 2FA ou mode administration).
func (c *Console) postModeles(q *req) {
	ctx := q.r.Context()
	f := q.r.PostFormValue
	err := c.verifierCode(q)
	ok := "fait"
	vers := "/modeles"
	if err == nil {
		switch f("action") {
		case "groupes":
			var n int
			n, err = c.enregistrerGroupes(q)
			ok = strconv.Itoa(n) + " changement(s) enregistré(s) : configuration envoyée aux agents, règles actives sous une minute"
		case "detectes":
			var n int
			n, err = c.appliquerDetectes(ctx, q.qui(), q.t)
			ok = strconv.Itoa(n) + " modèle(s) détecté(s) appliqué(s) : retirables sur la page de chaque machine"
		case "auto":
			actif := f("auto") == "oui"
			err = modeles.DefinirAuto(ctx, c.db, actif, q.qui(), q.t)
			ok = "application automatique des modèles détectés coupée"
			if actif {
				ok = "application automatique activée : les modèles détectés seront appliqués et signalés"
			}
		case "services_ajouter":
			mod := modeles.Trouver(f("modele"))
			if mod == nil {
				err = errors.New("modèle inconnu")
				break
			}
			vers += "#" + url.PathEscape(mod.Nom)
			var l []string
			if l, err = modeles.Services(ctx, c.db, mod); err == nil {
				vu := map[string]bool{}
				for _, x := range l {
					vu[x] = true
				}
				_ = q.r.ParseForm()
				for _, x := range q.r.PostForm["s"] {
					if !vu[x] {
						l, vu[x] = append(l, x), true
					}
				}
				err = modeles.DefinirServices(ctx, c.db, mod.Nom, l, q.qui(), q.t)
			}
			ok = "services ajoutés à « " + mod.Titre + " »"
		case "services":
			mod := modeles.Trouver(f("modele"))
			if mod == nil {
				err = errors.New("modèle inconnu")
				break
			}
			vers += "#" + url.PathEscape(mod.Nom)
			err = modeles.DefinirServices(ctx, c.db, mod.Nom, strings.FieldsFunc(f("services"), func(r rune) bool {
				return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
			}), q.qui(), q.t)
			ok = "services attendus de « " + mod.Titre + " » enregistrés (surveillés sur ses machines sous 5 min)"
		case "parametres":
			mod := modeles.Trouver(f("modele"))
			groupe := strings.TrimSpace(f("groupe"))
			if mod == nil || !c.groupeConnu(ctx, groupe) {
				err = errors.New("modèle ou groupe inconnu")
				break
			}
			vers += "#" + url.PathEscape(mod.Nom)
			var params map[string]float64
			var listes map[string][]string
			if params, err = lireParametres(mod, f); err == nil {
				listes, err = lireListesModele(mod, f, nil)
			}
			if err == nil {
				err = modeles.Definir(ctx, c.db, modeles.PorteeGroupe(groupe), mod.Nom, modeles.EtatActif, params, listes, q.qui(), q.t)
			}
			ok = "réglages de « " + mod.Titre + " » enregistrés pour le groupe " + groupe
		default:
			err = errors.New("action inconnue")
		}
	}
	c.oublierAValider()
	c.retour(q, vers, err, ok)
}

func (c *Console) groupeConnu(ctx context.Context, g string) bool {
	for _, x := range c.lireListes(ctx).Groupes {
		if x == g {
			return true
		}
	}
	return false
}

// enregistrerGroupes : cases « a » = « groupe|modèle » cochées.
func (c *Console) enregistrerGroupes(q *req) (int, error) {
	ctx := q.r.Context()
	if err := q.r.ParseForm(); err != nil {
		return 0, err
	}
	coches := map[string]bool{}
	for _, v := range q.r.PostForm["a"] {
		coches[v] = true
	}
	n := 0
	var errs []error
	for _, g := range c.lireListes(ctx).Groupes {
		as, err := modeles.Lister(ctx, c.db, modeles.PorteeGroupe(g))
		if err != nil {
			return n, err
		}
		actifs := map[string]bool{}
		for _, a := range as {
			actifs[a.Modele] = a.Etat == modeles.EtatActif
		}
		for _, mod := range modeles.Tous() {
			veut := coches[g+"|"+mod.Nom]
			if veut == actifs[mod.Nom] {
				continue
			}
			etat := ""
			if veut {
				etat = modeles.EtatActif
			}
			if err := modeles.Definir(ctx, c.db, modeles.PorteeGroupe(g), mod.Nom, etat, nil, nil, q.qui(), q.t); err != nil {
				errs = append(errs, err)
				continue
			}
			n++
		}
	}
	return n, errors.Join(errs...)
}

// nbModelesProposes : propositions par machine (liste des machines).
func (c *Console) nbModelesProposes(ctx context.Context, ms []flotte.InfoMachine, t time.Time) map[string]int {
	out := map[string]int{}
	det := c.detectes(ctx, t)
	for _, m := range ms {
		if m.Statut != "approuvee" {
			continue
		}
		if _, _, p, err := c.etatModeles(ctx, m, det); err == nil && len(p) > 0 {
			out[m.Nom] = len(p)
		}
	}
	return out
}

// appliquerDetectes : applique en une fois les modèles détectés et pas
// encore cochés sur toutes les machines (machines déjà en place avant les
// modèles). À la demande de l'administrateur (code 2FA) ; les modèles
// ignorés restent de côté.
func (c *Console) appliquerDetectes(ctx context.Context, qui string, t time.Time) (int, error) {
	ms, err := flotte.ListerMachines(ctx, c.db)
	if err != nil {
		return 0, err
	}
	det := c.detectes(ctx, t)
	n := 0
	var errs []error
	for _, m := range ms {
		if m.Statut != "approuvee" || m.Type != "agent" {
			continue
		}
		_, _, proposes, err := c.etatModeles(ctx, m, det)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		for _, mod := range proposes {
			if err := modeles.Definir(ctx, c.db, modeles.PorteeMachine(m.ID), mod.Nom, modeles.EtatActif, nil, nil, qui, t); err != nil {
				errs = append(errs, fmt.Errorf("%s / %s : %w", m.Nom, mod.Nom, err))
				continue
			}
			n++
		}
	}
	if n > 0 {
		c.oublierAValider()
	}
	return n, errors.Join(errs...)
}

// appliquerAuto : application automatique des modèles détectés (réglage,
// non par défaut). Chaque modèle appliqué est signalé par les canaux de
// notification (un message par machine et par modèle) et reste retirable.
func (c *Console) appliquerAuto(ctx context.Context, t time.Time) {
	if c.lecteur == nil || !modeles.LireAuto(ctx, c.db) {
		return
	}
	ms, err := flotte.ListerMachines(ctx, c.db)
	if err != nil {
		return
	}
	det := c.detectes(ctx, t)
	for _, m := range ms {
		if m.Statut != "approuvee" || m.Type != "agent" {
			continue
		}
		_, _, proposes, err := c.etatModeles(ctx, m, det)
		if err != nil {
			continue
		}
		for _, mod := range proposes {
			if err := modeles.Definir(ctx, c.db, modeles.PorteeMachine(m.ID), mod.Nom, modeles.EtatActif, nil, nil, modeles.QuiAuto, t); err != nil {
				c.journal.Warn("modèle non appliqué automatiquement", "machine", m.Nom, "modele", mod.Nom, "erreur", err)
				continue
			}
			c.journal.Info("modèle appliqué automatiquement", "machine", m.Nom, "modele", mod.Nom)
			c.prevenirSecurite("modele-auto|"+m.Nom+"|"+mod.Nom, "🧩 mirador : modèle « "+mod.Titre+" » appliqué automatiquement à "+m.Nom+
				" (détecté sur la machine).\nPour le retirer : page de la machine, bloc « Modèles ».", t)
			c.oublierAValider()
		}
	}
}

// formatSeuil : valeur d'un paramètre pour un champ de formulaire.
func formatSeuil(v float64) string { return strconv.FormatFloat(v, 'f', -1, 64) }

// servicesParMachine : services vus par l'agent sur chaque machine (une
// série node_systemd_unit_state par service chargé et suivi).
func (c *Console) servicesParMachine(ctx context.Context, t time.Time) map[string]map[string]bool {
	out := map[string]map[string]bool{}
	ech, err := c.instant(ctx, `max by (instance, name) (node_systemd_unit_state{state="failed", name=~".+\\.service"})`, t)
	if err != nil {
		return out
	}
	for _, e := range ech {
		i, n := e.Etiquettes.Get("instance"), e.Etiquettes.Get("name")
		if out[i] == nil {
			out[i] = map[string]bool{}
		}
		out[i][n] = true
	}
	return out
}

// servicesSysteme : services de base, jamais suggérés.
var servicesSysteme = []string{"systemd-", "user@", "getty@", "serial-getty@", "dbus", "polkit", "rsyslog", "cron", "ssh",
	"networking", "ifup@", "apparmor", "e2scrub", "fstrim", "logrotate", "man-db", "apt-daily", "unattended-upgrades", "qemu-guest-agent"}

// maxSuggestions : services suggérés par modèle.
const maxSuggestions = 20

// suggererServices : services vus sur TOUTES les machines du modèle et pas
// sur toutes les autres machines (sinon ce n'est pas propre au modèle), hors
// services de base et déjà listés.
func suggererServices(modele string, deja []string, ms []flotte.InfoMachine, effs map[int64]map[string]bool, vus map[string]map[string]bool) []string {
	var avec, sans []string
	for _, m := range ms {
		e, ok := effs[m.ID]
		if !ok || vus[m.Nom] == nil {
			continue // machine sans données systemd : ne compte pas
		}
		if e[modele] {
			avec = append(avec, m.Nom)
		} else {
			sans = append(sans, m.Nom)
		}
	}
	if len(avec) == 0 || len(sans) == 0 {
		return nil
	}
	listes := map[string]bool{}
	for _, d := range deja {
		listes[d] = true
	}
	var out []string
	for n := range vus[avec[0]] {
		if listes[n] || !modeles.ReService.MatchString(n) {
			continue
		}
		base := false
		for _, p := range servicesSysteme {
			if strings.HasPrefix(n, p) {
				base = true
				break
			}
		}
		if base {
			continue
		}
		partout := true
		for _, m := range avec[1:] {
			partout = partout && vus[m][n]
		}
		ailleurs := true
		for _, m := range sans {
			ailleurs = ailleurs && vus[m][n]
		}
		if partout && !ailleurs {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if len(out) > maxSuggestions {
		out = out[:maxSuggestions]
	}
	return out
}
