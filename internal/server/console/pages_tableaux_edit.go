// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode"
)

// Éditeur de tableaux (SPEC § 18.14), admins seulement : créer, dupliquer,
// importer (Grafana ou mirador), exporter, supprimer, rétablir un tableau
// fourni ; dans un tableau : propriétés, rangées, panneaux (formulaire et
// aperçu), largeur et ordre (glisser-déposer). Chaque changement repart du
// tableau en service (copie), est vérifié en entier (Valider) puis enregistré
// avec son audit. Une empreinte refuse un changement fait sur une version
// dépassée (deux onglets ouverts).

func (c *Console) routesTableaux() {
	m := c.mux
	m.HandleFunc("POST /tableaux", c.admin(c.postTableaux))
	m.HandleFunc("GET /tableaux/{nom}/export.json", c.lecture(c.exporterTableau))
	m.HandleFunc("GET /tableaux/{nom}/supprimer", c.admin(c.pageSupprimerTableau))
	m.HandleFunc("GET /tableaux/{nom}/modifier", c.admin(c.pageModifierTableau))
	m.HandleFunc("POST /tableaux/{nom}/modifier", c.admin(c.postModifierTableau))
	m.HandleFunc("GET /tableaux/{nom}/panneau", c.admin(c.pagePanneau))
	m.HandleFunc("POST /tableaux/{nom}/panneau", c.admin(c.postPanneau))
}

// --- Liste : créer, dupliquer, importer, supprimer, rétablir -----------------------

type ligneTableau struct {
	T       *Tableau
	Origine string
	Nb      int // panneaux
}

type donneesTableaux struct {
	Lignes    []ligneTableau
	Catalogue []ligneTableau // tableaux fournis proposés, pas encore installés
	// Raccourcis vers les pages Logs et Flux bloqués : installés / proposés.
	Pages, PagesProposees []PageRaccourci
}

func (c *Console) pageTableaux(q *req) {
	var d donneesTableaux
	for _, t := range c.listeTableaux() {
		n := 0
		for _, r := range t.Rangees {
			n += len(r.Panneaux)
		}
		d.Lignes = append(d.Lignes, ligneTableau{T: t, Origine: c.origineTableau(t.Nom), Nb: n})
	}
	d.Pages, d.PagesProposees = c.pagesInstallees(q.r.Context())
	for _, t := range c.etatTab().catalogue {
		n := 0
		for _, r := range t.Rangees {
			n += len(r.Panneaux)
		}
		d.Catalogue = append(d.Catalogue, ligneTableau{T: t, Origine: OrigineFourni, Nb: n})
	}
	c.rendre(q, "tableaux", "Tableaux", "tableaux", d)
}

// nomDepuisTitre : « Serveurs Linux » → « serveurs-linux » (unique).
func (c *Console) nomDepuisTitre(titre string) string {
	r := strings.NewReplacer("à", "a", "â", "a", "ä", "a", "é", "e", "è", "e", "ê", "e", "ë", "e", "î", "i", "ï", "i",
		"ô", "o", "ö", "o", "ù", "u", "û", "u", "ü", "u", "ç", "c")
	var b strings.Builder
	tiret := false
	for _, x := range r.Replace(strings.ToLower(titre)) {
		if (x >= 'a' && x <= 'z') || (x >= '0' && x <= '9') {
			b.WriteRune(x)
			tiret = false
		} else if !tiret && b.Len() > 0 {
			b.WriteByte('-')
			tiret = true
		}
	}
	nom := strings.Trim(b.String(), "-")
	if len(nom) > 34 {
		nom = strings.Trim(nom[:34], "-")
	}
	if nom == "" {
		nom = "tableau"
	}
	base := nom
	for i := 2; c.tableau(nom) != nil || tableauxDepart[nom] != nil; i++ {
		nom = base + "-" + strconv.Itoa(i)
	}
	return nom
}

func verifierTitre(s string) error {
	if s == "" || len([]rune(s)) > 80 {
		return errors.New("titre : 1 à 80 caractères")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("titre : caractère interdit")
		}
	}
	return nil
}

func (c *Console) postTableaux(q *req) {
	f := champs{q}
	ctx := q.r.Context()
	if strings.HasPrefix(q.r.Header.Get("Content-Type"), "multipart/") {
		c.importerTableau(q)
		return
	}
	nom := f.texte("nom")
	var err error
	var ok, vers string
	switch f.texte("action") {
	case "creer":
		titre := f.texte("titre")
		if err = verifierTitre(titre); err == nil {
			t := &Tableau{Version: 1, Nom: c.nomDepuisTitre(titre), Titre: titre, Periode: "6h",
				Rangees: []Rangee{{Titre: "Général", Panneaux: []Panneau{panneauExemple()}}}}
			if err = c.EnregistrerTableau(ctx, t, "tableau_creer", q.qui(), q.ip.String(), q.t); err == nil {
				ok, vers = "tableau créé : ajoutez ses panneaux", "/tableaux/"+t.Nom+"/modifier"
			}
		}
	case "dupliquer":
		src, _ := c.tableauOuCatalogue(nom)
		if src == nil {
			err = errors.New("tableau inconnu")
			break
		}
		var t *Tableau
		if t, err = copierTableau(src); err == nil {
			t.Titre = src.Titre + " (copie)"
			if len([]rune(t.Titre)) > 80 {
				t.Titre = string([]rune(t.Titre)[:80])
			}
			t.Nom = c.nomDepuisTitre(t.Titre)
			if err = c.EnregistrerTableau(ctx, t, "tableau_dupliquer", q.qui(), q.ip.String(), q.t); err == nil {
				ok, vers = "copie créée", "/tableaux/"+t.Nom+"/modifier"
			}
		}
	case "supprimer":
		if err = c.verifierCode(q); err == nil {
			err = c.SupprimerTableau(ctx, nom, q.qui(), q.ip.String(), q.t)
			ok = "tableau supprimé"
			if tableauxDepart[nom] != nil {
				ok += " (tableau fourni : il est de nouveau proposé plus bas)"
			}
		} else {
			vers = "/tableaux/" + nom + "/supprimer"
		}
	case "installer-page", "retirer-page":
		err = c.installerPage(ctx, nom, f.texte("action") == "installer-page", q.qui(), q.ip.String(), q.t)
		ok = "raccourci enregistré"
	case "installer":
		if err = c.InstallerTableau(ctx, nom, q.qui(), q.ip.String(), q.t); err == nil {
			ok, vers = "tableau installé", "/tableaux/"+nom
		}
	case "retablir":
		err, ok = c.RetablirTableau(ctx, nom, q.qui(), q.ip.String(), q.t), "tableau rétabli tel que fourni avec mirador"
	default:
		err = errors.New("action inconnue")
	}
	if vers == "" {
		vers = "/tableaux"
	}
	c.retour(q, vers, err, ok)
}

// panneauExemple : premier panneau d'un tableau neuf (à modifier).
func panneauExemple() Panneau {
	return Panneau{Type: PanneauCourbe, Titre: "CPU utilisé", Unite: "percent",
		Requetes: []Requete{{ID: "A", Expr: `100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"}[$__rate_interval])))`, Legende: "{{instance}}"}}}
}

// importerTableau : JSON mirador (version 1) ou export Grafana.
func (c *Console) importerTableau(q *req) {
	err := func() error {
		if err := q.r.ParseMultipartForm(1 << 20); err != nil {
			return errors.New("fichier illisible (1 Mio au plus)")
		}
		defer q.r.MultipartForm.RemoveAll() //nolint:errcheck // rien sur disque sous 1 Mio
		fh, en, err := q.r.FormFile("fichier")
		if err != nil {
			return errors.New("aucun fichier")
		}
		defer fh.Close()
		brut, err := io.ReadAll(io.LimitReader(fh, 1<<20))
		if err != nil {
			return err
		}
		var essai struct {
			Version int    `json:"version"`
			Nom     string `json:"nom"`
			Panels  []any  `json:"panels"`
		}
		if err := json.Unmarshal(brut, &essai); err != nil {
			return errors.New("fichier JSON attendu (export Grafana ou tableau mirador)")
		}
		var t *Tableau
		if essai.Panels != nil {
			if t, err = DepuisGrafana(strings.TrimSuffix(en.Filename, ".json"), brut); err != nil {
				return fmt.Errorf("conversion Grafana : %w", err)
			}
		} else {
			t = &Tableau{}
			if err := json.Unmarshal(brut, t); err != nil {
				return err
			}
		}
		if t.Titre == "" {
			t.Titre = "Tableau importé"
		}
		// Toujours un nouveau nom : un import ne remplace jamais un tableau.
		t.Nom = c.nomDepuisTitre(t.Titre)
		if err := c.EnregistrerTableau(q.r.Context(), t, "tableau_importer", q.qui(), q.ip.String(), q.t); err != nil {
			return err
		}
		c.flash(q, "tableau importé : « "+t.Titre+" »", "")
		http.Redirect(q.w, q.r, "/tableaux/"+t.Nom, http.StatusSeeOther)
		return nil
	}()
	if err != nil {
		c.retour(q, "/tableaux", err, "")
	}
}

// pageSupprimerTableau : confirmation avec le code 2FA.
func (c *Console) pageSupprimerTableau(q *req) {
	t := c.tableau(q.r.PathValue("nom"))
	if t == nil {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Tableau introuvable", "tableaux", nil)
		return
	}
	c.rendre(q, "tableau-supprimer", "Supprimer « "+t.Titre+" »", "tableaux", ligneTableau{T: t, Origine: c.origineTableau(t.Nom)})
}

func (c *Console) exporterTableau(q *req) {
	t, _ := c.tableauOuCatalogue(q.r.PathValue("nom"))
	if t == nil {
		http.NotFound(q.w, q.r)
		return
	}
	q.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	q.w.Header().Set("Content-Disposition", `attachment; filename="`+t.Nom+`.json"`)
	e := json.NewEncoder(q.w)
	e.SetIndent("", "  ")
	_ = e.Encode(t)
}

// --- Un tableau : propriétés, rangées, ordre -------------------------------------

type vueEdPanneau struct {
	ID      string // « ri.pi » (position actuelle)
	P       *Panneau
	Largeur int // en douzièmes, pour la maquette (0 = selon le type)
}

type vueEdRangee struct {
	Index    int
	R        *Rangee
	Panneaux []vueEdPanneau
}

type donneesEdTableau struct {
	Icones, Teintes []string
	T               *Tableau
	Origine         string
	Empreinte       string
	Variables       string // une par ligne : nom | titre | label_values(…)
	Rangees         []vueEdRangee
	Periodes        []string
}

// largeurMaquette : largeur affichée dans l'éditeur (celle du type si 0).
func largeurMaquette(p *Panneau) int {
	if p.Largeur != 0 {
		return p.Largeur
	}
	switch p.Type {
	case PanneauChiffre:
		return 3
	case PanneauTableau:
		return 12
	}
	return 6
}

func (c *Console) pageModifierTableau(q *req) {
	t := c.tableau(q.r.PathValue("nom"))
	if t == nil {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Tableau introuvable", "tableaux", nil)
		return
	}
	d := donneesEdTableau{T: t, Origine: c.origineTableau(t.Nom), Empreinte: empreinteTableau(t), Periodes: periodesProposees,
		Icones: iconesPanneaux, Teintes: teintesPanneaux}
	var vs []string
	for _, v := range t.Variables {
		vs = append(vs, v.Nom+" | "+v.Titre+" | "+v.Requete)
	}
	d.Variables = strings.Join(vs, "\n")
	for ri := range t.Rangees {
		r := &t.Rangees[ri]
		vr := vueEdRangee{Index: ri, R: r}
		for pi := range r.Panneaux {
			p := &r.Panneaux[pi]
			vr.Panneaux = append(vr.Panneaux, vueEdPanneau{ID: strconv.Itoa(ri) + "." + strconv.Itoa(pi), P: p, Largeur: largeurMaquette(p)})
		}
		d.Rangees = append(d.Rangees, vr)
	}
	c.rendre(q, "tableau-modifier", "Modifier « "+t.Titre+" »", "tableaux", d)
}

// copieAJour : copie du tableau en service, si l'empreinte reçue est la
// sienne (sinon il a changé entre-temps).
func (c *Console) copieAJour(q *req) (*Tableau, error) {
	src := c.tableau(q.r.PathValue("nom"))
	if src == nil {
		return nil, errors.New("tableau inconnu")
	}
	if q.r.PostFormValue("empreinte") != empreinteTableau(src) {
		return nil, errors.New("le tableau a changé entre-temps (autre onglet ?) : la page a été rechargée, refaire la modification")
	}
	return copierTableau(src)
}

// lirePosition : « ri.pi » valide dans t.
func lirePosition(t *Tableau, s string) (int, int, error) {
	a, b, ok := strings.Cut(s, ".")
	ri, e1 := strconv.Atoi(a)
	pi, e2 := strconv.Atoi(b)
	if !ok || e1 != nil || e2 != nil || ri < 0 || ri >= len(t.Rangees) || pi < 0 || pi >= len(t.Rangees[ri].Panneaux) {
		return 0, 0, errors.New("panneau inconnu")
	}
	return ri, pi, nil
}

func lireRangee(t *Tableau, s string) (int, error) {
	ri, err := strconv.Atoi(s)
	if err != nil || ri < 0 || ri >= len(t.Rangees) {
		return 0, errors.New("rangée inconnue")
	}
	return ri, nil
}

// appliquerOrdre : « 1:1.0,0.1|0:0.0 » = rangées dans le nouvel ordre
// (numéro d'origine avant « : »), puis leurs panneaux (positions d'origine).
// Chaque rangée et chaque panneau doivent apparaître une fois et une seule.
func appliquerOrdre(t *Tableau, ordre string) error {
	vuR := map[int]bool{}
	vuP := map[string]bool{}
	var nouvelles []Rangee
	total := 0
	for _, r := range t.Rangees {
		total += len(r.Panneaux)
	}
	for _, bloc := range strings.Split(ordre, "|") {
		tete, liste, ok := strings.Cut(bloc, ":")
		ri, err := lireRangee(t, tete)
		if !ok || err != nil || vuR[ri] {
			return errors.New("ordre illisible (rangée)")
		}
		vuR[ri] = true
		r := t.Rangees[ri]
		r.Panneaux = nil
		if liste != "" {
			for _, id := range strings.Split(liste, ",") {
				a, b, err := lirePosition(t, id)
				if err != nil || vuP[id] {
					return errors.New("ordre illisible (panneau)")
				}
				vuP[id] = true
				r.Panneaux = append(r.Panneaux, t.Rangees[a].Panneaux[b])
			}
		}
		nouvelles = append(nouvelles, r)
	}
	if len(vuR) != len(t.Rangees) || len(vuP) != total {
		return errors.New("ordre incomplet : recharger la page")
	}
	t.Rangees = nouvelles
	return nil
}

// lireVariables : « nom | titre | label_values(…) » par ligne.
func lireVariables(s string) ([]Variable, error) {
	var out []Variable
	for _, l := range strings.Split(s, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		p := strings.SplitN(l, "|", 3)
		if len(p) != 3 {
			return nil, fmt.Errorf("filtre %q : « nom | titre | label_values(SÉLECTEUR, ÉTIQUETTE) » attendu", l)
		}
		out = append(out, Variable{Nom: strings.TrimSpace(p[0]), Titre: strings.TrimSpace(p[1]), Requete: strings.TrimSpace(p[2])})
	}
	if len(out) > 10 {
		return nil, errors.New("10 filtres au plus")
	}
	return out, nil
}

func (c *Console) postModifierTableau(q *req) {
	f := champs{q}
	nom := q.r.PathValue("nom")
	ici := "/tableaux/" + nom + "/modifier"
	t, err := c.copieAJour(q)
	if err != nil {
		c.retour(q, ici, err, "")
		return
	}
	ok, err := func() (string, error) {
		switch f.texte("action") {
		case "proprietes":
			if err := verifierTitre(f.texte("titre")); err != nil {
				return "", err
			}
			vs, err := lireVariables(q.r.PostFormValue("variables"))
			if err != nil {
				return "", err
			}
			t.Titre, t.Description, t.Periode, t.Variables = f.texte("titre"), f.texte("description"), f.texte("periode"), vs
			t.Icone, t.Teinte = f.texte("icone"), f.texte("teinte")
			return "propriétés enregistrées", nil
		case "rangee-ajouter":
			titre := f.texte("titre")
			if titre == "" {
				titre = "Nouvelle rangée"
			}
			if len(t.Rangees) >= 30 {
				return "", errors.New("30 rangées au plus")
			}
			t.Rangees = append(t.Rangees, Rangee{Titre: titre})
			return "rangée ajoutée", nil
		case "rangee-modifier":
			ri, err := lireRangee(t, f.texte("rangee"))
			if err != nil {
				return "", err
			}
			t.Rangees[ri].Titre, t.Rangees[ri].Enroulee = f.texte("titre"), f.coche("enroulee")
			return "rangée enregistrée", nil
		case "rangee-supprimer":
			ri, err := lireRangee(t, f.texte("rangee"))
			if err != nil {
				return "", err
			}
			t.Rangees = append(t.Rangees[:ri], t.Rangees[ri+1:]...)
			return "rangée supprimée (avec ses panneaux)", nil
		case "panneau-supprimer":
			ri, pi, err := lirePosition(t, f.texte("panneau"))
			if err != nil {
				return "", err
			}
			ps := t.Rangees[ri].Panneaux
			t.Rangees[ri].Panneaux = append(ps[:pi], ps[pi+1:]...)
			return "panneau supprimé", nil
		case "panneau-dupliquer":
			ri, pi, err := lirePosition(t, f.texte("panneau"))
			if err != nil {
				return "", err
			}
			p := t.Rangees[ri].Panneaux[pi]
			b, _ := json.Marshal(p)
			var copie Panneau
			_ = json.Unmarshal(b, &copie)
			copie.Titre += " (copie)"
			ps := t.Rangees[ri].Panneaux
			t.Rangees[ri].Panneaux = append(ps[:pi+1], append([]Panneau{copie}, ps[pi+1:]...)...)
			return "panneau dupliqué", nil
		case "largeur":
			ri, pi, err := lirePosition(t, f.texte("panneau"))
			if err != nil {
				return "", err
			}
			l, err := strconv.Atoi(f.texte("largeur"))
			if err != nil {
				return "", errors.New("largeur inconnue")
			}
			t.Rangees[ri].Panneaux[pi].Largeur = l
			return "largeur enregistrée", nil
		case "ordre":
			return "disposition enregistrée", appliquerOrdre(t, f.texte("ordre"))
		}
		return "", errors.New("action inconnue")
	}()
	if err == nil {
		err = c.EnregistrerTableau(q.r.Context(), t, "tableau_modifier", q.qui(), q.ip.String(), q.t)
	}
	c.retour(q, ici, err, ok)
}

// --- Un panneau : formulaire et aperçu ------------------------------------------------

type ligneRequete struct {
	ID, Expr, Legende string
}

type uniteProposee struct{ Code, Nom string }

var unitesProposees = []uniteProposee{
	{"", "Nombre"}, {"short", "Nombre (k, M, G)"}, {"none", "Nombre brut"}, {"percent", "Pourcentage (0 à 100)"}, {"percentunit", "Pourcentage (0 à 1)"},
	{"bytes", "Octets"}, {"Bps", "Octets par seconde"}, {"bps", "Bits par seconde"}, {"pps", "Paquets par seconde"},
	{"ops", "Opérations par seconde"}, {"s", "Durée (secondes)"}, {"ms", "Millisecondes"}, {"d", "Jours"},
	{"celsius", "Degrés Celsius"}, {"lignes/min", "Lignes par minute"}, {"watt", "Watts"}, {"dBm", "dBm"}, {"dateTimeAsIso", "Date (secondes ou millisecondes Unix)"},
}

type largeurProposee struct {
	Valeur int
	Nom    string
}

var largeursProposees = []largeurProposee{{0, "Automatique (selon le type)"}, {3, "¼ de la largeur"}, {4, "⅓"}, {6, "½"},
	{8, "⅔"}, {9, "¾"}, {12, "Toute la largeur"}}

type donneesPanneau struct {
	T               *Tableau
	Empreinte       string
	Position        string // « ri.pi », ou « ri.nouveau »
	Nouveau         bool
	P               Panneau
	Requetes        []ligneRequete
	Seuils          string
	Correspondances string
	Colonnes        string
	Masquer         string
	Decimales       string
	Unites          []uniteProposee
	Largeurs        []largeurProposee
	Icones, Teintes []string
	Apercu          *vuePanneau
	Erreur          string
	// Aide à l'écriture des requêtes.
	Modeles      []groupeModeles
	Metriques    []metriqueVue
	Calculs      []struct{ Code, Nom string }
	Combinaisons []struct{ Code, Nom string }
	FiltresTab   []string // filtres du tableau (pour l'assistant)
}

type groupeModeles struct {
	Categorie string
	Modeles   []ModeleRequete
}

// aideRequetes : listes de l'aide (modèles, métriques reçues, choix).
func (c *Console) aideRequetes(q *req, d *donneesPanneau) {
	for _, cat := range categoriesModeles() {
		g := groupeModeles{Categorie: cat}
		for _, m := range modelesRequetes {
			if m.Categorie == cat {
				g.Modeles = append(g.Modeles, m)
			}
		}
		d.Modeles = append(d.Modeles, g)
	}
	d.Metriques = c.metriquesRecues(q.r.Context(), q.t)
	d.Calculs, d.Combinaisons = calculsAssistant, combinaisonsAssistant
	if d.T != nil {
		d.FiltresTab = filtresTableau(d.T)
	}
}

// textes des champs « lignes » à partir d'un panneau.
func (d *donneesPanneau) remplir(p Panneau) {
	d.P = p
	for _, r := range p.Requetes {
		d.Requetes = append(d.Requetes, ligneRequete{r.ID, r.Expr, r.Legende})
	}
	for len(d.Requetes) < len(p.Requetes)+2 && len(d.Requetes) < 12 {
		d.Requetes = append(d.Requetes, ligneRequete{})
	}
	var ls []string
	for _, s := range p.Seuils {
		if s.Valeur == nil {
			ls = append(ls, s.Couleur)
		} else {
			ls = append(ls, strconv.FormatFloat(*s.Valeur, 'g', -1, 64)+" "+s.Couleur)
		}
	}
	d.Seuils = strings.Join(ls, "\n")
	ls = nil
	for _, c := range p.Correspondances {
		cle := c.Valeur
		if cle == "" {
			if c.De != nil {
				cle = strconv.FormatFloat(*c.De, 'g', -1, 64)
			}
			cle += ".."
			if c.A != nil {
				cle += strconv.FormatFloat(*c.A, 'g', -1, 64)
			}
		}
		ls = append(ls, strings.TrimSpace(cle+" = "+c.Texte+" "+c.Couleur))
	}
	d.Correspondances = strings.Join(ls, "\n")
	ls = nil
	for _, c := range p.Colonnes {
		ls = append(ls, strings.TrimRight(c.Champ+" | "+c.Titre+" | "+c.Unite, " |"))
	}
	d.Colonnes = strings.Join(ls, "\n")
	d.Masquer = strings.Join(p.Masquer, ", ")
	if p.Decimales != nil {
		d.Decimales = strconv.Itoa(*p.Decimales)
	}
}

func (c *Console) pagePanneau(q *req) {
	t := c.tableau(q.r.PathValue("nom"))
	if t == nil {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Tableau introuvable", "tableaux", nil)
		return
	}
	d := donneesPanneau{T: t, Empreinte: empreinteTableau(t), Unites: unitesProposees, Largeurs: largeursProposees,
		Icones: iconesPanneaux, Teintes: teintesPanneaux}
	pos := q.r.URL.Query().Get("id")
	if ri, pi, err := lirePosition(t, pos); err == nil {
		d.Position = pos
		d.remplir(t.Rangees[ri].Panneaux[pi])
	} else if ri, err := lireRangee(t, q.r.URL.Query().Get("rangee")); err == nil {
		d.Position, d.Nouveau = strconv.Itoa(ri)+".nouveau", true
		d.remplir(Panneau{Type: PanneauCourbe, Titre: "Nouveau panneau", Requetes: []Requete{{ID: "A"}}})
	} else {
		c.retour(q, "/tableaux/"+t.Nom+"/modifier", errors.New("panneau inconnu"), "")
		return
	}
	c.aideRequetes(q, &d)
	c.rendre(q, "panneau-modifier", "Panneau", "tableaux", d)
}

var couleursSeuils = map[string]bool{"ok": true, "attention": true, "erreur": true, "info": true}

// panneauDuFormulaire : le panneau tel que saisi (base = l'ancien, pour
// garder ce que le formulaire ne montre pas : seuils des colonnes…).
func panneauDuFormulaire(f champs, base Panneau) (Panneau, error) {
	p := base
	var errs []error
	p.Type, p.Titre, p.Description, p.Unite = f.texte("type"), f.texte("titre"), f.texte("description"), f.texte("unite")
	if err := verifierTitre(p.Titre); err != nil {
		errs = append(errs, err)
	}
	p.Decimales = nil
	if s := f.texte("decimales"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || n > 6 {
			errs = append(errs, errors.New("décimales : 0 à 6, ou vide (automatique)"))
		} else {
			p.Decimales = &n
		}
	}
	if n, err := strconv.Atoi(f.texte("largeur")); err == nil {
		p.Largeur = n
	}
	p.Icone, p.Teinte = f.texte("icone"), f.texte("teinte")
	// Requêtes : lignes non vides ; identifiant gardé (colonnes d'un tableau).
	p.Requetes = nil
	utilises := map[string]bool{}
	for i := 0; i < 12; i++ {
		expr := strings.TrimSpace(f.q.r.PostFormValue("expr" + strconv.Itoa(i)))
		if expr == "" {
			continue
		}
		id := f.texte("id" + strconv.Itoa(i))
		if id == "" || utilises[id] || len(id) > 8 {
			for _, x := range "ABCDEFGHIJKLMNOP" {
				if !utilises[string(x)] {
					id = string(x)
					break
				}
			}
		}
		utilises[id] = true
		p.Requetes = append(p.Requetes, Requete{ID: id, Expr: expr, Legende: f.texte("legende" + strconv.Itoa(i))})
	}
	// Seuils et correspondances : lignes de texte (voir lireSeuils).
	var e1, e2 []error
	p.Seuils, e1 = lireSeuils(f.q.r.PostFormValue("seuils"))
	p.Correspondances, e2 = lireCorrespondances(f.q.r.PostFormValue("correspondances"))
	errs = append(append(errs, e1...), e2...)
	// Colonnes (tableau) : « champ | titre | unité » ; le reste de chaque
	// colonne (seuils, correspondances) est gardé si le champ existait.
	anciennes := map[string]Colonne{}
	for _, c := range base.Colonnes {
		anciennes[c.Champ] = c
	}
	p.Colonnes = nil
	for _, l := range strings.Split(f.q.r.PostFormValue("colonnes"), "\n") {
		if strings.TrimSpace(l) == "" {
			continue
		}
		parts := strings.Split(l, "|")
		col := anciennes[strings.TrimSpace(parts[0])]
		col.Champ = strings.TrimSpace(parts[0])
		col.Titre = col.Champ
		if len(parts) > 1 && strings.TrimSpace(parts[1]) != "" {
			col.Titre = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			col.Unite = strings.TrimSpace(parts[2])
		}
		p.Colonnes = append(p.Colonnes, col)
	}
	p.Masquer = f.liste("masquer")
	return p, errors.Join(errs...)
}

func (c *Console) postPanneau(q *req) {
	f := champs{q}
	nom := q.r.PathValue("nom")
	t, err := c.copieAJour(q)
	if err != nil {
		c.retour(q, "/tableaux/"+nom+"/modifier", err, "")
		return
	}
	pos := f.texte("position")
	var base Panneau
	ri, pi, errPos := lirePosition(t, pos)
	nouveau := false
	if errPos != nil {
		a, b, _ := strings.Cut(pos, ".")
		if ri, err = lireRangee(t, a); err != nil || b != "nouveau" {
			c.retour(q, "/tableaux/"+nom+"/modifier", errors.New("panneau inconnu"), "")
			return
		}
		nouveau = true
	} else {
		base = t.Rangees[ri].Panneaux[pi]
	}
	p, err := panneauDuFormulaire(f, base)
	if f.texte("action") == "modele" {
		// Requête toute faite : le formulaire peut être incomplet, on
		// insère puis on montre l'aperçu (rien n'est enregistré).
		p, err = appliquerModele(t, p, f.texte("modele"), f.coche("remplacer") || nouveau && len(base.Requetes) == 0 && p.Titre == "Nouveau panneau")
		q.r.PostForm.Set("action", "apercu")
	}
	if err == nil {
		// Vérification complète : le panneau seul dans le tableau.
		essai := *t
		essai.Rangees = []Rangee{{Titre: "essai", Panneaux: []Panneau{p}}}
		err = essai.Valider()
	}
	if err != nil || f.texte("action") == "apercu" {
		d := donneesPanneau{T: c.tableau(nom), Empreinte: q.r.PostFormValue("empreinte"), Position: pos, Nouveau: nouveau,
			Unites: unitesProposees, Largeurs: largeursProposees,
			Icones: iconesPanneaux, Teintes: teintesPanneaux}
		c.aideRequetes(q, &d)
		d.remplir(p)
		if err != nil {
			d.Erreur = err.Error()
		} else {
			d.Apercu = c.apercuPanneau(q, t, &p)
		}
		c.rendre(q, "panneau-modifier", "Panneau", "tableaux", d)
		return
	}
	if nouveau {
		if n := len(t.Rangees[ri].Panneaux); n >= 40 {
			c.retour(q, "/tableaux/"+nom+"/modifier", errors.New("40 panneaux par rangée au plus"), "")
			return
		}
		t.Rangees[ri].Panneaux = append(t.Rangees[ri].Panneaux, p)
	} else {
		t.Rangees[ri].Panneaux[pi] = p
	}
	err = c.EnregistrerTableau(q.r.Context(), t, "tableau_modifier", q.qui(), q.ip.String(), q.t)
	c.retour(q, "/tableaux/"+nom+"/modifier", err, "panneau « "+p.Titre+" » enregistré")
}

// lireSeuils : « couleur » (à partir de tout en bas) ou « valeur couleur ».
func lireSeuils(texte string) (out []Seuil, errs []error) {
	for _, l := range strings.Split(texte, "\n") {
		champs := strings.Fields(l)
		switch len(champs) {
		case 0:
			continue
		case 1:
			if couleursSeuils[champs[0]] {
				out = append(out, Seuil{Couleur: champs[0]})
				continue
			}
		case 2:
			v, err := strconv.ParseFloat(strings.Replace(champs[0], ",", ".", 1), 64)
			if err == nil && couleursSeuils[champs[1]] {
				out = append(out, Seuil{Valeur: &v, Couleur: champs[1]})
				continue
			}
		}
		errs = append(errs, fmt.Errorf("seuil %q : « couleur » ou « valeur couleur » (ok, attention, erreur, info)", strings.TrimSpace(l)))
	}
	return out, errs
}

// lireCorrespondances : « 1 = OK ok », « 10..20 = Moyen attention ».
func lireCorrespondances(texte string) (out []Correspondance, errs []error) {
	for _, l := range strings.Split(texte, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		cle, txt, ok := strings.Cut(l, "=")
		cle, txt = strings.TrimSpace(cle), strings.TrimSpace(txt)
		if !ok || txt == "" {
			errs = append(errs, fmt.Errorf("correspondance %q : « valeur = texte [couleur] » attendu", l))
			continue
		}
		var cr Correspondance
		if mots := strings.Fields(txt); len(mots) > 1 && couleursSeuils[mots[len(mots)-1]] {
			cr.Couleur = mots[len(mots)-1]
			txt = strings.TrimSpace(strings.TrimSuffix(txt, cr.Couleur))
		}
		cr.Texte = txt
		if de, a, plage := strings.Cut(cle, ".."); plage {
			lire := func(s string) (*float64, error) {
				if strings.TrimSpace(s) == "" {
					return nil, nil
				}
				v, err := strconv.ParseFloat(strings.Replace(strings.TrimSpace(s), ",", ".", 1), 64)
				return &v, err
			}
			var e1, e2 error
			cr.De, e1 = lire(de)
			cr.A, e2 = lire(a)
			if e1 != nil || e2 != nil || (cr.De == nil && cr.A == nil) {
				errs = append(errs, fmt.Errorf("correspondance %q : plage « de..à » illisible", l))
				continue
			}
		} else {
			if _, err := strconv.ParseFloat(cle, 64); err != nil {
				errs = append(errs, fmt.Errorf("correspondance %q : valeur numérique attendue", l))
				continue
			}
			cr.Valeur = cle
		}
		out = append(out, cr)
	}
	return out, errs
}

// idsRequetes : identifiants des requêtes d'un panneau (12 au plus).
const idsRequetes = "ABCDEFGHIJKL"

// appliquerModele : requête toute faite insérée dans le panneau (première
// ligne vide, sinon ajoutée) ; remplacer = titre, type, unité, couleurs
// du modèle aussi.
func appliquerModele(t *Tableau, p Panneau, id string, remplacer bool) (Panneau, error) {
	m, ok := modeleParID(id)
	if !ok {
		return p, errors.New("choisir une requête toute faite")
	}
	expr := appliquerFiltres(m.Expr, filtresTableau(t))
	place := -1
	for i, r := range p.Requetes {
		if strings.TrimSpace(r.Expr) == "" {
			place = i
			break
		}
	}
	if remplacer {
		place = 0
		p.Requetes = nil
	}
	r := Requete{Expr: expr, Legende: m.Legende}
	switch {
	case place >= 0 && place < len(p.Requetes):
		r.ID = p.Requetes[place].ID
		p.Requetes[place] = r
	case len(p.Requetes) >= 12:
		return p, errors.New("12 requêtes au plus dans un panneau")
	default:
		r.ID = idsRequetes[len(p.Requetes) : len(p.Requetes)+1] // moins de 12 : vérifié juste au-dessus
		p.Requetes = append(p.Requetes, r)
	}
	if r.ID == "" {
		r.ID = "A"
		p.Requetes[0].ID = "A"
	}
	if remplacer {
		p.Type, p.Titre, p.Description, p.Unite = m.Type, m.Titre, m.Description, m.Unite
		p.Icone, p.Teinte = m.Icone, m.Teinte
		p.Seuils, _ = lireSeuils(m.Seuils)
		p.Correspondances, _ = lireCorrespondances(m.Correspondances)
		p.Colonnes, p.Masquer, p.Decimales = nil, nil, nil
	}
	return p, nil
}

// apercuPanneau : le panneau calculé avec les filtres vides (toutes les
// machines) sur la période par défaut du tableau.
func (c *Console) apercuPanneau(q *req, t *Tableau, p *Panneau) *vuePanneau {
	vals := map[string]string{}
	for _, v := range t.Variables {
		vals[v.Nom] = ""
	}
	d, err := lirePeriode(t.Periode)
	if err != nil {
		d = 6 * time.Hour
	}
	vp := &vuePanneau{P: p}
	if p.Type == PanneauCourbe {
		b, _ := json.Marshal(c.courbe(q.r.Context(), p.Requetes, p.Unite, vals, d, q.t))
		vp.Courbe, vp.CourbeJSON = "apercu", string(b)
		return vp
	}
	if err := c.remplirPanneau(q.r.Context(), vp, vals, pasPour(d), d, q.t); err != nil {
		vp.Erreur = err.Error()
	}
	return vp
}
