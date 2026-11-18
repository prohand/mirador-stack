// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/json"
	"errors"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
)

// Page « Logs » (SPEC § 18.5, refaite en § 33) : filtres rangés en
// pastilles, histogramme, lignes compactes et détail au clic, messages
// regroupés, contexte d'une ligne. Le contenu des logs vient des machines :
// toujours affiché comme du texte (html/template échappe tout ; le mode
// direct construit les lignes avec textContent).

const (
	lignesAnalyse   = 2000 // lignes lues pour l'histogramme et les regroupements
	autourMax       = 200  // lignes avant / après au plus (contexte)
	autourFenetre   = time.Hour
	exportMax       = 10_000
	exclusionsMax   = 10
	rechercheMaxLen = 200
)

// filtresLogs : la recherche lue dans l'adresse (page, direct, export).
type filtresLogs struct {
	Champs    map[string]string   // égalités (machine, source…)
	Variantes map[string][]string // niveaux « et plus grave », anciens noms
	Exclus    map[string][]string
	Mots      string
	Contient  string
	Recherche string // champ « Rechercher » tel que tapé
	Periode   string
	Debut     int64 // millisecondes
	Fin       int64
	Perso     bool // période choisie par zoom (de, a)
}

// niveauxPlusGraves : « err+ » et « warning+ » du filtre Niveau.
var niveauxPlusGraves = map[string][]string{
	"err+":     niveauxErreur,
	"warning+": append(append([]string{}, niveauxAvertissement...), niveauxErreur...),
}

var titresNiveauGrave = map[string]string{"err+": "err et plus grave", "warning+": "warning et plus grave"}

// lireRecherche : « mots entiers » et "texte exact" entre guillemets.
func lireRecherche(s string) (mots, contient string) {
	var reste []string
	for {
		i := strings.IndexByte(s, '"')
		if i < 0 {
			break
		}
		j := strings.IndexByte(s[i+1:], '"')
		if j < 0 {
			break
		}
		if contient == "" {
			contient = s[i+1 : i+1+j]
		} else {
			reste = append(reste, s[i+1:i+1+j]) // un seul texte exact : les autres deviennent des mots
		}
		s = s[:i] + " " + s[i+2+j:]
	}
	reste = append(reste, strings.Fields(strings.ReplaceAll(s, `"`, " "))...)
	return strings.Join(reste, " "), strings.TrimSpace(contient)
}

// motsDe : mots entiers (même découpage que le moteur : lettres et chiffres).
func motsDe(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool { return !unicode.IsLetter(r) && !unicode.IsDigit(r) })
}

func lireFiltresLogs(v url.Values, t time.Time) (filtresLogs, error) {
	f := filtresLogs{Champs: map[string]string{}, Periode: v.Get("periode")}
	for _, ch := range champsLogs {
		x := strings.TrimSpace(v.Get(ch.Nom))
		if x == "" {
			continue
		}
		if !reValeurChamp.MatchString(x) {
			return f, errors.New(ch.Titre + " : caractères non permis")
		}
		if ch.Nom == "level" {
			if graves, ok := niveauxPlusGraves[x]; ok {
				f.Champs["level"] = graves[0]
				f.Variantes = map[string][]string{"level": graves[1:]}
				continue
			}
		}
		f.Champs[ch.Nom] = x
	}
	for _, s := range v["sans"] {
		nom, val, ok := strings.Cut(s, ":")
		if !ok || !champConnu(nom) || !reValeurChamp.MatchString(val) {
			return f, errors.New("exclusion invalide")
		}
		if f.Exclus == nil {
			f.Exclus = map[string][]string{}
		}
		f.Exclus[nom] = append(f.Exclus[nom], val)
		if len(v["sans"]) > exclusionsMax {
			return f, errors.New("10 exclusions au plus")
		}
	}
	f.Recherche = strings.TrimSpace(v.Get("q"))
	f.Mots, f.Contient = lireRecherche(f.Recherche)
	// Anciennes adresses (raccourcis, liens enregistrés) : mots / contient.
	if m := strings.TrimSpace(v.Get("mots")); m != "" {
		f.Mots = strings.TrimSpace(f.Mots + " " + m)
		f.Recherche = strings.TrimSpace(f.Recherche + " " + m)
	}
	if c := strings.TrimSpace(v.Get("contient")); c != "" && f.Contient == "" {
		f.Contient = c
		f.Recherche = strings.TrimSpace(f.Recherche + ` "` + c + `"`)
	}
	if len(f.Recherche) > rechercheMaxLen {
		return f, errors.New("recherche : 200 caractères au plus")
	}
	// Période : zoom (de, a en millisecondes) ou durée jusqu'à maintenant.
	de, _ := strconv.ParseInt(v.Get("de"), 10, 64)
	a, _ := strconv.ParseInt(v.Get("a"), 10, 64)
	if de > 0 && a > de && a-de <= int64(31*24*time.Hour/time.Millisecond) {
		f.Debut, f.Fin, f.Perso = de, a, true
		return f, nil
	}
	if f.Periode == "" {
		f.Periode = "1h"
	}
	d, err := lirePeriode(f.Periode)
	if err != nil {
		return f, err
	}
	f.Debut, f.Fin = t.Add(-d).UnixMilli(), t.UnixMilli()
	return f, nil
}

func champConnu(nom string) bool {
	for _, ch := range champsLogs {
		if ch.Nom == nom {
			return true
		}
	}
	return false
}

// requete : la recherche pour le moteur.
func (f filtresLogs) requete(limite int) journaux.Requete {
	return journaux.Requete{Debut: f.Debut, Fin: f.Fin, Champs: f.Champs, Variantes: copieVariantes(f.Variantes),
		Exclus: f.Exclus, Mots: f.Mots, Contient: f.Contient, Limite: limite}
}

func copieVariantes(m map[string][]string) map[string][]string {
	if m == nil {
		return nil
	}
	out := make(map[string][]string, len(m))
	for k, v := range m {
		out[k] = append([]string(nil), v...)
	}
	return out
}

// --- Vue ----------------------------------------------------------------------------

type champVue struct {
	Nom, Titre, Valeur       string
	LienFiltrer, LienExclure string // vides : champ pas filtrable
}

type ligneVue struct {
	T                                  int64
	Heure, Jour                        string // Jour : séparateur avant cette ligne
	Host, Job, Niveau, Origine, Classe string
	Segments                           []segment
	Repetitions                        int
	Champs                             []champVue
	JSON                               string // « Copier en JSON »
	LienAutour                         string
	Cible                              bool // ligne visée par « Voir autour »
}

type triGroupes struct {
	Code, Titre, Lien string
	Actif             bool
}

type pastilleFiltre struct {
	Titre, Valeur, Lien string
	Exclu               bool
}

type raccourciLogs struct {
	ID        int64
	Nom, Lien string
	Perso     bool
}

type vueAutour struct {
	Host, Heure   string
	Avant, Apres  int
	LienPlusAvant string
	LienPlusApres string
	LienRetour    string
	Lignes        []ligneVue
}

type donneesLogs struct {
	Machine        champLog // filtres visibles
	Niveau         champLog
	Autres         []champLog // dans « Plus de filtres »
	Recherche      string
	Periode        string
	Periodes       []string
	Perso          bool
	PeriodePerso   string
	Limite         int
	Replier        bool
	Vue, Tri       string
	Motif          string
	Pastilles      []pastilleFiltre
	LienEffacer    string
	Raccourcis     []raccourciLogs
	Cherche        bool
	Lignes         []ligneVue
	NbTrouvees     int
	Tronque        bool
	Erreurs        int
	Avertis        int
	BlocsLus       int
	Duree          time.Duration
	DureeTexte     string // « 14 ms », « 1,2 s »
	Histo          histogramme
	Groupes        []groupeLogs
	Tris           []triGroupes
	Apprentissage  bool // registre des motifs de moins de 7 jours : pas encore de « nouveau »
	LienLignes     string
	LienGroupes    string
	LienPlusAncien string
	Autour         *vueAutour
	Requete        string // adresse de la recherche (enregistrer, exporter, direct)
	PlusOuvert     bool   // « Plus de filtres » déplié (un de ces filtres est actif)
}

// raccourcisFixes : recherches proposées à tous.
var raccourcisFixes = []raccourciLogs{
	{Nom: "Erreurs système (24 h)", Lien: "/logs?level=err%2B&periode=24h"},
	{Nom: "« error » dans Docker (24 h)", Lien: "/logs?job=docker&q=error&periode=24h"},
	{Nom: "Manque de mémoire (7 j)", Lien: "/logs?q=%22out+of+memory%22&periode=7d"},
	{Nom: "Flux bloqués", Lien: "/flux-bloques"},
}

// lienLogsAvec : adresse de la page avec la recherche actuelle modifiée.
func lienLogsAvec(v url.Values, modif func(url.Values)) string {
	w := url.Values{}
	for k, x := range v {
		if k == "avant" || k == "autour" || k == "n_avant" || k == "n_apres" {
			continue
		}
		w[k] = append([]string(nil), x...)
	}
	modif(w)
	if s := w.Encode(); s != "" {
		return "/logs?" + s
	}
	return "/logs"
}

// titreChamp : « machine », « unité »… (pastilles, détail).
func titreChamp(nom string) string {
	for _, ch := range champsLogs {
		if ch.Nom == nom {
			return strings.ToLower(ch.Titre)
		}
	}
	return nom
}

// presenter : lignes affichées (séparateur par jour, détail, surlignage).
func presenter(ls []stockage.Ligne, v url.Values, f filtresLogs, replier bool, t time.Time) []ligneVue {
	loc := fuseau.Load()
	mots := motsDe(f.Mots)
	var out []ligneVue
	jourPrec := ""
	for _, l := range ls {
		o := l.Champs["unit"]
		if o == "" {
			o = l.Champs["container"]
		}
		if n := len(out); replier && n > 0 && out[n-1].Host == l.Champs["host"] && out[n-1].Origine == o && strings.EqualFold(joindre(out[n-1].Segments), l.Msg) {
			out[n-1].Repetitions++
			continue
		}
		tl := time.UnixMilli(l.T).In(loc)
		x := ligneVue{T: l.T, Heure: tl.Format("15:04:05"), Host: l.Champs["host"], Job: l.Champs["job"], Niveau: l.Champs["level"],
			Origine: o, Classe: classeNiveau(l.Champs["level"]), Segments: surligner(l.Msg, mots, f.Contient), Repetitions: 1}
		if j := libelleJour(tl, t); j != jourPrec {
			x.Jour, jourPrec = j, j
		}
		noms := make([]string, 0, len(l.Champs))
		for k := range l.Champs {
			noms = append(noms, k)
		}
		sort.Strings(noms)
		for _, k := range noms {
			cv := champVue{Nom: k, Titre: titreChamp(k), Valeur: l.Champs[k]}
			if champConnu(k) && reValeurChamp.MatchString(cv.Valeur) {
				val := cv.Valeur
				cle := k
				cv.LienFiltrer = lienLogsAvec(v, func(w url.Values) { w.Set(cle, val); w.Del("motif") })
				cv.LienExclure = lienLogsAvec(v, func(w url.Values) {
					if w.Get(cle) == val {
						w.Del(cle)
					}
					w.Add("sans", cle+":"+val)
				})
			}
			x.Champs = append(x.Champs, cv)
		}
		if b, err := json.Marshal(map[string]any{"heure": tl.Format(time.RFC3339), "message": l.Msg, "champs": l.Champs}); err == nil {
			x.JSON = string(b)
		}
		if h := l.Champs["host"]; h != "" && reValeurChamp.MatchString(h) {
			x.LienAutour = "/logs?" + url.Values{"autour": {strconv.FormatInt(l.T, 10)}, "host": {h}}.Encode()
		}
		out = append(out, x)
	}
	return out
}

func joindre(segs []segment) string {
	var b strings.Builder
	for _, s := range segs {
		b.WriteString(s.Texte)
	}
	return b.String()
}

// pastilles : filtres actifs, chacun avec son lien « retirer ».
func pastilles(v url.Values, f filtresLogs) []pastilleFiltre {
	var out []pastilleFiltre
	for _, ch := range champsLogs {
		x := v.Get(ch.Nom)
		if x == "" {
			continue
		}
		val := x
		if t, ok := titresNiveauGrave[x]; ok {
			val = t
		}
		nom := ch.Nom
		out = append(out, pastilleFiltre{Titre: strings.ToLower(ch.Titre), Valeur: val, Lien: lienLogsAvec(v, func(w url.Values) { w.Del(nom) })})
	}
	for _, s := range v["sans"] {
		nom, val, _ := strings.Cut(s, ":")
		cible := s
		out = append(out, pastilleFiltre{Titre: "sans " + titreChamp(nom), Valeur: val, Exclu: true, Lien: lienLogsAvec(v, func(w url.Values) {
			var reste []string
			for _, y := range w["sans"] {
				if y != cible {
					reste = append(reste, y)
				}
			}
			w["sans"] = reste
		})})
	}
	if f.Mots != "" {
		out = append(out, pastilleFiltre{Titre: "mots", Valeur: f.Mots, Lien: lienLogsAvec(v, func(w url.Values) {
			_, c := lireRecherche(w.Get("q"))
			w.Del("mots")
			if c != "" {
				w.Set("q", `"`+c+`"`)
			} else {
				w.Del("q")
			}
		})})
	}
	if f.Contient != "" {
		out = append(out, pastilleFiltre{Titre: "texte", Valeur: f.Contient, Lien: lienLogsAvec(v, func(w url.Values) {
			m, _ := lireRecherche(w.Get("q"))
			w.Del("contient")
			if m != "" {
				w.Set("q", m)
			} else {
				w.Del("q")
			}
		})})
	}
	if f.Perso {
		loc := fuseau.Load()
		out = append(out, pastilleFiltre{Titre: "période", Valeur: time.UnixMilli(f.Debut).In(loc).Format("02/01 15:04") + " → " + time.UnixMilli(f.Fin).In(loc).Format("02/01 15:04"),
			Lien: lienLogsAvec(v, func(w url.Values) { w.Del("de"); w.Del("a") })})
	}
	if m := v.Get("motif"); m != "" {
		out = append(out, pastilleFiltre{Titre: "message type", Valeur: m, Lien: lienLogsAvec(v, func(w url.Values) { w.Del("motif") })})
	}
	return out
}

func (c *Console) pageLogs(q *req) {
	ctx := q.r.Context()
	v := q.r.URL.Query()
	d := donneesLogs{Periodes: periodesLogs, Replier: v.Get("replier") != "non", Vue: v.Get("vue"), Tri: v.Get("tri"), Motif: v.Get("motif")}
	if d.Vue != "groupes" {
		d.Vue = "lignes"
	}
	d.Limite, _ = strconv.Atoi(v.Get("limite"))
	if d.Limite <= 0 || d.Limite > 2000 {
		d.Limite = 300
	}
	f, err := lireFiltresLogs(v, q.t)
	d.Recherche, d.Periode, d.Perso = f.Recherche, f.Periode, f.Perso
	if f.Perso {
		loc := fuseau.Load()
		d.PeriodePerso = time.UnixMilli(f.Debut).In(loc).Format("02/01 15:04") + " → " + time.UnixMilli(f.Fin).In(loc).Format("15:04")
	}
	// Filtres et valeurs proposées.
	var machines []string
	if ms, e := flotte.ListerMachines(ctx, c.db); e == nil {
		for _, m := range ms {
			machines = append(machines, m.Nom)
		}
	}
	choix := c.choixLogs(ctx, machines)
	for _, ch := range champsLogs {
		x := strings.TrimSpace(v.Get(ch.Nom))
		cl := champLog{Nom: ch.Nom, Titre: ch.Titre, Valeur: x, Choix: avecValeur(choix[ch.Nom], x)}
		if ch.Nom == "level" {
			cl.Choix = choix["level"]
		}
		switch ch.Nom {
		case "host":
			d.Machine = cl
		case "level":
			d.Niveau = cl
		default:
			d.Autres = append(d.Autres, cl)
			if x != "" {
				d.PlusOuvert = true
			}
		}
	}
	d.Pastilles = pastilles(v, f)
	if len(d.Pastilles) > 0 {
		d.LienEffacer = "/logs?periode=" + url.QueryEscape(f.Periode)
	}
	d.Raccourcis = append(d.Raccourcis, raccourcisFixes...)
	if perso, e := c.recherchesLogs(ctx, q.u.ID); e == nil {
		d.Raccourcis = append(d.Raccourcis, perso...)
	}
	w := url.Values{}
	for k, x := range v {
		if k != "avant" && k != "autour" && k != "n_avant" && k != "n_apres" && k != "vue" && k != "tri" {
			w[k] = x
		}
	}
	d.Requete = w.Encode()
	d.LienLignes = lienLogsAvec(v, func(w url.Values) { w.Del("vue"); w.Del("tri") })
	d.LienGroupes = lienLogsAvec(v, func(w url.Values) { w.Set("vue", "groupes"); w.Del("motif") })
	for _, t := range []triGroupes{{Code: "frequents", Titre: "Plus fréquents"}, {Code: "nouveaux", Titre: "Nouveaux d'abord"}, {Code: "rares", Titre: "Rares d'abord"}, {Code: "erreurs", Titre: "Erreurs d'abord"}} {
		tri := t.Code
		t.Lien = lienLogsAvec(v, func(w url.Values) { w.Set("vue", "groupes"); w.Set("tri", tri) })
		t.Actif = d.Tri == t.Code || (d.Tri == "" && t.Code == "frequents")
		d.Tris = append(d.Tris, t)
	}
	if err == nil && c.opt.Logs != nil {
		if a := v.Get("autour"); a != "" {
			d.Autour, err = c.autour(q, v, a)
		} else {
			err = c.chercherPourPage(q, v, f, &d)
		}
	}
	if err != nil {
		c.flash(q, "", err.Error())
	}
	c.rendre(q, "logs", "Logs", "logs", d)
}

// chercherPourPage : lignes, histogramme et regroupements.
func (c *Console) chercherPourPage(q *req, v url.Values, f filtresLogs, d *donneesLogs) error {
	r := f.requete(lignesAnalyse)
	if av, _ := strconv.ParseInt(v.Get("avant"), 10, 64); av > f.Debut && av <= f.Fin {
		r.Fin = av - 1 // « Charger plus ancien »
	}
	d.Cherche = true
	t0 := time.Now()
	res, err := c.chercherLogs(q.r.Context(), r)
	d.Duree = time.Since(t0).Round(time.Millisecond)
	d.DureeTexte = strconv.FormatInt(d.Duree.Milliseconds(), 10) + " ms"
	if d.Duree >= time.Second {
		d.DureeTexte = strings.Replace(strconv.FormatFloat(d.Duree.Seconds(), 'f', 1, 64), ".", ",", 1) + " s"
	}
	if err != nil {
		return err
	}
	ls := res.Lignes
	if d.Motif != "" {
		ls = lignesDuMotif(ls, d.Motif)
	}
	d.NbTrouvees, d.Tronque, d.BlocsLus = len(ls), res.Tronque, res.BlocsLus
	for _, l := range ls {
		switch classeNiveau(l.Champs["level"]) {
		case "err":
			d.Erreurs++
		case "warn":
			d.Avertis++
		}
	}
	d.Histo = calculerHistogramme(ls, r.Debut, r.Fin, fuseau.Load())
	if d.Vue == "groupes" {
		d.Groupes = regrouper(ls, d.Tri, r.Debut, r.Fin, c.opt.Motifs, q.t)
		d.Apprentissage = c.opt.Motifs != nil && c.opt.Motifs.Apprentissage(q.t)
	}
	affichees := ls
	if len(affichees) > d.Limite {
		affichees = affichees[:d.Limite]
	}
	d.Lignes = presenter(affichees, v, f, d.Replier, q.t)
	if len(affichees) > 0 && (len(ls) > len(affichees) || res.Tronque) {
		plusAncienne := affichees[len(affichees)-1].T
		d.LienPlusAncien = lienLogsAvec(v, func(w url.Values) { w.Set("avant", strconv.FormatInt(plusAncienne, 10)) })
	}
	return nil
}

// autour : lignes de la même machine avant et après une ligne (toutes
// sources, sans les autres filtres).
func (c *Console) autour(q *req, v url.Values, a string) (*vueAutour, error) {
	t, err := strconv.ParseInt(a, 10, 64)
	h := v.Get("host")
	if err != nil || t <= 0 || h == "" || !reValeurChamp.MatchString(h) {
		return nil, errors.New("contexte : ligne ou machine invalide")
	}
	nAvant, _ := strconv.Atoi(v.Get("n_avant"))
	nApres, _ := strconv.Atoi(v.Get("n_apres"))
	if nAvant <= 0 {
		nAvant = 20
	}
	if nApres <= 0 {
		nApres = 20
	}
	nAvant, nApres = min(nAvant, autourMax), min(nApres, autourMax)
	res, err := c.chercherLogs(q.r.Context(), journaux.Requete{Debut: t - autourFenetre.Milliseconds(), Fin: t + autourFenetre.Milliseconds(),
		Champs: map[string]string{"host": h}, Limite: lignesAnalyse})
	if err != nil {
		return nil, err
	}
	ls := res.Lignes
	sort.SliceStable(ls, func(i, j int) bool { return ls[i].T < ls[j].T }) // du plus ancien au plus récent
	i := sort.Search(len(ls), func(k int) bool { return ls[k].T >= t })
	debut, fin := max(0, i-nAvant), min(len(ls), i+nApres+1)
	vue := &vueAutour{Host: h, Heure: time.UnixMilli(t).In(fuseau.Load()).Format("15:04:05"), Avant: nAvant, Apres: nApres,
		LienRetour: "/logs?" + url.Values{"host": {h}, "periode": {"24h"}}.Encode()}
	base := url.Values{"autour": {a}, "host": {h}}
	plus := func(nA, nP int) string {
		w := url.Values{}
		for k, x := range base {
			w[k] = x
		}
		w.Set("n_avant", strconv.Itoa(nA))
		w.Set("n_apres", strconv.Itoa(nP))
		return "/logs?" + w.Encode()
	}
	vue.LienPlusAvant, vue.LienPlusApres = plus(nAvant+20, nApres), plus(nAvant, nApres+20)
	vue.Lignes = presenter(ls[debut:fin], url.Values{"host": {h}, "periode": {"24h"}}, filtresLogs{}, false, q.t)
	for k := range vue.Lignes {
		vue.Lignes[k].Cible = vue.Lignes[k].T == t
	}
	return vue, nil
}
