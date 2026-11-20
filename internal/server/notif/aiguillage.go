// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"database/sql"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/alertes"
)

// Aiguillage (SPEC.md § 17.6) : reçoit les débuts et fins d'alertes du
// moteur, les range par groupe (alertname + instance par défaut), applique la
// sourdine, attend un peu pour regrouper ce qui arrive ensemble, envoie, puis
// rappelle tant qu'une alerte n'est pas acquittée. Même logique
// qu'Alertmanager (group_wait, group_interval, repeat_interval).

// Envoyeur : envoie un message (canaux et bascule : partie 3 ; mode ombre :
// noté seulement). Doit noter lui-même l'envoi dans l'historique.
type Envoyeur interface {
	Envoyer(ctx context.Context, m *Message, r *Reglages)
}

// Options de l'aiguillage.
type Options struct {
	DB         *sql.DB
	EnCours    func() []alertes.Alerte // alertes en cours du moteur
	Envoyeur   Envoyeur                // canaux (mode actif)
	Journal    *slog.Logger
	Maintenant func() time.Time
	// Logs : recherche des extraits joints aux messages (nil : aucun).
	Logs ChercheurLogs
	// Changement : dernier changement de la machine dans l'heure qui
	// précède le début de l'alerte (SPEC § 47), texte court ; nil : aucun.
	Changement func(ctx context.Context, machine string, avant time.Time) (quand time.Time, texte string, ok bool)
}

type suivi struct {
	a        alertes.Alerte
	notifiee bool // son début a été envoyé
	resolue  bool
}

type groupe struct {
	cle          string
	cree         time.Time
	dernierEnvoi time.Time
	alertes      map[uint64]*suivi
}

// Aiguilleur : état de l'aiguillage.
type Aiguilleur struct {
	opt Options

	mu       sync.Mutex
	groupes  map[string]*groupe
	reglages Reglages
	sourdine *Sourdine
	acquis   map[int64]bool
	// aRappeler : alertes dont la prise en charge vient d'expirer (rappel
	// au prochain tour, sans attendre l'intervalle des rappels).
	aRappeler map[int64]bool
	// escaladees : alertes déjà envoyées au canal de secours (une fois).
	escaladees map[int64]bool
	gens       map[string]int64
	chargeLe   time.Time
	enSourd    map[uint64]string // dernière raison de sourdine par alerte (affichage)
	// deplacees : alertes passées dans le groupe de leur site (site
	// injoignable) → clé de ce groupe, pour y ranger aussi leur fin.
	deplacees map[uint64]string
	sitesBas  map[string]SiteInjoignable // sites injoignables au dernier tour
}

// Nouveau crée l'aiguillage. Les alertes déjà actives (redémarrage) sont
// considérées comme déjà envoyées : pas de doublon, rappels repris.
func Nouveau(ctx context.Context, opt Options) (*Aiguilleur, error) {
	if opt.Journal == nil {
		opt.Journal = slog.New(slog.DiscardHandler)
	}
	if opt.Maintenant == nil {
		opt.Maintenant = time.Now
	}
	g := &Aiguilleur{opt: opt, groupes: map[string]*groupe{}, gens: map[string]int64{}, enSourd: map[uint64]string{},
		deplacees: map[uint64]string{}}
	if err := g.recharger(ctx, true); err != nil {
		return nil, err
	}
	t := opt.Maintenant()
	for _, a := range opt.EnCours() {
		if a.Etat != alertes.Active {
			continue
		}
		gr := g.groupeDe(a, t)
		gr.alertes[a.Etiquettes.Hash()] = &suivi{a: a, notifiee: true}
		gr.dernierEnvoi = t
	}
	return g, nil
}

// Recevoir : appelé par le moteur après chaque évaluation. Les alertes sont
// rangées tout de suite dans leur groupe (l'attente de regroupement part de
// leur arrivée) ; l'envoi se fait au tour suivant. Rapide, ne bloque pas.
func (g *Aiguilleur) Recevoir(evs []alertes.Evenement) {
	t := g.opt.Maintenant()
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, e := range evs {
		h := e.Alerte.Etiquettes.Hash()
		gr := g.groupes[g.deplacees[h]] // alerte rangée dans le groupe de son site
		if gr == nil {
			gr = g.groupeDe(e.Alerte, t)
		}
		s := gr.alertes[h]
		switch {
		case e.Type == "debut":
			if s == nil || s.resolue { // nouvelle (ou revenue avant l'envoi de sa fin)
				gr.alertes[h] = &suivi{a: e.Alerte, notifiee: s != nil && s.notifiee}
			}
		case s != nil:
			s.a, s.resolue = e.Alerte, true
		}
	}
}

// Boucle : un tour toutes les 5 s jusqu'à l'arrêt.
func (g *Aiguilleur) Boucle(ctx context.Context) {
	t := time.NewTicker(5 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			g.Tour(ctx)
		}
	}
}

// recharger relit réglages, sourdine et acquittements si leur compteur a
// changé (ou chaque minute : groupes des machines, silences qui commencent).
func (g *Aiguilleur) recharger(ctx context.Context, force bool) error {
	t := g.opt.Maintenant()
	change := force || t.Sub(g.chargeLe) >= time.Minute
	gens := map[string]int64{}
	for _, n := range []string{"notif", "sourdine", "acquittements"} {
		v, err := alertes.LireGeneration(ctx, g.opt.DB, n)
		if err != nil {
			return err
		}
		gens[n] = v
		change = change || v != g.gens[n]
	}
	if !change {
		return nil
	}
	r, err := LireReglages(ctx, g.opt.DB)
	if err != nil {
		return err
	}
	s, err := Charger(ctx, g.opt.DB, r.ReglesPanne, t)
	if err != nil {
		return err
	}
	if ids, err := expirerAcquittements(ctx, g.opt.DB, t); err != nil {
		return err
	} else if len(ids) > 0 {
		g.mu.Lock()
		if g.aRappeler == nil {
			g.aRappeler = map[int64]bool{}
		}
		for _, id := range ids {
			g.aRappeler[id] = true
		}
		g.mu.Unlock()
	}
	ac, err := acquittees(ctx, g.opt.DB)
	if err != nil {
		return err
	}
	g.mu.Lock()
	g.reglages, g.sourdine, g.acquis, g.gens, g.chargeLe = r, s, ac, gens, t
	g.mu.Unlock()
	return nil
}

// cleGroupe : valeurs des étiquettes de regroupement.
func cleGroupe(a *alertes.Alerte, par []string) string {
	var b strings.Builder
	for i, n := range par {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(n + "=" + a.Etiquettes.Get(n))
	}
	return b.String()
}

// groupeDe (sous verrou) : groupe de l'alerte, créé si besoin.
func (g *Aiguilleur) groupeDe(a alertes.Alerte, t time.Time) *groupe {
	cle := cleGroupe(&a, g.reglages.GrouperPar)
	gr := g.groupes[cle]
	if gr == nil {
		gr = &groupe{cle: cle, cree: t, alertes: map[uint64]*suivi{}}
		g.groupes[cle] = gr
	}
	return gr
}

// Tour : traite les événements reçus, décide des envois.
func (g *Aiguilleur) Tour(ctx context.Context) {
	if err := g.recharger(ctx, false); err != nil {
		g.opt.Journal.Error("notifications : relecture des réglages", "erreur", err)
	}
	t := g.opt.Maintenant()
	var actives []alertes.Alerte
	for _, a := range g.opt.EnCours() {
		if a.Etat == alertes.Active {
			actives = append(actives, a)
		}
	}

	g.mu.Lock()
	// Les alertes actives rafraîchissent leur copie (valeur, résumé, numéro).
	parEmpreinte := make(map[uint64]alertes.Alerte, len(actives))
	for _, a := range actives {
		parEmpreinte[a.Etiquettes.Hash()] = a
	}
	// Site injoignable (site.go) : les pannes de ses machines passent dans
	// un seul groupe, donc un seul message.
	g.sitesBas = g.sourdine.sitesInjoignables(actives)
	g.regrouperSites(g.sitesBas, t)
	enSourd := map[uint64]string{}
	var messages []*Message
	cles := make([]string, 0, len(g.groupes))
	for c := range g.groupes {
		cles = append(cles, c)
	}
	sort.Strings(cles)
	for _, c := range cles {
		gr := g.groupes[c]
		if m := g.decider(gr, t, actives, parEmpreinte, enSourd); m != nil {
			messages = append(messages, m)
		}
		if len(gr.alertes) == 0 {
			delete(g.groupes, c)
		}
	}
	g.enSourd = enSourd
	r := g.reglages
	g.mu.Unlock()

	for _, m := range messages {
		g.ajouterExtraits(ctx, m, &r, t)
		g.ajouterChangements(ctx, m)
		if r.Mode == Ombre {
			noterOmbre(ctx, g.opt.DB, m, &r, g.opt.Journal)
			continue
		}
		g.opt.Envoyeur.Envoyer(ctx, m, &r)
	}
}

// decider (sous verrou) : message à envoyer pour un groupe, ou nil.
func (g *Aiguilleur) decider(gr *groupe, t time.Time, actives []alertes.Alerte, parEmpreinte map[uint64]alertes.Alerte,
	enSourd map[uint64]string) *Message {
	var nouvelles, toujours, resolues []alertes.Alerte
	gravite := alertes.Info
	for h, s := range gr.alertes {
		if a, ok := parEmpreinte[h]; ok && !s.resolue {
			s.a = a
		}
		if s.resolue {
			if s.notifiee {
				resolues = append(resolues, s.a)
			} else {
				delete(gr.alertes, h) // jamais annoncée : sa fin ne l'est pas non plus
				delete(g.deplacees, h)
			}
			continue
		}
		if raison := g.sourdine.Raison(&s.a, actives, t); raison != "" {
			enSourd[h] = raison
			continue
		}
		if g.retenue(gr, s, t) {
			continue // tout le site va peut-être tomber : on attend un peu
		}
		if rang(s.a.Gravite) < rang(gravite) {
			gravite = s.a.Gravite
		}
		if s.notifiee {
			toujours = append(toujours, s.a)
		} else {
			nouvelles = append(nouvelles, s.a)
		}
	}
	if len(resolues) > 0 && len(nouvelles) == 0 && len(toujours) == 0 {
		gravite = resolues[0].Gravite
	}
	route := g.reglages.Routes[gravite]
	m := &Message{Le: t, Groupe: gr.cle, Gravite: gravite, Son: route.Son, Site: groupeSite(gr.cle)}
	if m.Site != "" && retourEnCours(resolues, append(nouvelles, toujours...), t) {
		return nil // des machines du site reviennent : on attend les autres
	}
	switch {
	case len(nouvelles)+len(resolues) > 0:
		attendre := gr.cree.Add(time.Duration(route.Attente))
		if !gr.dernierEnvoi.IsZero() {
			attendre = gr.dernierEnvoi.Add(time.Duration(route.Intervalle))
		}
		if t.Before(attendre) {
			return nil
		}
		m.Type = TypeAlerte
		if len(nouvelles) == 0 {
			m.Type = TypeResolution
			m.Son = false
		}
		trier(nouvelles)
		trier(toujours)
		trier(resolues)
		m.Actives, m.Nouvelles, m.Resolues = append(nouvelles, toujours...), len(nouvelles), resolues
	case len(toujours) > 0 && g.aEscalader(toujours, route, t):
		// Escalade : pas prises en charge à temps → aussi au canal de secours.
		for _, a := range toujours {
			if !g.acquis[a.ID] && !g.escaladees[a.ID] && !a.ActiveLe.IsZero() && !t.Before(a.ActiveLe.Add(time.Duration(route.Escalade))) {
				m.Actives = append(m.Actives, a)
				g.escaladees[a.ID] = true
			}
		}
		trier(m.Actives)
		m.Type, m.Son, m.Escalade = TypeRappel, route.Son, true
	case len(toujours) > 0 && (g.rappelForce(toujours) || route.Rappel > 0 && !t.Before(gr.dernierEnvoi.Add(time.Duration(route.Rappel)))):
		for _, a := range toujours {
			if !g.acquis[a.ID] {
				m.Actives = append(m.Actives, a)
			}
			delete(g.aRappeler, a.ID)
		}
		gr.dernierEnvoi = t // même si tout est acquitté : prochain rappel dans un intervalle
		if len(m.Actives) == 0 {
			return nil
		}
		trier(m.Actives)
		m.Type, m.Son = TypeRappel, false
	default:
		return nil
	}
	gr.dernierEnvoi = t
	for h, s := range gr.alertes {
		if s.resolue {
			delete(gr.alertes, h)
			delete(g.deplacees, h)
		} else if _, sourd := enSourd[h]; !sourd {
			s.notifiee = true
		}
	}
	m.Dependantes = g.dependantes(m.Actives, actives, t)
	return m
}

// dependantes : nombre d'alertes tues parce qu'une machine du message est en
// panne (le message le dit : « + N alertes d'équipements dépendants »).
func (g *Aiguilleur) dependantes(dans, actives []alertes.Alerte, t time.Time) int {
	if g.sourdine == nil {
		return 0
	}
	pannes := map[string]bool{}
	for _, a := range dans {
		if g.sourdine.reglesPanne[a.Regle] {
			pannes[a.Machine()] = true
		}
	}
	if len(pannes) == 0 {
		return 0
	}
	n := 0
	for i := range actives {
		if p := g.sourdine.parentEnPanne(&actives[i], actives); p != "" && pannes[p] {
			n++
		}
	}
	return n
}

func rang(g string) int {
	switch g {
	case alertes.Critique:
		return 0
	case alertes.Avertissement:
		return 1
	}
	return 2
}

func trier(as []alertes.Alerte) {
	sort.Slice(as, func(i, j int) bool {
		if as[i].Debut.Equal(as[j].Debut) {
			return labels.Compare(as[i].Etiquettes, as[j].Etiquettes) < 0
		}
		return as[i].Debut.Before(as[j].Debut)
	})
}

// RaisonSourdine : pourquoi une alerte est en sourdine (dernier tour), "" sinon.
func (g *Aiguilleur) RaisonSourdine(a *alertes.Alerte) string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.enSourd[a.Etiquettes.Hash()]
}

// Mode en cours (ombre ou actif).
func (g *Aiguilleur) Mode() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.reglages.Mode
}

// --- Historique des envois -------------------------------------------------------------

// Envoi : une ligne de l'historique.
type Envoi struct {
	Le                                   time.Time
	Canal, Groupe, Gravite, Type, Statut string
	Nb                                   int
	Duree                                time.Duration
	Secours                              bool
	Erreur, Texte                        string
}

// TailleMaxHistorique : texte gardé par envoi.
const TailleMaxHistorique = 4096

// NoterEnvoi ajoute une ligne à l'historique des envois.
func NoterEnvoi(ctx context.Context, db *sql.DB, e Envoi) error {
	if len(e.Texte) > TailleMaxHistorique {
		e.Texte = couper(e.Texte, TailleMaxHistorique)
	}
	if len(e.Erreur) > 512 {
		e.Erreur = e.Erreur[:512]
	}
	secours := 0
	if e.Secours {
		secours = 1
	}
	_, err := db.ExecContext(ctx, `INSERT INTO envois(le, canal, groupe, gravite, type, nb, statut, duree_ms, secours, erreur, texte)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, ms(e.Le), e.Canal, e.Groupe, e.Gravite, e.Type, e.Nb, e.Statut,
		e.Duree.Milliseconds(), secours, e.Erreur, e.Texte)
	return err
}

// ListerEnvois : derniers envois, plus récents d'abord.
func ListerEnvois(ctx context.Context, db *sql.DB, limite int) ([]Envoi, error) {
	rows, err := db.QueryContext(ctx, `SELECT le, canal, groupe, gravite, type, nb, statut, duree_ms, secours, erreur, texte
		FROM envois ORDER BY id DESC LIMIT ?`, limite)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Envoi
	for rows.Next() {
		var e Envoi
		var le, d, sec int64
		if err := rows.Scan(&le, &e.Canal, &e.Groupe, &e.Gravite, &e.Type, &e.Nb, &e.Statut, &d, &sec, &e.Erreur, &e.Texte); err != nil {
			return nil, err
		}
		e.Le, e.Duree, e.Secours = depuisMs(le), time.Duration(d)*time.Millisecond, sec == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// PurgerEnvois efface l'historique des envois avant la date donnée.
func PurgerEnvois(ctx context.Context, db *sql.DB, avant time.Time) (int64, error) {
	res, err := db.ExecContext(ctx, `DELETE FROM envois WHERE le < ?`, ms(avant))
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// noterOmbre : mode ombre, le message est seulement noté (statut « ombre »).
func noterOmbre(ctx context.Context, db *sql.DB, m *Message, r *Reglages, journal *slog.Logger) {
	loc, _ := time.LoadLocation(r.Fuseau)
	txt := Texte(m, r.mise(loc, r.Canaux.Principal))
	if err := NoterEnvoi(ctx, db, Envoi{Le: m.Le, Canal: "ombre", Groupe: m.Groupe, Gravite: m.Gravite, Type: m.Type,
		Nb: m.Nb(), Statut: Ombre, Texte: txt}); err != nil {
		journal.Error("historique des envois", "erreur", err)
	}
}

// rappelForce (sous verrou) : une alerte du groupe vient de sortir de sa
// prise en charge (durée passée) : rappel tout de suite.
func (g *Aiguilleur) rappelForce(toujours []alertes.Alerte) bool {
	for _, a := range toujours {
		if g.aRappeler[a.ID] {
			return true
		}
	}
	return false
}

// aEscalader (sous verrou) : une alerte du groupe n'est toujours pas prise
// en charge après le délai d'escalade de sa gravité (et pas encore escaladée).
func (g *Aiguilleur) aEscalader(toujours []alertes.Alerte, route Route, t time.Time) bool {
	if route.Escalade <= 0 {
		return false
	}
	if g.escaladees == nil {
		g.escaladees = map[int64]bool{}
	}
	for _, a := range toujours {
		if !g.acquis[a.ID] && !g.escaladees[a.ID] && !a.ActiveLe.IsZero() && !t.Before(a.ActiveLe.Add(time.Duration(route.Escalade))) {
			return true
		}
	}
	return false
}
