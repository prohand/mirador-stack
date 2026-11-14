// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/server/modeles"
)

// Modèles personnels dans l'interface (partie C, SPEC § 24.11) :
//   - « Nouveau modèle » (vide) ou « Dupliquer » un modèle livré ;
//   - formulaire : titre, description, détection, services, seuils, listes,
//     règles (lignes vides en plus pour ajouter) ; configuration des agents
//     en JSON (avancé) ;
//   - export JSON de n'importe quel modèle, import JSON d'un modèle
//     personnel (partage entre serveurs) ;
//   - suppression d'un modèle personnel qui n'est plus attribué.

func (c *Console) routesModelesPerso() {
	c.mux.HandleFunc("GET /modeles/nouveau", c.lecture(c.pageNouveauModele))
	c.mux.HandleFunc("GET /modeles/perso/{nom}", c.lecture(c.pageModelePerso))
	c.mux.HandleFunc("POST /modeles/perso", c.admin(c.postModelePerso))
	c.mux.HandleFunc("GET /modeles/export/{nom}", c.lecture(c.exporterModele))
}

// lignesVides : lignes vides ajoutées à chaque tableau du formulaire.
const lignesVides = 2

// donneesModelePerso : formulaire d'un modèle personnel.
type donneesModelePerso struct {
	M           *modeles.Modele
	Nouveau     bool   // création (le nom est saisi)
	Depuis      string // titre du modèle copié
	Config      string // JSON des clés de configuration des agents
	Services    string
	Vides       []int // lignes vides à ajouter (listes, seuils)
	VidesRegles []int // règles vides à ajouter
	TypesCheck  []string
	Gravites    []string
	Attribue    []string // portées où il est attribué (suppression refusée)
}

// typesCheck : types de checks, les plus courants d'abord.
func typesCheck() []string {
	ordre := []string{"http", "tcp", "processus", "fichier", "dossier", "dns", "ping"}
	out := make([]string, 0, len(config.TypesCheck))
	for _, t := range ordre {
		if config.TypesCheck[t] {
			out = append(out, t)
		}
	}
	for t := range config.TypesCheck { // type ajouté plus tard à l'agent
		if !slices.Contains(out, t) {
			out = append(out, t)
		}
	}
	return out
}

// formulaireModele affiche le formulaire ; configBrute : JSON saisi à
// réafficher tel quel après une erreur (sinon celui du modèle).
func (c *Console) formulaireModele(q *req, m *modeles.Modele, nouveau bool, depuis, configBrute string) {
	d := donneesModelePerso{M: m, Nouveau: nouveau, Depuis: depuis, Services: strings.Join(m.Services, ", "),
		Vides: make([]int, lignesVides), VidesRegles: make([]int, 1), TypesCheck: typesCheck(), Gravites: []string{"critical", "warning", "info"}}
	if configBrute != "" {
		d.Config = configBrute
	} else if len(m.Config) > 0 && string(m.Config) != "null" {
		var joli any
		if json.Unmarshal(m.Config, &joli) == nil {
			if b, err := json.MarshalIndent(joli, "", "  "); err == nil {
				d.Config = string(b)
			}
		}
	}
	if !nouveau {
		d.Attribue, _ = modeles.Attributions(q.r.Context(), c.db, m.Nom)
	}
	titre := "Nouveau modèle"
	if !nouveau {
		titre = "Modèle « " + m.Titre + " »"
	}
	c.rendre(q, "modele-perso", titre, "machines", d)
}

// pageNouveauModele : vide, ou copie d'un modèle (« ?depuis=linux »).
func (c *Console) pageNouveauModele(q *req) {
	m := &modeles.Modele{Version: 1}
	depuis := ""
	if src := modeles.Trouver(q.r.URL.Query().Get("depuis")); src != nil {
		m = modeles.Copie(src, nomLibre(src.Nom+"-perso"), src.Titre+" (personnel)")
		depuis = src.Titre
	}
	c.formulaireModele(q, m, true, depuis, "")
}

// nomLibre : nom de modèle pas encore pris (suffixe -2, -3…).
func nomLibre(base string) string {
	if len(base) > 28 {
		base = strings.TrimRight(base[:28], "-")
	}
	nom := base
	for i := 2; modeles.Trouver(nom) != nil && i < 1000; i++ {
		nom = base + "-" + strconv.Itoa(i)
	}
	return nom
}

func (c *Console) pageModelePerso(q *req) {
	m := modeles.Trouver(q.r.PathValue("nom"))
	if m == nil || !m.Perso {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Introuvable", "machines", nil)
		return
	}
	c.formulaireModele(q, m, false, "", "")
}

// postModelePerso : enregistrer (formulaire), importer (JSON), supprimer.
func (c *Console) postModelePerso(q *req) {
	ctx := q.r.Context()
	if err := q.r.ParseForm(); err != nil {
		c.retour(q, "/modeles", err, "")
		return
	}
	f := q.r.PostFormValue
	if err := c.verifierCode(q); err != nil {
		c.retour(q, "/modeles", err, "")
		return
	}
	switch f("action") {
	case "supprimer":
		nom := f("nom")
		err := modeles.SupprimerPerso(ctx, c.db, nom, q.qui(), q.t)
		if err != nil {
			c.retour(q, "/modeles/perso/"+url.PathEscape(nom), err, "")
			return
		}
		c.oublierAValider()
		c.retour(q, "/modeles", nil, "modèle « "+nom+" » supprimé (et ses règles)")
	case "importer":
		m, err := modeles.DecoderModele([]byte(f("json")))
		if err == nil {
			if ancien := modeles.Trouver(m.Nom); ancien != nil && !ancien.Perso {
				err = errors.New("« " + m.Nom + " » est un modèle livré : changer le « nom » dans le JSON")
			}
		}
		if err == nil {
			err = modeles.EnregistrerPerso(ctx, c.db, m, q.qui(), q.t)
		}
		if err != nil {
			c.retour(q, "/modeles#perso", err, "")
			return
		}
		c.oublierAValider()
		c.retour(q, "/modeles#"+url.PathEscape(m.Nom), nil, "modèle « "+m.Titre+" » importé")
	case "enregistrer":
		nouveau := f("nouveau") == "oui"
		m, err := lireFormulaireModele(q.r.PostForm)
		if err == nil && nouveau && modeles.Trouver(m.Nom) != nil {
			err = errors.New("le nom « " + m.Nom + " » est déjà pris")
		}
		if err == nil && !nouveau {
			if ancien := modeles.Trouver(m.Nom); ancien == nil || !ancien.Perso {
				err = errors.New("modèle personnel inconnu")
			}
		}
		if err == nil {
			err = modeles.EnregistrerPerso(ctx, c.db, m, q.qui(), q.t)
		}
		if err != nil {
			// Saisie gardée : la page est rendue avec le message d'erreur.
			c.flash(q, "", err.Error())
			c.formulaireModele(q, m, nouveau, f("depuis"), f("config"))
			return
		}
		c.oublierAValider()
		c.retour(q, "/modeles#"+url.PathEscape(m.Nom), nil, "modèle « "+m.Titre+" » enregistré : règles à jour, configuration envoyée aux agents qui le portent")
	default:
		c.retour(q, "/modeles", errors.New("action inconnue"), "")
	}
}

// lireFormulaireModele : modèle saisi (lignes sans nom ignorées). Le modèle
// renvoyé garde la saisie même en cas d'erreur (pour la réafficher).
func lireFormulaireModele(v url.Values) (*modeles.Modele, error) {
	g := func(k string) string { return strings.TrimSpace(v.Get(k)) }
	m := &modeles.Modele{Version: 1, Nom: g("nom"), Titre: g("titre"), Description: g("description"), Detection: g("detection")}
	m.Services = strings.FieldsFunc(v.Get("services"), func(r rune) bool {
		return r == ',' || r == ' ' || r == '\n' || r == '\r' || r == '\t'
	})
	var errs []error
	if cfg := g("config"); cfg != "" {
		var brut json.RawMessage
		if err := json.Unmarshal([]byte(cfg), &brut); err != nil {
			errs = append(errs, errors.New("configuration des agents : JSON invalide"))
		} else {
			m.Config = brut
		}
	}
	col := func(k string, i int) string {
		if l := v[k]; i < len(l) {
			return strings.TrimSpace(l[i])
		}
		return ""
	}
	nombre := func(texte, quoi string) float64 {
		x, err := strconv.ParseFloat(strings.ReplaceAll(texte, ",", "."), 64)
		if err != nil {
			errs = append(errs, errors.New(quoi+" : nombre attendu"))
		}
		return x
	}
	for i := range v["p_nom"] {
		if col("p_nom", i) == "" {
			continue
		}
		p := modeles.Parametre{Nom: col("p_nom", i), Titre: col("p_titre", i), Unite: col("p_unite", i)}
		p.Defaut = nombre(col("p_defaut", i), "seuil "+p.Nom+", défaut")
		p.Min = nombre(col("p_min", i), "seuil "+p.Nom+", minimum")
		p.Max = nombre(col("p_max", i), "seuil "+p.Nom+", maximum")
		m.Parametres = append(m.Parametres, p)
	}
	for i := range v["l_nom"] {
		if col("l_nom", i) == "" {
			continue
		}
		m.Listes = append(m.Listes, modeles.ListeParam{Nom: col("l_nom", i), Titre: col("l_titre", i), Type: col("l_type", i),
			Exemple: strings.ReplaceAll(col("l_exemple", i), " | ", "\n"), Aide: col("l_aide", i)})
	}
	for i := range v["r_nom"] {
		if col("r_nom", i) == "" {
			continue
		}
		m.Regles = append(m.Regles, modeles.RegleModele{Nom: col("r_nom", i), Gravite: col("r_gravite", i), Pour: col("r_pour", i),
			Remplace: col("r_remplace", i), Expr: col("r_expr", i), Resume: col("r_resume", i), Description: col("r_description", i)})
	}
	if len(errs) > 0 {
		return m, errors.Join(errs...)
	}
	return m, m.Valider()
}

// exporterModele : JSON d'un modèle (livré ou personnel), à importer sur un
// autre serveur ou à garder.
func (c *Console) exporterModele(q *req) {
	m := modeles.Trouver(q.r.PathValue("nom"))
	if m == nil {
		c.rendreCode(q, http.StatusNotFound, "introuvable", "Introuvable", "machines", nil)
		return
	}
	b, err := json.MarshalIndent(m, "", " ")
	if err != nil {
		http.Error(q.w, err.Error(), http.StatusInternalServerError)
		return
	}
	q.w.Header().Set("Content-Type", "application/json; charset=utf-8")
	q.w.Header().Set("Content-Disposition", `attachment; filename="mirador-modele-`+m.Nom+`.json"`)
	_, _ = q.w.Write(append(b, '\n'))
}
