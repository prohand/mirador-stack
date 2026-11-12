// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"mirador-stack/internal/server/auth"
	"net/http"
	"path"
	"slices"
	"strings"
	"sync/atomic"
	"time"
	"unicode"
	"unicode/utf8"
)

//go:embed gabarits statique
var fichiers embed.FS

// --- Fichiers statiques ------------------------------------------------------------
//
// Embarqués dans le binaire, compressés une fois au démarrage (gzip), servis
// avec une empreinte dans l'adresse (?v=…) : le navigateur les garde un an
// et ne les redemande jamais tant que le binaire ne change pas.

type statique struct {
	brut, gz   []byte
	type_, ver string
}

var statiques = map[string]*statique{}

func init() {
	_ = fs.WalkDir(fichiers, "statique", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		b, err := fichiers.ReadFile(p)
		if err != nil {
			return err
		}
		var gz bytes.Buffer
		zw, _ := gzip.NewWriterLevel(&gz, gzip.BestCompression)
		_, _ = zw.Write(b)
		_ = zw.Close()
		e := sha256.Sum256(b)
		t := mime.TypeByExtension(path.Ext(p))
		if path.Ext(p) == ".woff2" {
			t = "font/woff2"
		}
		if t == "" {
			t = "application/octet-stream"
		}
		statiques["/"+p] = &statique{brut: b, gz: gz.Bytes(), type_: t, ver: hex.EncodeToString(e[:6])}
		return nil
	})
}

// adresseStatique : chemin avec l'empreinte du contenu.
func adresseStatique(nom string) string {
	if s := statiques["/statique/"+nom]; s != nil {
		return "/statique/" + nom + "?v=" + s.ver
	}
	return "/statique/" + nom
}

func servirStatique(w http.ResponseWriter, r *http.Request) {
	s := statiques[r.URL.Path]
	if s == nil || (r.Method != http.MethodGet && r.Method != http.MethodHead) {
		http.NotFound(w, r)
		return
	}
	h := w.Header()
	h.Set("Content-Type", s.type_)
	// Empreinte du contenu : un navigateur qui l'a déjà reçoit « 304 »
	// (polices, liées depuis la feuille de style sans « ?v= »).
	etag := `"` + s.ver + `"`
	h.Set("ETag", etag)
	if r.Header.Get("If-None-Match") == etag {
		w.WriteHeader(http.StatusNotModified)
		return
	}
	if r.URL.Query().Get("v") == s.ver {
		h.Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		h.Set("Cache-Control", "no-cache")
	}
	h.Set("Vary", "Accept-Encoding")
	corps := s.brut
	if strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") && len(s.gz) < len(s.brut) {
		h.Set("Content-Encoding", "gzip")
		corps = s.gz
	}
	h.Set("Content-Length", fmt.Sprint(len(corps)))
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write(corps)
}

// --- Gabarits ----------------------------------------------------------------------

type gabarits struct {
	pages map[string]*template.Template
}

// fuseau : fuseau d'affichage des dates (réglage « Interface »).
var fuseau atomic.Pointer[time.Location]

func init() {
	if l, err := time.LoadLocation("Europe/Paris"); err == nil {
		fuseau.Store(l)
	} else {
		fuseau.Store(time.UTC)
	}
}

func fonctions() template.FuncMap {
	return template.FuncMap{
		"statique": adresseStatique,
		// dateS / dateMs : horodatage Unix (secondes / millisecondes) → texte.
		"dateS": func(s int64) string {
			if s <= 0 {
				return "—"
			}
			return time.Unix(s, 0).In(fuseau.Load()).Format("02/01/2006 15:04:05")
		},
		"dateMs": func(ms int64) string {
			if ms <= 0 {
				return "—"
			}
			return time.UnixMilli(ms).In(fuseau.Load()).Format("02/01/2006 15:04:05")
		},
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(fuseau.Load()).Format("02/01/2006 15:04:05")
		},
		"depuisS":   func(s int64) string { return depuis(s * 1000) },
		"depuisMs":  depuis,
		"duree":     func(d time.Duration) string { return dureeLisible(d) },
		"taille":    tailleLisible,
		"pct":       func(v float64) string { return fmt.Sprintf("%.1f %%", v) },
		"egal":      func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) },
		"sub":       func(a, b int) int { return a - b },
		"liste":     func(v ...string) []string { return v },
		"dansListe": func(s string, l []string) bool { return slices.Contains(l, s) },
		// choixIcone : données du morceau « choix-icone » (éditeur).
		"choixIcone": func(icone, teinte string, icones, teintes []string) map[string]any {
			return map[string]any{"Icone": icone, "Teinte": teinte, "Icones": icones, "Teintes": teintes}
		},
		// nomPays : « FR » → « France (FR) » quand le nom est connu.
		"nomPays": func(code string) string {
			if n, ok := nomsPays[code]; ok {
				return n + " (" + code + ")"
			}
			return code
		},
		// titreIcone : données du morceau « titre-icone ».
		"titreIcone": func(titre, icone, teinte string) map[string]string {
			return map[string]string{"Titre": titre, "Icone": icone, "Teinte": teinte}
		},
		// choix : données du morceau « choix-liste » (liste déroulante).
		"choix": func(nom, actuel string, valeurs []string) map[string]any {
			return map[string]any{"Nom": nom, "Actuel": actuel, "Valeurs": valeurs}
		},
		"ecart":    func(a, b time.Time) time.Duration { return b.Sub(a) },
		"lienLogs": lienLogs,
		"drapeau":  drapeau,
		"somme":    func(a, b int64) int64 { return a + b },
		"enGo": func(n int64) string {
			return strings.Replace(fmt.Sprintf("%.1f", float64(n)/(1<<30)), ".", ",", 1)
		},
		"enGoEntier": func(n int64) string { return fmt.Sprintf("%.0f", float64(n)/(1<<30)) },
		// onglet : onglets des réglages (l'onglet Olvid n'apparaît que si
		// le canal est activé).
		"onglet": func(p any, actif string) ongletsReglages {
			o := ongletsReglages{Actif: actif}
			switch v := p.(type) {
			case Page:
				o.Olvid = v.Olvid
			case *Page:
				o.Olvid = v.Olvid
			}
			return o
		},
		"majuscule": func(s string) string {
			if s == "" {
				return s
			}
			return majusculeDebut(s)
		},
		"menu": func(href, icone, libelle, actif, cle string, compteur int) lienMenu {
			return lienMenu{Href: href, Icone: icone, Libelle: libelle, Actif: actif == cle, Compteur: compteur}
		},
		"initiale": func(nom string) string {
			for _, r := range nom {
				return string(r)
			}
			return "?"
		},
		"mio":    func(n int64) int64 { return n >> 20 },
		"entier": func(f float64) int64 { return int64(f) },
	}
}

// depuis : « il y a 3 min ».
func depuis(ms int64) string {
	if ms <= 0 {
		return "jamais"
	}
	d := time.Since(time.UnixMilli(ms))
	if d < 0 {
		return "dans " + dureeLisible(-d)
	}
	return "il y a " + dureeLisible(d)
}

func dureeLisible(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%d s", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%d min", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%d h %02d", int(d.Hours()), int(d.Minutes())%60)
	default:
		return fmt.Sprintf("%d j", int(d.Hours()/24))
	}
}

// majusculeDebut : première lettre en majuscule (accents compris : « é » → « É »).
func majusculeDebut(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	if r == utf8.RuneError {
		return s
	}
	return string(unicode.ToUpper(r)) + s[n:]
}

// virgule : « 20.5 Go » devient « 20,5 Go » (écriture française).
func virgule(s string) string { return strings.Replace(s, ".", ",", 1) }

func tailleLisible(n int64) string {
	switch {
	case n >= 1<<30:
		return virgule(fmt.Sprintf("%.1f Go", float64(n)/(1<<30)))
	case n >= 1<<20:
		return virgule(fmt.Sprintf("%.1f Mo", float64(n)/(1<<20)))
	case n >= 1<<10:
		return fmt.Sprintf("%.0f Ko", float64(n)/(1<<10))
	case n == 0:
		return "0"
	default:
		return fmt.Sprintf("%d octets", n)
	}
}

// chargerGabarits : chaque page = base.html (ou seule.html pour les pages
// de connexion) + son propre fichier.
func chargerGabarits() (*gabarits, error) {
	g := &gabarits{pages: map[string]*template.Template{}}
	for _, cadre := range []struct{ base, dossier string }{
		{"gabarits/base.html", "gabarits/pages"},
		{"gabarits/seule.html", "gabarits/connexion"},
		{"gabarits/fragment.html", "gabarits/fragments"},
	} {
		entrees, err := fs.ReadDir(fichiers, cadre.dossier)
		if err != nil {
			return nil, err
		}
		for _, e := range entrees {
			nom := strings.TrimSuffix(e.Name(), ".html")
			t, err := template.New(path.Base(cadre.base)).Funcs(fonctions()).ParseFS(fichiers, cadre.base,
				"gabarits/morceaux.html", cadre.dossier+"/"+e.Name())
			if err != nil {
				return nil, fmt.Errorf("gabarit %s : %w", e.Name(), err)
			}
			g.pages[nom] = t
		}
	}
	return g, nil
}

// executer rend dans un tampon d'abord : une erreur de gabarit ne laisse
// pas une page à moitié envoyée.
func (g *gabarits) executer(w http.ResponseWriter, code int, nom string, donnees any) {
	t := g.pages[nom]
	if t == nil {
		http.Error(w, "page inconnue", http.StatusInternalServerError)
		return
	}
	var b bytes.Buffer
	if err := t.Execute(&b, donnees); err != nil {
		http.Error(w, "erreur d'affichage : "+err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(code)
	_, _ = w.Write(b.Bytes())
}

// Pages de connexion (sans menu).
type pageSeule struct {
	Titre string
	D     donneesConnexion
}

func (g *gabarits) rendreSeule(w http.ResponseWriter, nom, titre string, d donneesConnexion) {
	g.rendreSeuleCode(w, http.StatusOK, nom, titre, d)
}

func (g *gabarits) rendreSeuleCode(w http.ResponseWriter, code int, nom, titre string, d donneesConnexion) {
	g.executer(w, code, nom, pageSeule{Titre: titre, D: d})
}

// lienMenu : une entrée du menu latéral.
type lienMenu struct {
	Href, Icone, Libelle string
	Actif                bool
	Compteur             int
}

// ongletsReglages : données du morceau « onglets-reglages ».
type ongletsReglages struct {
	Actif string
	Olvid bool
}

// Page : données communes de toutes les pages de l'interface.
type Page struct {
	Titre, Menu   string
	Utilisateur   string
	Admin         bool
	CSRF          string
	Flash, Alerte string
	ModeOmbre     bool
	AConfirmer    []AConfirmer
	Version       string
	Theme         string
	Disposition   string // gauche ou haut (réglage « Interface »)
	Replie        bool   // menu replié par défaut
	NbAlertes     int    // alertes actives (compteur du menu)
	Olvid         bool   // canal Olvid activé : l'onglet « Olvid » apparaît
	D             any
}

// rendre affiche une page avec le menu (message « flash » consommé).
func (c *Console) rendre(q *req, nom, titre, menu string, d any) {
	c.rendreCode(q, http.StatusOK, nom, titre, menu, d)
}

func (c *Console) rendreCode(q *req, code int, nom, titre, menu string, d any) {
	p := Page{Titre: titre, Menu: menu, Utilisateur: q.u.Nom, Admin: q.admin(), CSRF: q.s.CSRF,
		Version: c.opt.Version, D: d}
	c.sessions.Modifier(q.s.Empreinte, func(s *auth.Session) { p.Flash, p.Alerte, s.Flash, s.Alerte = s.Flash, s.Alerte, "", "" })
	p.Flash, p.Alerte = majusculeDebut(p.Flash), majusculeDebut(p.Alerte)
	if c.opt.Aiguilleur != nil {
		p.ModeOmbre = c.opt.Aiguilleur.Mode() == "ombre"
	}
	p.AConfirmer, _ = ListerAConfirmer(q.r.Context(), c.db)
	p.Olvid = c.olvidActive(q.r.Context())
	if r := c.interfaceCourante.Load(); r != nil {
		if r.Theme != "auto" {
			p.Theme = r.Theme
		}
		p.Disposition, p.Replie = r.Menu, r.MenuReplie
	}
	if p.Disposition == "" {
		p.Disposition = "gauche"
	}
	if c.opt.Alertes != nil {
		for _, a := range c.opt.Alertes.EnCours() {
			if a.Etat == "active" {
				p.NbAlertes++
			}
		}
	}
	c.pages.executer(q.w, code, nom, p)
}

// drapeau : emoji du drapeau d'un code pays (2 lettres), vide sinon.
func drapeau(code string) string {
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return ""
	}
	return string([]rune{rune(code[0]) - 'A' + 0x1F1E6, rune(code[1]) - 'A' + 0x1F1E6})
}
