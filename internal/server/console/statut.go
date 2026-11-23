// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"database/sql"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"math"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/servicesmetier"
	"mirador-stack/internal/server/web"
)

// Page de statut publique (SPEC § 84, idée reprise de Checkmate ; écartée
// au § 12.2, acceptée le 20/11 avec ces protections) : /statut/<jeton>,
// sans connexion, servie avant la liste d'adresses autorisées. Seules les
// applications choisies y paraissent, par leur nom : jamais de machine,
// d'adresse IP, de message d'alerte ni de log. Sans script ni cookie.

const cleStatut = "page_statut"

// ReglagesStatut : réglage de la page (Réglages › Interface).
type ReglagesStatut struct {
	Actif        bool     `json:"actif"`
	Titre        string   `json:"titre"`
	Jeton        string   `json:"jeton"` // partie secrète de l'adresse (à partager)
	Applications []string `json:"applications"`
}

const joursStatut = 90

func lireStatut(ctx context.Context, db *sql.DB) ReglagesStatut {
	r := ReglagesStatut{Titre: "État des services"}
	_, _ = lireJSONReglage(ctx, db, cleStatut, &r)
	return r
}

func nouveauJetonStatut() string {
	b := make([]byte, 18)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// --- Page publique ----------------------------------------------------------

//go:embed gabarits/statut/statut.html
var gabaritStatut string

//go:embed gabarits/statut/statut.css
var styleStatut []byte

var pageStatut = template.Must(template.New("statut").Parse(gabaritStatut))

type jourStatut struct{ Classe, Titre string }

type appStatut struct {
	Nom, Texte, Classe string
	Dispo              string // « 99,98 % »
	Jours              []jourStatut
}

type incidentStatut struct {
	EnCours bool
	Titre   string // « Nextcloud en panne »
	Quand   string
}

type donneesStatut struct {
	Titre      string
	Bandeau    string
	ClasseBand string
	Apps       []appStatut
	Incidents  []incidentStatut
	Le         string
	Debut      string
}

// servirStatut : /statut/<jeton> et /statut.css.
func (c *Console) servirStatut(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		http.Error(w, "méthode refusée", http.StatusMethodNotAllowed)
		return
	}
	h := w.Header()
	h.Set("X-Robots-Tag", "noindex, nofollow")
	h.Set("Referrer-Policy", "no-referrer")
	if r.URL.Path == "/statut.css" {
		h.Set("Content-Type", "text/css; charset=utf-8")
		h.Set("Cache-Control", "public, max-age=3600")
		_, _ = w.Write(styleStatut)
		return
	}
	t := c.maintenant()
	if !c.statuts.permis(web.AdresseClient(r).String(), t) {
		http.Error(w, "trop d'appels", http.StatusTooManyRequests)
		return
	}
	ctx := r.Context()
	reg := lireStatut(ctx, c.db)
	jeton := strings.TrimPrefix(r.URL.Path, "/statut/")
	if !reg.Actif || reg.Jeton == "" || len(jeton) > 64 || subtle.ConstantTimeCompare([]byte(jeton), []byte(reg.Jeton)) != 1 {
		http.NotFound(w, r)
		return
	}
	d := c.donneesStatut(ctx, reg, t)
	var buf bytes.Buffer
	if err := pageStatut.Execute(&buf, d); err != nil {
		http.Error(w, "page indisponible", http.StatusInternalServerError)
		return
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	_, _ = w.Write(buf.Bytes())
}

// intervalleEtat : période d'une alerte d'application.
type intervalleEtat struct {
	d, f  int64 // secondes
	panne bool
}

func (c *Console) donneesStatut(ctx context.Context, reg ReglagesStatut, t time.Time) donneesStatut {
	loc := fuseau.Load()
	d := donneesStatut{Titre: reg.Titre, Le: t.In(loc).Format("02/01 15:04")}
	// Applications choisies qui existent encore.
	existe := map[string]bool{}
	if ss, err := servicesmetier.Lire(ctx, c.db); err == nil {
		for _, s := range ss {
			existe[s.Nom] = true
		}
	}
	var noms []string
	for _, n := range reg.Applications {
		if existe[n] {
			noms = append(noms, n)
		}
	}
	// État actuel : série écrite chaque minute par le serveur (§ 50).
	etats := map[string]int{}
	if c.lecteur != nil {
		if ech, err := c.instant(ctx, `max by (service) (mirador_service_etat)`, t); err == nil {
			for _, e := range ech {
				if !math.IsNaN(e.Valeur) {
					etats[e.Etiquettes.Get("service")] = int(e.Valeur)
				}
			}
		}
	}
	// Historique : alertes ApplicationEnPanne / ApplicationDegradee (gardées
	// en base bien plus longtemps que les métriques brutes).
	j0 := t.In(loc).AddDate(0, 0, -(joursStatut - 1))
	j0 = time.Date(j0.Year(), j0.Month(), j0.Day(), 0, 0, 0, 0, loc)
	d.Debut = "il y a " + strconv.Itoa(joursStatut) + " jours"
	parApp := c.historiqueApplications(ctx, j0, t)
	nbPanne, nbDegrade := 0, 0
	var touchees []string
	for _, n := range noms {
		a := appStatut{Nom: n}
		switch etats[n] {
		case servicesmetier.Panne:
			a.Texte, a.Classe = "En panne", "k"
			nbPanne++
			touchees = append(touchees, n)
		case servicesmetier.Degrade:
			a.Texte, a.Classe = "Perturbé", "a"
			nbDegrade++
			touchees = append(touchees, n)
		default:
			a.Texte, a.Classe = "Fonctionne", ""
		}
		var pannes []intervalle
		for _, iv := range parApp[n] {
			if iv.panne {
				pannes = append(pannes, intervalle{iv.d, iv.f})
			}
		}
		pannes = reunir(pannes)
		total := t.Unix() - j0.Unix()
		arret := dureeDans(pannes, j0.Unix(), t.Unix())
		a.Dispo = pct(100*(1-float64(arret)/float64(total))) + " %"
		for k := 0; k < joursStatut; k++ {
			j := j0.AddDate(0, 0, k)
			jf := j.AddDate(0, 0, 1)
			if jf.After(t) {
				jf = t
			}
			titre := j.Format("02/01")
			classe := ""
			for _, iv := range parApp[n] {
				if iv.d < jf.Unix() && iv.f > j.Unix() {
					if iv.panne {
						classe = "k"
					} else if classe == "" {
						classe = "a"
					}
				}
			}
			switch classe {
			case "k":
				titre += " : en panne " + dureeArret(dureeDans(pannes, j.Unix(), jf.Unix()))
			case "a":
				titre += " : perturbé"
			default:
				titre += " : fonctionne"
			}
			a.Jours = append(a.Jours, jourStatut{classe, titre})
		}
		d.Apps = append(d.Apps, a)
	}
	switch {
	case len(noms) == 0:
		d.Bandeau, d.ClasseBand = "Aucun service à afficher pour le moment.", "n"
	case nbPanne+nbDegrade == 0:
		d.Bandeau, d.ClasseBand = "Tous les services fonctionnent", ""
	case len(touchees) == 1 && nbPanne == 1:
		d.Bandeau, d.ClasseBand = "Un service en panne : "+touchees[0], "k"
	case len(touchees) == 1:
		d.Bandeau, d.ClasseBand = "Un service perturbé : "+touchees[0], "a"
	default:
		cl := "a"
		if nbPanne > 0 {
			cl = "k"
		}
		d.Bandeau, d.ClasseBand = fmt.Sprintf("%d services touchés : %s", len(touchees), strings.Join(touchees, ", ")), cl
	}
	// Incidents : en cours, puis terminés des 14 derniers jours (10 au plus).
	type inc struct {
		incidentStatut
		debut int64
	}
	var incs []inc
	depuis := t.Add(-14 * 24 * time.Hour).Unix()
	for _, n := range noms {
		for _, iv := range parApp[n] {
			enCours := iv.f >= t.Unix()-60
			if !enCours && iv.f < depuis {
				continue
			}
			x := inc{incidentStatut{EnCours: enCours, Titre: n + map[bool]string{true: " en panne", false: " perturbé"}[iv.panne]}, iv.d}
			deb := time.Unix(iv.d, 0).In(loc)
			if enCours {
				x.Quand = "depuis " + deb.Format("02/01 15:04")
			} else {
				fin := time.Unix(iv.f, 0).In(loc)
				x.Quand = deb.Format("02/01 15:04") + " → " + fin.Format("15:04") + " (" + dureeArret(iv.f-iv.d) + ")"
			}
			incs = append(incs, x)
		}
	}
	sort.SliceStable(incs, func(i, j int) bool {
		if incs[i].EnCours != incs[j].EnCours {
			return incs[i].EnCours
		}
		return incs[i].debut > incs[j].debut
	})
	for i, x := range incs {
		if i == 10 {
			break
		}
		d.Incidents = append(d.Incidents, x.incidentStatut)
	}
	return d
}

// --- Réglage (Réglages › Interface) ------------------------------------------

type donneesStatutReglage struct {
	ReglagesStatut
	URL          string
	Applications []string // toutes les applications
	Choisies     map[string]bool
}

func (c *Console) donneesStatutReglage(q *req) donneesStatutReglage {
	ctx := q.r.Context()
	r := lireStatut(ctx, c.db)
	d := donneesStatutReglage{ReglagesStatut: r, Choisies: map[string]bool{}}
	if r.Jeton != "" {
		d.URL = "https://" + q.r.Host + "/statut/" + r.Jeton
	}
	if ss, err := servicesmetier.Lire(ctx, c.db); err == nil {
		for _, s := range ss {
			d.Applications = append(d.Applications, s.Nom)
		}
	}
	for _, a := range r.Applications {
		d.Choisies[a] = true
	}
	return d
}

func (c *Console) postStatut(q *req) {
	ctx := q.r.Context()
	f := champs{q}
	r := lireStatut(ctx, c.db)
	switch f.texte("action") {
	case "jeton":
		r.Jeton = nouveauJetonStatut()
	case "enregistrer":
		r.Actif = f.coche("actif")
		r.Titre = strings.TrimSpace(f.texte("titre"))
		r.Applications = nil
		existe := map[string]bool{}
		if ss, err := servicesmetier.Lire(ctx, c.db); err == nil {
			for _, s := range ss {
				existe[s.Nom] = true
			}
		}
		for _, a := range q.r.PostForm["applications"] {
			if existe[a] {
				r.Applications = append(r.Applications, a)
			}
		}
		if r.Jeton == "" {
			r.Jeton = nouveauJetonStatut()
		}
	default:
		c.retour(q, "/reglages/statut", errors.New("action inconnue"), "")
		return
	}
	if r.Titre == "" {
		r.Titre = "État des services"
	}
	if len([]rune(r.Titre)) > 80 || strings.ContainsAny(r.Titre, "\r\n<>") {
		c.retour(q, "/reglages/statut", errors.New("titre : 80 caractères au plus, sans < >"), "")
		return
	}
	brut, _ := json.Marshal(r)
	tx, err := c.db.BeginTx(ctx, nil)
	if err == nil {
		defer tx.Rollback() //nolint:errcheck // sans effet après Commit
		_, err = tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
			ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
			cleStatut, string(brut), q.t.Unix(), q.qui())
		if err == nil {
			detail := fmt.Sprintf("actif=%v applications=%s", r.Actif, strings.Join(r.Applications, ","))
			if f.texte("action") == "jeton" {
				detail = "nouvelle adresse"
			}
			err = Auditer(ctx, tx, q.qui(), "page_statut", "", "", detail, q.ip.String(), q.t)
		}
		if err == nil {
			err = tx.Commit()
		}
	}
	ok := "page de statut enregistrée"
	if f.texte("action") == "jeton" {
		ok = "nouvelle adresse : l'ancienne ne marche plus"
	}
	c.retour(q, "/reglages/statut", err, ok)
}
