// SPDX-License-Identifier: GPL-3.0-or-later

package alertes

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"sort"
	"sync"
	"text/template"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"github.com/prometheus/prometheus/promql"
	"github.com/prometheus/prometheus/promql/parser"
	"github.com/prometheus/prometheus/storage"
)

// États d'une alerte.
const (
	EnAttente = "en_attente" // condition vraie, durée « pour » pas encore atteinte
	Active    = "active"     // en alerte
	Resolue   = "resolue"
)

// Alerte : une alerte (règle + étiquettes de la série).
type Alerte struct {
	ID                        int64
	Regle                     string
	Etiquettes                labels.Labels // dont alertname et severity
	Gravite                   string
	Etat                      string
	Debut                     time.Time // condition vraie depuis
	ActiveLe                  time.Time
	Fin                       time.Time
	ValeurDebut               float64
	ValeurMax                 float64
	Valeur                    float64
	Resume                    string
	Description               string
	AcquitteeLe               time.Time
	AcquitteePar, Commentaire string

	vueLe   time.Time // dernière évaluation où la condition était vraie
	aEcrire bool      // état changé depuis la dernière écriture en base
}

// Machine : nom de l'équipement (instance, sinon host, sinon src_ip), comme
// le modèle Telegram actuel.
func (a *Alerte) Machine() string {
	for _, n := range []string{"instance", "host", "src_ip"} {
		if v := a.Etiquettes.Get(n); v != "" {
			return v
		}
	}
	return ""
}

// Evenement : changement à notifier.
type Evenement struct {
	Type   string // "debut" (passée en alerte) ou "fin" (résolue)
	Alerte Alerte // copie
}

// Recepteur : reçoit les changements après chaque évaluation (notifications,
// partie 3). Appelé hors de tout verrou du moteur ; ne doit pas bloquer.
type Recepteur interface {
	Recevoir(evs []Evenement)
}

// Options du moteur.
type Options struct {
	DB         *sql.DB
	Donnees    storage.Queryable // base brute des métriques
	Journal    *slog.Logger
	Maintenant func() time.Time
	Recepteur  Recepteur
	// Délai après le démarrage pendant lequel aucune alerte ne passe en
	// alerte (les agents vident leur file d'attente) : 3 min par défaut.
	Grace time.Duration
	// Limites de chaque évaluation (protection mémoire et CPU).
	MaxPoints int           // points chargés en même temps : 1 000 000 par défaut (~16 Mo)
	Delai     time.Duration // 10 s par défaut
}

// etatRegle : une règle chargée et son état d'évaluation.
type etatRegle struct {
	def      Regle
	expr     parser.Expr
	resume   *template.Template
	descr    *template.Template
	decalage time.Duration // étale les évaluations dans l'intervalle
	prochain time.Time
	alertes  map[uint64]*Alerte

	// Santé (affichée par la commande regles, métriques internes).
	derniere     time.Time
	duree        time.Duration
	resultats    int
	erreur       string
	nbErreurs    uint64
	nbEvaluation uint64
}

// SanteRegle : état d'évaluation d'une règle.
type SanteRegle struct {
	Nom, Groupe, Erreur  string
	Active               bool
	Derniere             time.Time
	Duree                time.Duration
	Resultats            int
	Evaluations, Erreurs uint64
	EnAlerte, EnAttente  int
}

// Moteur : évalue les règles et tient l'état des alertes.
type Moteur struct {
	opt    Options
	moteur *promql.Engine
	debut  time.Time

	mu         sync.Mutex
	regles     map[string]*etatRegle
	generation int64
	resolues   []*Alerte     // résolues depuis la dernière écriture (historique)
	derniereOK time.Time     // dernière évaluation complète sans erreur de stockage
	cumul      time.Duration // temps passé à évaluer depuis le démarrage
}

// Nouveau crée le moteur, charge les règles et les alertes en cours.
func Nouveau(ctx context.Context, opt Options) (*Moteur, error) {
	if opt.Journal == nil {
		opt.Journal = slog.New(slog.DiscardHandler)
	}
	if opt.Maintenant == nil {
		opt.Maintenant = time.Now
	}
	if opt.Grace == 0 {
		opt.Grace = 3 * time.Minute
	}
	if opt.MaxPoints <= 0 {
		opt.MaxPoints = 1_000_000
	}
	if opt.Delai <= 0 {
		opt.Delai = 10 * time.Second
	}
	m := &Moteur{opt: opt, debut: opt.Maintenant(), regles: map[string]*etatRegle{}, generation: -1}
	m.moteur = promql.NewEngine(promql.EngineOpts{
		Logger:        opt.Journal.With("composant", "promql"),
		MaxSamples:    opt.MaxPoints,
		Timeout:       opt.Delai,
		LookbackDelta: 5 * time.Minute,
		Parser:        analyseur, // fonctions permises + lag
	})
	if err := m.Recharger(ctx); err != nil {
		return nil, err
	}
	if err := m.restaurer(ctx); err != nil {
		return nil, fmt.Errorf("alertes en cours : %w", err)
	}
	return m, nil
}

// --- Chargement des règles ---------------------------------------------------------

// Recharger relit les règles si elles ont changé (compteur « regles »).
// Une règle retirée ou désactivée : ses alertes sont résolues.
func (m *Moteur) Recharger(ctx context.Context) error {
	gen, err := LireGeneration(ctx, m.opt.DB, "regles")
	if err != nil {
		return err
	}
	m.mu.Lock()
	inchange := gen == m.generation
	m.mu.Unlock()
	if inchange {
		return nil
	}
	defs, err := ListerRegles(ctx, m.opt.DB)
	if err != nil {
		return err
	}
	maintenant := m.opt.Maintenant()
	var evs []Evenement
	m.mu.Lock()
	vues := map[string]bool{}
	for _, d := range defs {
		if !d.Active {
			continue
		}
		e, err := d.Valider()
		if err != nil { // vérifiée à l'enregistrement : ne devrait pas arriver
			m.opt.Journal.Error("règle illisible ignorée", "regle", d.Nom, "erreur", err)
			continue
		}
		vues[d.Nom] = true
		r := m.regles[d.Nom]
		if r == nil {
			r = &etatRegle{alertes: map[uint64]*Alerte{}}
			m.regles[d.Nom] = r
		}
		r.def, r.expr = d, e
		r.resume, _ = compilerModele(d.Resume)
		r.descr, _ = compilerModele(d.Description)
		r.decalage = decalage(d.Nom, time.Duration(d.Intervalle))
		r.prochain = prochaine(maintenant, time.Duration(d.Intervalle), r.decalage)
		// Gravité ou étiquettes changées : les alertes en cours gardent les
		// leurs jusqu'à leur fin (pas de fausse fin + nouveau début).
	}
	for nom, r := range m.regles {
		if vues[nom] {
			continue
		}
		for k, a := range r.alertes {
			if a.Etat == Active {
				evs = append(evs, m.resoudre(a, maintenant))
			} else {
				m.resolues = append(m.resolues, a) // en attente : ligne effacée
				a.Etat = ""
			}
			delete(r.alertes, k)
		}
		delete(m.regles, nom)
	}
	m.generation = gen
	m.mu.Unlock()
	m.ecrire(ctx)
	m.envoyer(evs)
	m.opt.Journal.Info("règles d'alerte chargées", "actives", len(vues), "total", len(defs))
	return nil
}

// decalage : position fixe de la règle dans son intervalle (d'après son nom),
// pour ne pas évaluer toutes les règles à la même seconde.
func decalage(nom string, iv time.Duration) time.Duration {
	h := fnv.New32a()
	_, _ = h.Write([]byte(nom))
	n := int64(iv / time.Second)
	if n <= 0 {
		return 0
	}
	return time.Duration(int64(h.Sum32())%n) * time.Second
}

// prochaine évaluation après t, alignée sur l'intervalle + décalage.
func prochaine(t time.Time, iv, dec time.Duration) time.Time {
	p := t.Truncate(iv).Add(dec)
	for !p.After(t) {
		p = p.Add(iv)
	}
	return p
}

// restaurer relit les alertes en cours (redémarrage du serveur) : pas de
// nouvelle notification, « depuis » conservé, durée « pour » continuée.
func (m *Moteur) restaurer(ctx context.Context) error {
	rows, err := m.opt.DB.QueryContext(ctx, `SELECT id, regle, etiquettes, gravite, etat, debut, active_le,
		valeur_debut, valeur_max, valeur, resume, description, acquittee_le, acquittee_par, commentaire
		FROM alertes WHERE etat != 'resolue'`)
	if err != nil {
		return err
	}
	defer rows.Close()
	m.mu.Lock()
	defer m.mu.Unlock()
	var orphelines []*Alerte
	for rows.Next() {
		var a Alerte
		var etq string
		var debut, activeLe, ackLe int64
		if err := rows.Scan(&a.ID, &a.Regle, &etq, &a.Gravite, &a.Etat, &debut, &activeLe,
			&a.ValeurDebut, &a.ValeurMax, &a.Valeur, &a.Resume, &a.Description, &ackLe, &a.AcquitteePar, &a.Commentaire); err != nil {
			return err
		}
		a.Etiquettes = etiquettesJSON(etq)
		a.Debut, a.ActiveLe, a.AcquitteeLe = depuisMs(debut), depuisMs(activeLe), depuisMs(ackLe)
		a.vueLe = m.opt.Maintenant()
		r := m.regles[a.Regle]
		if r == nil { // règle retirée pendant l'arrêt
			orphelines = append(orphelines, &a)
			continue
		}
		r.alertes[a.Etiquettes.Hash()] = &a
	}
	if err := rows.Err(); err != nil {
		return err
	}
	maintenant := m.opt.Maintenant()
	for _, a := range orphelines {
		if a.Etat == Active {
			m.resoudre(a, maintenant) // pas de notification : règle déjà retirée
		} else {
			a.Etat = ""
			m.resolues = append(m.resolues, a)
		}
	}
	return nil
}

func depuisMs(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func versMs(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

// --- Évaluation -----------------------------------------------------------------------

// Boucle évalue chaque règle à son rythme et recharge les règles quand elles
// changent (vérifié toutes les 10 s). Rend la main quand ctx est annulé.
func (m *Moteur) Boucle(ctx context.Context) {
	verif := time.NewTicker(10 * time.Second)
	defer verif.Stop()
	for {
		attente := time.Until(m.prochaineEcheance())
		if attente < 0 {
			attente = 0
		}
		minuterie := time.NewTimer(attente)
		select {
		case <-ctx.Done():
			minuterie.Stop()
			return
		case <-verif.C:
			minuterie.Stop()
			if err := m.Recharger(ctx); err != nil {
				m.opt.Journal.Error("rechargement des règles", "erreur", err)
			}
		case <-minuterie.C:
			m.EvaluerDues(ctx, m.opt.Maintenant())
		}
	}
}

func (m *Moteur) prochaineEcheance() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	p := m.opt.Maintenant().Add(10 * time.Second)
	for _, r := range m.regles {
		if r.prochain.Before(p) {
			p = r.prochain
		}
	}
	return p
}

// EvaluerDues évalue les règles arrivées à échéance à l'instant t, écrit les
// changements en une transaction et les transmet au récepteur.
func (m *Moteur) EvaluerDues(ctx context.Context, t time.Time) {
	m.mu.Lock()
	var dues []*etatRegle
	for _, r := range m.regles {
		if !r.prochain.After(t) {
			dues = append(dues, r)
		}
	}
	m.mu.Unlock()
	sort.Slice(dues, func(i, j int) bool { return dues[i].def.Nom < dues[j].def.Nom })
	var evs []Evenement
	toutesOK := true
	for _, r := range dues {
		ts := r.prochain
		if ts.Before(t.Add(-time.Duration(r.def.Intervalle))) {
			ts = t // retard (serveur suspendu) : on évalue maintenant, sans rattrapage
		}
		e, ok := m.evaluer(ctx, r, ts)
		evs = append(evs, e...)
		toutesOK = toutesOK && ok
		m.mu.Lock()
		r.prochain = prochaine(t, time.Duration(r.def.Intervalle), r.decalage)
		m.mu.Unlock()
	}
	if len(dues) > 0 && toutesOK {
		m.mu.Lock()
		m.derniereOK = t
		m.mu.Unlock()
	}
	m.ecrire(ctx)
	m.envoyer(evs)
}

// EvaluerTout évalue toutes les règles à l'instant t (tests, commande « essai »).
func (m *Moteur) EvaluerTout(ctx context.Context, t time.Time) []Evenement {
	m.mu.Lock()
	var toutes []*etatRegle
	for _, r := range m.regles {
		toutes = append(toutes, r)
	}
	m.mu.Unlock()
	sort.Slice(toutes, func(i, j int) bool { return toutes[i].def.Nom < toutes[j].def.Nom })
	var evs []Evenement
	for _, r := range toutes {
		e, _ := m.evaluer(ctx, r, t)
		evs = append(evs, e...)
	}
	m.ecrire(ctx)
	m.envoyer(evs)
	return evs
}

// resultat : une série renvoyée par l'expression.
type resultat struct {
	etq    labels.Labels
	valeur float64
}

// requete exécute l'expression d'une règle à l'instant ts.
func (m *Moteur) requete(ctx context.Context, expr string, ts time.Time) ([]resultat, error) {
	q, err := m.moteur.NewInstantQuery(ctx, m.opt.Donnees, nil, expr, ts)
	if err != nil {
		return nil, err
	}
	defer q.Close()
	res := q.Exec(ctx)
	if res.Err != nil {
		return nil, res.Err
	}
	switch v := res.Value.(type) {
	case promql.Vector:
		out := make([]resultat, 0, len(v))
		for _, s := range v {
			if s.H != nil {
				continue // histogrammes natifs : non utilisés
			}
			out = append(out, resultat{etq: s.Metric, valeur: s.F})
		}
		return out, nil
	case promql.Scalar:
		// Une expression scalaire ne filtre rien : une seule alerte, sans étiquette.
		return []resultat{{etq: labels.EmptyLabels(), valeur: v.V}}, nil
	}
	return nil, fmt.Errorf("résultat de type %s inattendu", res.Value.Type())
}

// evaluer : une règle, à l'instant ts. Renvoie les changements à notifier et
// false si le stockage a échoué.
func (m *Moteur) evaluer(ctx context.Context, r *etatRegle, ts time.Time) ([]Evenement, bool) {
	t0 := time.Now()
	res, err := m.requete(ctx, r.def.Expr, ts)
	duree := time.Since(t0)

	m.mu.Lock()
	defer m.mu.Unlock()
	r.derniere, r.duree = ts, duree
	r.nbEvaluation++
	m.cumul += duree
	if err != nil {
		r.nbErreurs++
		if r.erreur != err.Error() { // journal sans répétition
			m.opt.Journal.Warn("évaluation d'une règle", "regle", r.def.Nom, "erreur", err)
		}
		r.erreur = err.Error()
		// Erreur : on garde l'état tel quel (ni début, ni fin sur une panne).
		// Seules les pannes du stockage ou les délais dépassés comptent pour le
		// heartbeat : une règle mal écrite ne doit pas le couper.
		var es promql.ErrStorage
		var ed promql.ErrQueryTimeout
		return nil, !errors.As(err, &es) && !errors.As(err, &ed) && !errors.Is(err, context.DeadlineExceeded)
	}
	r.erreur = ""
	r.resultats = len(res)

	var evs []Evenement
	vues := make(map[uint64]bool, len(res))
	enGrace := ts.Before(m.debut.Add(m.opt.Grace))
	for _, x := range res {
		etq := etiquettesAlerte(x.etq, &r.def)
		h := etq.Hash()
		if vues[h] {
			continue // deux séries donnent les mêmes étiquettes : une seule alerte
		}
		vues[h] = true
		a := r.alertes[h]
		if a == nil {
			a = &Alerte{Regle: r.def.Nom, Etiquettes: etq, Gravite: r.def.Gravite, Etat: EnAttente,
				Debut: ts, ValeurMax: x.valeur, aEcrire: true}
			r.alertes[h] = a
		}
		a.Valeur, a.vueLe = x.valeur, ts
		if x.valeur > a.ValeurMax || math.IsNaN(a.ValeurMax) {
			a.ValeurMax = x.valeur
		}
		if a.Etat == EnAttente && ts.Sub(a.Debut) >= time.Duration(r.def.Pour) && !enGrace {
			a.Etat, a.ActiveLe, a.ValeurDebut = Active, ts, x.valeur
			m.textes(r, a)
			a.aEcrire = true
			evs = append(evs, Evenement{Type: "debut", Alerte: *a})
			continue
		}
		if a.Etat == Active {
			m.textes(r, a)
		}
	}
	for h, a := range r.alertes {
		if vues[h] {
			continue
		}
		if a.Etat == Active && ts.Sub(a.vueLe) < time.Duration(r.def.Garder) {
			continue // « garder » : encore active un moment
		}
		if a.Etat == Active {
			evs = append(evs, m.resoudre(a, ts))
		} else {
			a.Etat = ""
			m.resolues = append(m.resolues, a) // en attente puis disparue : effacée
		}
		delete(r.alertes, h)
	}
	return evs, true
}

// etiquettesAlerte : étiquettes de la série sans __name__, + celles de la
// règle, + alertname et severity (comme vmalert).
func etiquettesAlerte(serie labels.Labels, r *Regle) labels.Labels {
	b := labels.NewBuilder(serie)
	b.Del(labels.MetricName)
	for k, v := range r.Etiquettes {
		b.Set(k, v)
	}
	b.Set("alertname", r.Nom)
	b.Set("severity", r.Gravite)
	return b.Labels()
}

func (m *Moteur) textes(r *etatRegle, a *Alerte) {
	mp := a.Etiquettes.Map()
	a.Resume = remplir(r.resume, r.def.Resume, mp, a.Valeur)
	a.Description = remplir(r.descr, r.def.Description, mp, a.Valeur)
}

// resoudre (sous verrou) : fin d'une alerte active.
func (m *Moteur) resoudre(a *Alerte, t time.Time) Evenement {
	a.Etat, a.Fin = Resolue, t
	m.resolues = append(m.resolues, a)
	return Evenement{Type: "fin", Alerte: *a}
}

func (m *Moteur) envoyer(evs []Evenement) {
	m.mu.Lock()
	r := m.opt.Recepteur
	m.mu.Unlock()
	if len(evs) > 0 && r != nil {
		r.Recevoir(evs)
	}
}

// DefinirRecepteur branche le récepteur après la création (l'aiguillage des
// notifications a besoin du moteur pour être créé).
func (m *Moteur) DefinirRecepteur(r Recepteur) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.opt.Recepteur = r
}

// --- Écriture en base ------------------------------------------------------------

// ecrire enregistre en une transaction : nouvelles alertes (en attente ou
// actives), passages en alerte, fins, effacements. Les alertes dont l'état
// n'a pas changé ne sont pas réécrites (une écriture par changement d'état).
// En cas d'échec, rien n'est perdu en mémoire : nouvel essai au tour suivant.
func (m *Moteur) ecrire(ctx context.Context) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var aEcrire []*Alerte
	for _, r := range m.regles {
		for _, a := range r.alertes {
			if a.aEcrire {
				aEcrire = append(aEcrire, a)
			}
		}
	}
	if len(aEcrire) == 0 && len(m.resolues) == 0 {
		return
	}
	nouveaux := map[*Alerte]int64{} // identifiants donnés par la base, posés après Commit
	err := func() error {
		tx, err := m.opt.DB.BeginTx(ctx, nil)
		if err != nil {
			return err
		}
		defer tx.Rollback() //nolint:errcheck // sans effet après Commit
		for _, a := range m.resolues {
			switch {
			case a.Etat == "" && a.ID > 0: // en attente puis disparue
				_, err = tx.ExecContext(ctx, `DELETE FROM alertes WHERE id = ?`, a.ID)
			case a.Etat == Resolue && a.ID > 0:
				err = m.mettreAJour(ctx, tx, a)
			case a.Etat == Resolue: // jamais écrite (échec d'écriture précédent)
				_, err = m.inserer(ctx, tx, a)
			}
			if err != nil {
				return err
			}
		}
		for _, a := range aEcrire {
			if a.ID > 0 {
				if err := m.mettreAJour(ctx, tx, a); err != nil {
					return err
				}
				continue
			}
			id, err := m.inserer(ctx, tx, a)
			if err != nil {
				return err
			}
			nouveaux[a] = id
		}
		return tx.Commit()
	}()
	if err != nil {
		m.opt.Journal.Error("écriture des alertes", "erreur", err)
		return
	}
	for _, a := range aEcrire {
		a.aEcrire = false
		if id, ok := nouveaux[a]; ok {
			a.ID = id
		}
	}
	m.resolues = m.resolues[:0]
}

func (m *Moteur) mettreAJour(ctx context.Context, tx *sql.Tx, a *Alerte) error {
	_, err := tx.ExecContext(ctx, `UPDATE alertes SET etat = ?, active_le = ?, fin = ?, valeur_debut = ?, valeur = ?,
		valeur_max = ?, resume = ?, description = ? WHERE id = ?`,
		a.Etat, versMs(a.ActiveLe), versMs(a.Fin), a.ValeurDebut, a.Valeur, a.ValeurMax, a.Resume, a.Description, a.ID)
	return err
}

func (m *Moteur) inserer(ctx context.Context, tx *sql.Tx, a *Alerte) (int64, error) {
	etq, err := json.Marshal(a.Etiquettes.Map())
	if err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, `INSERT INTO alertes(regle, etiquettes, gravite, etat, debut, active_le, fin,
		valeur_debut, valeur_max, valeur, resume, description) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Regle, string(etq), a.Gravite, a.Etat, versMs(a.Debut), versMs(a.ActiveLe), versMs(a.Fin),
		a.ValeurDebut, a.ValeurMax, a.Valeur, a.Resume, a.Description)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// --- Lecture de l'état ---------------------------------------------------------------

// EnCours renvoie une copie des alertes en cours (actives et en attente),
// triées par gravité puis début.
func (m *Moteur) EnCours() []Alerte {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Alerte
	for _, r := range m.regles {
		for _, a := range r.alertes {
			out = append(out, *a)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if gi, gj := rangGravite(out[i].Gravite), rangGravite(out[j].Gravite); gi != gj {
			return gi < gj
		}
		return out[i].Debut.Before(out[j].Debut)
	})
	return out
}

func rangGravite(g string) int {
	switch g {
	case Critique:
		return 0
	case Avertissement:
		return 1
	}
	return 2
}

// Sante : état d'évaluation de chaque règle chargée.
func (m *Moteur) Sante() []SanteRegle {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]SanteRegle, 0, len(m.regles))
	for _, r := range m.regles {
		s := SanteRegle{Nom: r.def.Nom, Groupe: r.def.Groupe, Active: true, Erreur: r.erreur, Derniere: r.derniere,
			Duree: r.duree, Resultats: r.resultats, Evaluations: r.nbEvaluation, Erreurs: r.nbErreurs}
		for _, a := range r.alertes {
			if a.Etat == Active {
				s.EnAlerte++
			} else {
				s.EnAttente++
			}
		}
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nom < out[j].Nom })
	return out
}

// DerniereEvaluationOK : dernière fois où toutes les règles dues ont été
// évaluées sans erreur de stockage. Le heartbeat externe s'arrête si elle est
// trop ancienne (le serveur ne peut plus évaluer ses règles).
func (m *Moteur) DerniereEvaluationOK() time.Time {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.derniereOK
}

// DureeCumulee : temps total passé à évaluer les règles depuis le démarrage
// (métrique interne : sa hausse par minute = coût du moteur).
func (m *Moteur) DureeCumulee() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.cumul
}
