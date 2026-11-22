// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"

	"mirador-stack/internal/server/auth"
)

// Menu réorganisable (SPEC § 79) : chaque compte peut changer l'ordre des
// entrées (dans leur groupe) et en masquer, avec un menu pour l'ordinateur
// et un autre pour le téléphone, et choisir les 4 raccourcis de la barre du
// bas du téléphone. Les deux ordres sont rendus dans la page (classes
// o-N / t-N, CSS `order` selon la largeur) : pas de script nécessaire.

// entreeMenu : une entrée possible du menu.
type entreeMenu struct {
	Cle, Href, Icone, Libelle string
	Groupe                    int  // 0 Supervision, 1 Configuration
	Admin                     bool // seulement pour un admin
	Fixe                      bool // ne peut pas être masquée (pour ne jamais se bloquer)
	// Court : libellé de la barre du bas du téléphone, si le libellé est
	// trop long pour y tenir sur une ligne (retour du 20/11).
	Court string
}

var groupesMenu = []string{"Supervision", "Configuration"}

// entreesMenu : ordre par défaut.
var entreesMenu = []entreeMenu{
	{Cle: "accueil", Href: "/", Icone: "ensemble", Libelle: "Vue d'ensemble", Court: "Accueil"},
	{Cle: "machines", Href: "/machines", Icone: "machines", Libelle: "Machines"},
	{Cle: "conteneurs", Href: "/conteneurs", Icone: "conteneur", Libelle: "Conteneurs"},
	{Cle: "taches", Href: "/taches", Icone: "calendrier", Libelle: "Tâches planifiées", Court: "Tâches"},
	{Cle: "services", Href: "/applications", Icone: "activite", Libelle: "Applications"},
	{Cle: "elements", Href: "/elements", Icone: "base", Libelle: "Éléments"},
	{Cle: "sondes", Href: "/sondes", Icone: "globe", Libelle: "Sondes"},
	{Cle: "alertes", Href: "/alertes", Icone: "alertes", Libelle: "Alertes"},
	{Cle: "logs", Href: "/logs", Icone: "logs", Libelle: "Logs"},
	{Cle: "tableaux", Href: "/tableaux", Icone: "tableaux", Libelle: "Tableaux"},
	// Santé de mirador (SPEC § 121) : en bas de Supervision.
	{Cle: "sante", Href: "/sante-mirador", Icone: "jauge", Libelle: "Santé de mirador", Court: "Santé"},
	{Cle: "regles", Href: "/regles", Icone: "regles", Libelle: "Règles", Groupe: 1},
	{Cle: "reglages", Href: "/reglages", Icone: "reglages", Libelle: "Réglages", Groupe: 1, Fixe: true},
	{Cle: "utilisateurs", Href: "/utilisateurs", Icone: "utilisateurs", Libelle: "Utilisateurs", Groupe: 1, Admin: true},
	{Cle: "securite", Href: "/securite", Icone: "securite", Libelle: "Sécurité", Groupe: 1, Admin: true, Fixe: true},
	{Cle: "audit", Href: "/audit", Icone: "audit", Libelle: "Audit", Groupe: 1},
}

// basDefaut : raccourcis de la barre du bas du téléphone.
var basDefaut = []string{"accueil", "alertes", "machines", "logs"}

func entreeParCle(cle string) (entreeMenu, bool) {
	for _, e := range entreesMenu {
		if e.Cle == cle {
			return e, true
		}
	}
	return entreeMenu{}, false
}

// prefMode : menu d'un mode (ordinateur ou téléphone).
type prefMode struct {
	Ordre  []string `json:"ordre,omitempty"`  // clés dans l'ordre voulu (les absentes suivent, ordre par défaut)
	Caches []string `json:"caches,omitempty"` // clés masquées
	Bas    []string `json:"bas,omitempty"`    // téléphone : raccourcis de la barre du bas (1 à 4)
}

// prefMenu : document gardé dans utilisateurs.menu.
type prefMenu struct {
	Ordi prefMode `json:"ordi"`
	Tel  prefMode `json:"tel"`
	// Accueil : disposition de la vue d'ensemble (SPEC § 83).
	Accueil prefAccueil `json:"accueil,omitempty"`
}

func lirePrefMenu(s string) prefMenu {
	var p prefMenu
	if s != "" {
		_ = json.Unmarshal([]byte(s), &p) // illisible : menu par défaut
	}
	return p
}

// rangs : rang de chaque clé (ordre voulu, puis les autres dans l'ordre par défaut).
func (m prefMode) rangs() map[string]int {
	r := map[string]int{}
	n := 0
	for _, c := range m.Ordre {
		if _, ok := entreeParCle(c); ok {
			if _, deja := r[c]; !deja {
				r[c] = n
				n++
			}
		}
	}
	for _, e := range entreesMenu {
		if _, deja := r[e.Cle]; !deja {
			r[e.Cle] = n
			n++
		}
	}
	return r
}

func (m prefMode) cache(cle string) bool {
	for _, c := range m.Caches {
		if c == cle {
			return true
		}
	}
	return false
}

// elementMenu : ce que le gabarit affiche (lien, ou titre de groupe).
type elementMenu struct {
	Titre  string // titre de groupe (sinon : lien)
	Lien   lienMenu
	Classe string // o-N t-N, cache-o, cache-t, fixe
	Cle    string
}

// construireMenu : entrées du menu de gauche (les deux ordres) et barre du bas.
func construireMenu(p *Page, u auth.Utilisateur) {
	pref := lirePrefMenu(u.Menu)
	ro, rt := pref.Ordi.rangs(), pref.Tel.rangs()
	// Rang dans le groupe → classe d'ordre : groupe 0 de 1 à 29, groupe 1 de 31 à 59.
	for g, titre := range groupesMenu {
		p.MenuElements = append(p.MenuElements, elementMenu{Titre: titre, Classe: fmtOrdre(g*30, g*30)})
		var dansGroupe []entreeMenu
		for _, e := range entreesMenu {
			if e.Groupe == g && (!e.Admin || p.Admin) {
				dansGroupe = append(dansGroupe, e)
			}
		}
		po, pt := positions(dansGroupe, ro), positions(dansGroupe, rt)
		for _, e := range dansGroupe {
			cl := fmtOrdre(g*30+1+po[e.Cle], g*30+1+pt[e.Cle])
			if !e.Fixe && pref.Ordi.cache(e.Cle) {
				cl += " cache-o"
			}
			if !e.Fixe && pref.Tel.cache(e.Cle) {
				cl += " cache-t"
			}
			if e.Fixe {
				cl += " fixe"
			}
			p.MenuElements = append(p.MenuElements, elementMenu{Lien: p.lienDe(e), Classe: cl, Cle: e.Cle})
		}
	}
	bas := pref.Tel.Bas
	if len(bas) == 0 {
		bas = basDefaut
	}
	for _, c := range bas {
		if e, ok := entreeParCle(c); ok && (!e.Admin || p.Admin) {
			p.MenuBas = append(p.MenuBas, p.lienDe(e))
		}
	}
	p.MenuBasCles = bas
}

// positions : rang de chaque entrée à l'intérieur de son groupe.
func positions(es []entreeMenu, rangs map[string]int) map[string]int {
	tri := append([]entreeMenu(nil), es...)
	for i := 1; i < len(tri); i++ { // tri par insertion : 15 entrées au plus
		for j := i; j > 0 && rangs[tri[j].Cle] < rangs[tri[j-1].Cle]; j-- {
			tri[j], tri[j-1] = tri[j-1], tri[j]
		}
	}
	out := map[string]int{}
	for i, e := range tri {
		out[e.Cle] = i
	}
	return out
}

func fmtOrdre(o, t int) string { return "o-" + strconv.Itoa(o) + " t-" + strconv.Itoa(t) }

// lienDe : lien du menu avec son compteur.
func (p *Page) lienDe(e entreeMenu) lienMenu {
	l := lienMenu{Href: e.Href, Icone: e.Icone, Libelle: e.Libelle, Court: e.Libelle, Actif: p.Menu == e.Cle}
	if e.Court != "" {
		l.Court = e.Court
	}
	switch e.Cle {
	case "machines":
		l.Compteur, l.ClasseCompteur, l.AideCompteur = p.NbAValider, "attention", "Machines où l'agent a trouvé quelque chose de nouveau à surveiller"
	case "conteneurs":
		l.Compteur, l.ClasseCompteur, l.AideCompteur = p.NbConteneurs, "attention", "Conteneurs non sains ou arrêtés alors qu'ils devraient redémarrer"
	case "sondes":
		l.Compteur = p.NbSondes
	case "taches":
		l.Compteur, l.AideCompteur = p.NbTaches, "Tâches suivies en échec ou en retard"
	case "alertes":
		l.Compteur = p.NbAlertes
	case "sante":
		l.Compteur, l.ClasseCompteur, l.AideCompteur = p.NbSante, "attention", "Services de mirador à regarder"
	}
	return l
}

// demandeMenu : envoi du mode édition (mirador.js).
type demandeMenu struct {
	Mode   string   `json:"mode"` // ordi ou tel
	Defaut bool     `json:"defaut"`
	Ordre  []string `json:"ordre"`
	Caches []string `json:"caches"`
	Bas    []string `json:"bas"`
}

// verifierMenu : clés connues, sans doublon ; entrées fixes jamais masquées ;
// barre du bas : 1 à 4 raccourcis.
func verifierMenu(d demandeMenu) (prefMode, error) {
	var m prefMode
	vus := map[string]bool{}
	for _, c := range d.Ordre {
		if _, ok := entreeParCle(c); !ok || vus[c] {
			return m, errors.New("entrée de menu inconnue ou en double")
		}
		vus[c] = true
		m.Ordre = append(m.Ordre, c)
	}
	for _, c := range d.Caches {
		e, ok := entreeParCle(c)
		if !ok {
			return m, errors.New("entrée de menu inconnue")
		}
		if e.Fixe {
			return m, errors.New(e.Libelle + " ne peut pas être masqué")
		}
		m.Caches = append(m.Caches, c)
	}
	if d.Mode == "tel" && d.Bas != nil {
		if len(d.Bas) < 1 || len(d.Bas) > 4 {
			return m, errors.New("barre du bas : 1 à 4 raccourcis")
		}
		vb := map[string]bool{}
		for _, c := range d.Bas {
			if _, ok := entreeParCle(c); !ok || vb[c] {
				return m, errors.New("raccourci inconnu ou en double")
			}
			vb[c] = true
			m.Bas = append(m.Bas, c)
		}
	}
	return m, nil
}

// postMenu : enregistre le menu d'un mode (JSON envoyé par « Terminé »).
func (c *Console) postMenu(q *req) {
	var d demandeMenu
	if err := json.NewDecoder(io.LimitReader(q.r.Body, 8192)).Decode(&d); err != nil || (d.Mode != "ordi" && d.Mode != "tel") {
		http.Error(q.w, "demande illisible", http.StatusBadRequest)
		return
	}
	pref := lirePrefMenu(q.u.Menu)
	var m prefMode
	if !d.Defaut {
		var err error
		if m, err = verifierMenu(d); err != nil {
			http.Error(q.w, err.Error(), http.StatusBadRequest)
			return
		}
	}
	if d.Mode == "ordi" {
		pref.Ordi = m
	} else {
		pref.Tel = m
	}
	b, _ := json.Marshal(pref)
	texte := string(b)
	if pref.Ordi.vide() && pref.Tel.vide() && pref.Accueil.Ordi.vide() && pref.Accueil.Tel.vide() {
		texte = ""
	}
	if err := auth.DefinirMenu(q.r.Context(), c.db, q.u.ID, texte); err != nil {
		http.Error(q.w, err.Error(), http.StatusBadRequest)
		return
	}
	c.oublierComptes()
	q.w.WriteHeader(http.StatusNoContent)
}

func (m prefMode) vide() bool { return len(m.Ordre) == 0 && len(m.Caches) == 0 && len(m.Bas) == 0 }
