// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"html/template"
	"io/fs"
	"mime"
	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/taches"
	"net/http"
	"path"
	"reflect"
	"slices"
	"strconv"
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
		switch path.Ext(p) {
		case ".woff2":
			t = "font/woff2"
		case ".webmanifest": // manifeste de l'application installée (PWA)
			t = "application/manifest+json"
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
		// heureLog : « 23:52 » aujourd'hui, sinon « 14/11 23:52 » (page Logs).
		// coupeURL : adresse avec des points de coupure (<wbr>) après « / », « . »,
		// « ? », « & » : sur téléphone, une longue adresse passe à la ligne à
		// ces endroits plutôt qu'au milieu d'un mot (retour du 20/11).
		"coupeURL":   coupeURL,
		"choixDuree": choixDuree,
		// dureeSec : durée réglable en secondes (attributs data-* lus par mirador.js).
		"dureeSec": func(d any) int64 { return reflect.ValueOf(d).Int() / int64(time.Second) },
		"heureLog": func(ms int64) string {
			t := time.UnixMilli(ms).In(fuseau.Load())
			if t.Format("2006-01-02") == time.Now().In(fuseau.Load()).Format("2006-01-02") {
				return t.Format("15:04")
			}
			return t.Format("02/01 15:04")
		},
		"dateMs": func(ms int64) string {
			if ms <= 0 {
				return "—"
			}
			return time.UnixMilli(ms).In(fuseau.Load()).Format("02/01/2006 15:04:05")
		},
		"heureCourte":   func(t time.Time) string { return t.In(fuseau.Load()).Format("15:04") },
		"heureSecondes": func(t time.Time) string { return t.In(fuseau.Load()).Format("15:04:05") },
		// textePanne : panne vue par l'agent, en clair (SPEC § 98).
		"textePanne": textePanne,
		"dateCourte": func(t time.Time) string { return t.In(fuseau.Load()).Format("02/01") },
		"date": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.In(fuseau.Load()).Format("02/01/2006 15:04:05")
		},
		"depuisS": func(s int64) string { return depuis(s * 1000) },
		// sansIlYA : « il y a 5 min » → « 5 min » (colonne dont le titre le dit déjà).
		"sansIlYA": func(s string) string { return strings.TrimPrefix(s, "il y a ") },
		"depuisMs": depuis,
		// joindre : « a, b, c » (liste de noms).
		"joindre": func(l []string) string { return strings.Join(l, ", ") },
		"duree":   func(d time.Duration) string { return dureeLisible(d) },
		"dureeC":  dureeCourte,
		"enUnite": enUnite,
		"ancre":   ancre,
		"periodeFr": func(p string) string { // « 7d » → « 7 j », « 6h » → « 6 h »
			return strings.NewReplacer("d", " j", "h", " h", "w", " sem.").Replace(p)
		},
		"docs":   func(chemin string) string { return AdresseDocs + chemin },
		"taille": tailleLisible,
		// Sauvegardes Proxmox (SPEC § 89) : taille en octets (float), durée.
		"tailleOctets": func(v float64) string { return tailleLisible(int64(v)) },
		"dureeVM": func(d time.Duration) string {
			if d < time.Minute {
				return strconv.Itoa(int(d/time.Second)) + " s"
			}
			return dureeLisible(d)
		},
		"pct":   func(v float64) string { return fmt.Sprintf("%.1f %%", v) },
		"egal":  func(a, b any) bool { return fmt.Sprint(a) == fmt.Sprint(b) },
		"sub":   func(a, b int) int { return a - b },
		"seuil": formatSeuil,
		// barres : texte sur plusieurs lignes → une ligne (« | » entre elles).
		"barres": func(s string) string { return strings.ReplaceAll(s, "\n", " | ") },
		// dict : paramètres nommés pour un morceau de gabarit (« clé », valeur…).
		"dict": func(kv ...any) (map[string]any, error) {
			if len(kv)%2 != 0 {
				return nil, errors.New("dict : nombre d'arguments impair")
			}
			m := make(map[string]any, len(kv)/2)
			for i := 0; i < len(kv); i += 2 {
				k, ok := kv[i].(string)
				if !ok {
					return nil, errors.New("dict : clé non texte")
				}
				m[k] = kv[i+1]
			}
			return m, nil
		},
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
		"ecart":   func(a, b time.Time) time.Duration { return b.Sub(a) },
		"drapeau": drapeau,
		"somme":   func(a, b int64) int64 { return a + b },
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
		// sectionsReglages : onglets et cartes des réglages (Olvid seulement
		// si le canal est activé).
		"sectionsReglages": func(olvid bool) []sectionReglages {
			out := make([]sectionReglages, 0, len(sectionsReglages))
			for _, x := range sectionsReglages {
				if x.Cle != "olvid" || olvid {
					out = append(out, x)
				}
			}
			return out
		},
		"pagesLieesReglages": func() []sectionReglages { return pagesLieesReglages },
		// accord : « alerte(s) » → « alerte » ou « alertes » selon le nombre.
		"accord": accorder,
		// sorte : « sonde http » → « Sonde HTTP », « conteneur » → « Conteneur »
		// (affichage seulement ; la valeur sert aux filtres).
		"sorte": sorteLisible,
		// nomAlerte : « MiseAJourAgentEchouee » → « Mise à jour de l'agent
		// échouée » (libelle_regle.go, SPEC § 73).
		"nomAlerte": libelleRegle,
		// etatAlerte : « en_attente » → « En attente », « active » → « En cours ».
		"etatAlerte":   etatAlerte,
		"nomRegleDe":   nomRegleDe,
		"partieExport": partieExport,
		"groupeRegle":  groupeRegle,
		"dureeFr":      dureeFr,
		"motTechnique": motTechnique,
		"quiLisible":   quiLisible,
		// Tâches planifiées (SPEC § 80) : valeurs des formulaires.
		"secondes": func(d time.Duration) int64 { return int64(d / time.Second) },
		"minutes":  func(d time.Duration) int64 { return int64(d / time.Minute) },
		"heures":   func(d time.Duration) string { return strconv.FormatFloat(d.Hours(), 'f', -1, 64) },
		"autrePeriode": func(d time.Duration) bool {
			return d != 0 && d != time.Hour && d != 24*time.Hour && d != 7*24*time.Hour
		},
		"tacheParDefaut": func() taches.Tache {
			return taches.Tache{Source: taches.SourcePing, Periode: 24 * time.Hour, Marge: time.Hour, Gravite: "warning"}
		},
		"tacheDetecteeDefaut": func(source string) taches.Tache {
			t := taches.Tache{Source: source, Periode: 24 * time.Hour, Marge: 2 * time.Hour, Gravite: "warning"}
			if source == "systemd" {
				t.Periode, t.Marge = 0, time.Hour
			}
			return t
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
		// menuAValider : lien avec un compteur orange (« à valider ») au lieu
		// du compteur rouge des alertes.
		"menuAValider": func(href, icone, libelle, actif, cle string, compteur int, aide string) lienMenu {
			return lienMenu{Href: href, Icone: icone, Libelle: libelle, Actif: actif == cle, Compteur: compteur,
				ClasseCompteur: "attention", AideCompteur: aide}
		},
		"initiale": func(nom string) string {
			for _, r := range nom {
				return string(r)
			}
			return "?"
		},
		"mio": func(n int64) int64 { return n >> 20 },
		// Colonnes que l'on peut masquer dans le tableau de la vue d'ensemble.
		"colonnesParc": func() map[string]string {
			return map[string]string{"courbe": "Courbe CPU", "cpu": "CPU", "memoire": "Mémoire", "disque": "Disque",
				"alertes": "Alertes", "valider": "À valider", "majs": "Mises à jour", "vu": "Vu",
				"site": "Site", "type": "Type"}
		},
		// colonnesParcCachees : colonnes masquées d'office (navigateur sans
		// choix enregistré) : Site et Type seulement s'il y a un seul site ou
		// un seul type dans le parc (retour du 19/11, SPEC § 71).
		"colonnesParcCachees": colonnesParcCachees,
		"entier":              func(f float64) int64 { return int64(f) },
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
	if d < 5*time.Second {
		return "à l'instant" // plutôt que « il y a 0 s »
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

// dureeCourte : durée réglable affichée simplement (« 30 min », « 12 h »,
// « 7 j », « 1 an ») ; accepte time.Duration et les types Duree.
func dureeCourte(v any) string {
	rv := reflect.ValueOf(v)
	if rv.Kind() != reflect.Int64 {
		return fmt.Sprint(v)
	}
	d := time.Duration(rv.Int())
	an, jour := 365*24*time.Hour, 24*time.Hour
	switch {
	case d <= 0:
		return "0"
	case d%an == 0:
		if d == an {
			return "1 an"
		}
		return fmt.Sprintf("%d ans", d/an)
	case d%jour == 0:
		return fmt.Sprintf("%d j", d/jour)
	case d%time.Hour == 0:
		return fmt.Sprintf("%d h", d/time.Hour)
	case d%time.Minute == 0:
		return fmt.Sprintf("%d min", d/time.Minute)
	case d < time.Minute && d%time.Second == 0:
		return fmt.Sprintf("%d s", d/time.Second)
	}
	return d.String()
}

// ancre : identifiant HTML tiré d'un titre (« Réseau » → « reseau »).
func ancre(titre string) string {
	r := strings.NewReplacer("à", "a", "â", "a", "ä", "a", "é", "e", "è", "e", "ê", "e", "ë", "e", "î", "i", "ï", "i",
		"ô", "o", "ö", "o", "ù", "u", "û", "u", "ü", "u", "ç", "c", "œ", "oe")
	var b strings.Builder
	tiret := false
	for _, c := range r.Replace(strings.ToLower(titre)) {
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteRune(c)
			tiret = false
		} else if !tiret && b.Len() > 0 {
			b.WriteByte('-')
			tiret = true
		}
	}
	return "r-" + strings.TrimSuffix(b.String(), "-")
}

// AdresseDocs : documentation en ligne, cible des liens « En savoir plus »
// des aides (dépôt public du projet).
const AdresseDocs = "https://github.com/prohand/mirador-stack/blob/main/docs/"

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
		{"gabarits/ecran.html", "gabarits/ecran"}, // écran mural (SPEC § 31)
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

// gabaritRelais : page relais d'un lien ouvert depuis un autre site (voir
// versConnexion). Elle recharge tout de suite la même adresse ; le lien
// reste là si le navigateur ne suit pas le rechargement.
var gabaritRelais = template.Must(template.New("relais").Parse(`<!doctype html>
<html lang="fr"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width, initial-scale=1">
<meta http-equiv="refresh" content="0;url={{.}}"><title>mirador</title></head>
<body><p><a href="{{.}}">Ouvrir la page</a></p></body></html>
`))

// relais : vers est un chemin de ce site, déjà vérifié (suiteLocale).
func (g *gabarits) relais(w http.ResponseWriter, vers string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = gabaritRelais.Execute(w, vers)
}

// lienMenu : une entrée du menu latéral.
type lienMenu struct {
	Href, Icone, Libelle string
	Court                string // libellé de la barre du bas (téléphone)
	Actif                bool
	Compteur             int
	ClasseCompteur       string // vide (rouge) ou « attention » (orange)
	AideCompteur         string // bulle d'aide du compteur
}

// ongletsReglages : données du morceau « onglets-reglages ».
type ongletsReglages struct {
	Actif string
	Olvid bool
}

// sectionReglages : une page de Réglages. La MÊME liste sert aux onglets et
// aux cartes de la page d'accueil des réglages : mêmes noms, même ordre.
type sectionReglages struct {
	Cle, Titre, Href, Aide string
	Icone, Teinte          string // carte de la page d'accueil des réglages (SPEC § 38)
	Ou                     string // pages liées : où la page se trouve dans le menu
}

// sectionsReglages : pages rangées sous /reglages (onglets).
var sectionsReglages = []sectionReglages{
	{Cle: "notifications", Icone: "envoi", Teinte: "bleu", Titre: "Notifications", Href: "/reglages/notifications", Aide: "Canaux, principal et secours, routage, heartbeat"},
	{Cle: "olvid", Icone: "cle", Teinte: "vert", Titre: "Olvid", Href: "/reglages/olvid", Aide: "Daemon Olvid, lien d'invitation, contacts"},
	{Cle: "conservation", Icone: "stockage", Teinte: "violet", Titre: "Conservation des données", Href: "/reglages/conservation", Aide: "Durées, plafonds, état réel, estimation"},
	{Cle: "sauvegardes", Icone: "sauvegarde", Teinte: "vert", Titre: "Sauvegardes", Href: "/reglages/sauvegardes", Aide: "Fréquence, destinations, rétention, sauvegarde immédiate"},
	{Cle: "serveur", Icone: "telechargement", Teinte: "bleu", Titre: "Mise à jour du serveur", Href: "/reglages/serveur", Aide: "Version installée, dernière version publiée, mise à jour en un clic (image signée, retour automatique)"},
	{Cle: "certificats", Icone: "cadenas", Teinte: "vert", Titre: "Certificats", Href: "/reglages/certificats", Aide: "Compte DNS pour Let's Encrypt (syslog TLS des collecteurs), avancement et renouvellement"},
	{Cle: "sites", Icone: "maison", Teinte: "bleu", Titre: "Sites, types et groupes", Href: "/reglages/sites", Aide: "Listes proposées à l'ajout d'une machine et dans sa fiche"},
	{Cle: "interface", Icone: "ecran", Teinte: "bleu", Titre: "Interface", Href: "/reglages/interface", Aide: "Fuseau, thème, menu, adresses de l'interface et d'ingestion, debug"},
	{Cle: "export", Icone: "fichier", Teinte: "bleu", Titre: "Export / import", Href: "/reglages/export", Aide: "Toute la configuration en YAML, avec ou sans les secrets chiffrés"},
}

// pagesLieesReglages : réglages rangés ailleurs dans le menu (à leur place
// logique), rappelés sur la page d'accueil des réglages.
var pagesLieesReglages = []sectionReglages{
	{Titre: "Configuration des agents", Href: "/reglages/agents", Ou: "Machines", Aide: "Par défaut, par groupe, par machine ; test du masquage"},
	{Titre: "Mises à jour des agents", Href: "/mises-a-jour", Ou: "Machines", Aide: "Versions signées, déploiement progressif"},
	{Titre: "Silences et maintenances", Href: "/sourdine", Ou: "Alertes", Aide: "Silences, inhibitions, dépendances"},
	{Titre: "Utilisateurs", Href: "/utilisateurs", Ou: "Configuration", Aide: "Comptes, rôles, 2FA"},
	{Titre: "Sécurité", Href: "/securite", Ou: "Configuration", Aide: "Sessions, verrouillage, liste d'adresses"},
	{Titre: "Journal d'audit", Href: "/audit", Ou: "Configuration", Aide: "Qui, quand, avant / après"},
}

// Page : données communes de toutes les pages de l'interface.
type Page struct {
	Titre, Menu   string
	Utilisateur   string
	Admin         bool
	CSRF          string
	Flash, Alerte string
	ModeOmbre     bool
	ModeAdmin     string // heure de fin du mode administration (vide : fermé)
	AConfirmer    []AConfirmer
	Version       string
	Theme         string // data-theme forcé (clair, sombre) ; vide = suit le système
	ThemeChoix    string // choix affiché dans le sélecteur : clair, sombre, auto
	SansAides     bool   // aides masquées (bouton du menu, SPEC § 26)
	Chemin        string // page affichée (retour après le choix du thème sans JavaScript)
	Disposition   string // gauche ou haut (réglage « Interface »)
	Replie        bool   // menu replié par défaut
	NbAlertes     int    // alertes actives (compteur du menu)
	NbAValider    int    // machines avec des éléments « à valider » (compteur du menu)
	NbConteneurs  int    // conteneurs non sains ou arrêtés à tort (compteur du menu, SPEC § 42)
	NbSondes      int    // sondes en échec (compteur du menu, SPEC § 48)
	NbTaches      int    // tâches planifiées en échec ou en retard (SPEC § 80)
	// ClassesAccueil : ordre et cartes masquées de la vue d'ensemble (SPEC § 83).
	ClassesAccueil map[string]string
	// Menu réorganisable (SPEC § 79) : entrées dans l'ordre par défaut avec
	// leurs classes d'ordre ; barre du bas du téléphone.
	MenuElements []elementMenu
	MenuBas      []lienMenu
	MenuBasCles  []string
	Olvid        bool // canal Olvid activé : l'onglet « Olvid » apparaît
	D            any
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
	if q.t.Before(q.s.AdminJusqua) {
		p.ModeAdmin = q.s.AdminJusqua.In(fuseau.Load()).Format("15:04")
	}
	p.AConfirmer, _ = ListerAConfirmer(q.r.Context(), c.db)
	p.Olvid = c.olvidActive(q.r.Context())
	// Thème : celui choisi par l'utilisateur (sélecteur du menu), sinon
	// celui du serveur (Réglages › Interface) ; « auto » suit le système.
	p.Chemin = q.r.URL.RequestURI()
	p.ThemeChoix = "auto"
	if r := c.interfaceCourante.Load(); r != nil {
		p.ThemeChoix = r.Theme
		p.Disposition, p.Replie = r.Menu, r.MenuReplie
	}
	if auth.ThemesUtilisateur[q.u.Theme] {
		p.ThemeChoix = q.u.Theme
	}
	p.SansAides = q.u.SansAides
	if p.ThemeChoix != "auto" {
		p.Theme = p.ThemeChoix
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
	if c.lecteur != nil {
		p.NbAValider = len(c.aValider(q.r.Context(), q.t))
		p.NbConteneurs = c.nbConteneursProblemes(q.r.Context(), q.t)
		p.NbSondes = c.nbSondesEnEchec(q.r.Context(), q.t)
		p.NbTaches = c.nbTachesEnProbleme(q.r.Context(), q.t)
	}
	construireMenu(&p, q.u)
	p.ClassesAccueil = classesAccueil(q.u)
	c.pages.executer(q.w, code, nom, p)
}

// drapeau : emoji du drapeau d'un code pays (2 lettres), vide sinon.
func drapeau(code string) string {
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		return ""
	}
	return string([]rune{rune(code[0]) - 'A' + 0x1F1E6, rune(code[1]) - 'A' + 0x1F1E6})
}

// sorteLisible : sorte d'un élément pour l'affichage (majuscule, sigles).
func sorteLisible(s string) string {
	if s == "" {
		return s
	}
	mots := strings.Fields(s)
	for i, m := range mots {
		switch m {
		case "http", "tcp", "dns":
			mots[i] = strings.ToUpper(m)
		}
	}
	return majusculeDebut(strings.Join(mots, " "))
}

// colonnesParcCachees : « site,type », « site », « type » ou « » selon le
// nombre de sites et de types différents du parc.
func colonnesParcCachees(parc []ligneParc) string {
	sites, types := map[string]bool{}, map[string]bool{}
	for _, l := range parc {
		sites[l.Site], types[l.Env] = true, true
	}
	var c []string
	if len(sites) < 2 {
		c = append(c, "site")
	}
	if len(types) < 2 {
		c = append(c, "type")
	}
	return strings.Join(c, ",")
}

// coupeURL : texte échappé, <wbr> après chaque « / . ? & : - » (voir FuncMap).
func coupeURL(u string) template.HTML {
	var b strings.Builder
	for _, r := range u {
		b.WriteString(template.HTMLEscapeString(string(r)))
		switch r {
		case '/', '.', '?', '&', ':', '-':
			b.WriteString("<wbr>")
		}
	}
	return template.HTML(b.String()) // #nosec G203 -- chaque caractère est échappé, seul <wbr> est ajouté
}
