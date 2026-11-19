// SPDX-License-Identifier: GPL-3.0-or-later

// Package servicesmetier : services (SPEC § 50, idée reprise de New Relic
// « workloads », idée seulement). Un service regroupe ce qui le fait
// marcher : machines, conteneurs, sondes. Son état : en panne si un
// élément « clé » a un problème, dégradé si un autre en a un, OK sinon.
// Même calcul pour la page Services et pour la série interne
// mirador_service_etat (règles ApplicationEnPanne et ApplicationDegradee ;
// « services » renommés « applications » dans l'interface le 19/11, § 65).
package servicesmetier

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Sortes d'éléments.
const (
	Machine   = "machine"
	Conteneur = "conteneur" // « machine/conteneur »
	Sonde     = "sonde"
)

// États.
const (
	OK      = 0
	Degrade = 1
	Panne   = 2
)

// Bornes.
const (
	MaxServices   = 30
	MaxComposants = 30
)

// Composant : un élément d'un service.
type Composant struct {
	Sorte string `json:"sorte"`
	Nom   string `json:"nom"`
	Cle   bool   `json:"cle,omitempty"`
}

// Service : un regroupement.
type Service struct {
	Nom        string      `json:"nom"`
	Composants []Composant `json:"composants"`
}

var reNomService = regexp.MustCompile(`^[\pL\pN][\pL\pN _.()'-]{0,47}$`)

const cleReglage = "services_metier"

// Lire : services enregistrés, triés par nom.
func Lire(ctx context.Context, db *sql.DB) ([]Service, error) {
	var v string
	err := db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, cleReglage).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var out []Service
	if err := json.Unmarshal([]byte(v), &out); err != nil {
		return nil, fmt.Errorf("réglage « services » illisible : %w", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Nom < out[j].Nom })
	return out, nil
}

// Valider : noms, sortes, bornes.
func Valider(liste []Service) error {
	if len(liste) > MaxServices {
		return fmt.Errorf("%d services au plus", MaxServices)
	}
	vus := map[string]bool{}
	for _, s := range liste {
		if !reNomService.MatchString(s.Nom) {
			return fmt.Errorf("nom de service %q invalide (48 caractères au plus)", s.Nom)
		}
		if vus[s.Nom] {
			return fmt.Errorf("deux services s'appellent %s", s.Nom)
		}
		vus[s.Nom] = true
		if len(s.Composants) == 0 || len(s.Composants) > MaxComposants {
			return fmt.Errorf("service %s : 1 à %d éléments", s.Nom, MaxComposants)
		}
		for _, c := range s.Composants {
			switch c.Sorte {
			case Machine, Sonde:
			case Conteneur:
				if m, n, ok := strings.Cut(c.Nom, "/"); !ok || m == "" || n == "" {
					return fmt.Errorf("service %s : conteneur %q à écrire machine/conteneur", s.Nom, c.Nom)
				}
			default:
				return fmt.Errorf("service %s : sorte %q inconnue (machine, conteneur, sonde)", s.Nom, c.Sorte)
			}
			if c.Nom == "" || len(c.Nom) > 128 || strings.ContainsAny(c.Nom, "\"\\\n") {
				return fmt.Errorf("service %s : nom d'élément %q invalide", s.Nom, c.Nom)
			}
		}
	}
	return nil
}

// Enregistrer : liste complète, vérifiée, avec audit.
func Enregistrer(ctx context.Context, db *sql.DB, liste []Service, qui, adresse string, t time.Time) error {
	if err := Valider(liste); err != nil {
		return err
	}
	avant, _ := Lire(ctx, db)
	brutAvant, _ := json.Marshal(avant)
	brut, err := json.Marshal(liste)
	if err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // sans effet après Commit
	if _, err := tx.ExecContext(ctx, `INSERT INTO reglages(cle, valeur, modifie_le, modifie_par) VALUES (?, ?, ?, ?)
		ON CONFLICT(cle) DO UPDATE SET valeur = excluded.valeur, modifie_le = excluded.modifie_le, modifie_par = excluded.modifie_par`,
		cleReglage, string(brut), t.Unix(), qui); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO audit(le, qui, action, objet, avant, apres, adresse) VALUES (?, ?, 'services', '', ?, ?, ?)`,
		t.Unix(), qui, string(brutAvant), string(brut), adresse); err != nil {
		return err
	}
	return tx.Commit()
}

// Lignes : « [clé] sorte nom », une par élément (formulaire).
func Lignes(s Service) string {
	var b strings.Builder
	for _, c := range s.Composants {
		if c.Cle {
			b.WriteString("clé ")
		}
		b.WriteString(c.Sorte + " " + c.Nom + "\n")
	}
	return b.String()
}

// LireLignes : l'inverse de Lignes.
func LireLignes(texte string) ([]Composant, error) {
	var out []Composant
	for _, l := range strings.Split(texte, "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		c := Composant{}
		if r, ok := strings.CutPrefix(l, "clé "); ok {
			c.Cle, l = true, strings.TrimSpace(r)
		} else if r, ok := strings.CutPrefix(l, "cle "); ok {
			c.Cle, l = true, strings.TrimSpace(r)
		}
		sorte, nom, ok := strings.Cut(l, " ")
		if !ok {
			return nil, fmt.Errorf("ligne %q : « [clé] machine|conteneur|sonde nom » attendu", l)
		}
		c.Sorte, c.Nom = sorte, strings.TrimSpace(nom)
		out = append(out, c)
	}
	return out, nil
}

// Lecteur : requêtes instantanées sur les métriques.
type Lecteur interface {
	Instant(ctx context.Context, expr string, t time.Time) ([]alertes.Echantillon, error)
}

// EtatComposant : état d'un élément.
type EtatComposant struct {
	Composant
	Texte  string // « OK · 41 ms », « Non sain », « Muette »…
	Classe string // ok, attention, erreur, info, "" (inconnu)
}

// EtatService : état d'un service.
type EtatService struct {
	Service
	Etat       int
	Texte      string // OK, Dégradé, En panne
	Classe     string
	Composants []EtatComposant
}

// Calculer : état de chaque service à l'instant t.
func Calculer(ctx context.Context, l Lecteur, enCours []alertes.Alerte, reglesPanne []string, services []Service, t time.Time) []EtatService {
	panne := map[string]bool{}
	for _, r := range reglesPanne {
		panne[r] = true
	}
	// Alertes actives par machine : panne, ou pire gravité.
	type resume struct {
		panne     bool
		critiques int
		avert     int
	}
	parMachine := map[string]*resume{}
	for _, a := range enCours {
		if a.Etat != alertes.Active {
			continue
		}
		m := a.Machine()
		r := parMachine[m]
		if r == nil {
			r = &resume{}
			parMachine[m] = r
		}
		switch {
		case panne[a.Regle]:
			r.panne = true
		case a.Gravite == alertes.Critique:
			r.critiques++
		case a.Gravite == alertes.Avertissement:
			r.avert++
		}
	}
	lire := func(expr string, cle func(alertes.Echantillon) string) map[string]float64 {
		out := map[string]float64{}
		if l == nil {
			return out
		}
		ech, err := l.Instant(ctx, expr, t)
		if err != nil {
			return out
		}
		for _, e := range ech {
			if !math.IsNaN(e.Valeur) {
				out[cle(e)] = e.Valeur
			}
		}
		return out
	}
	parCt := func(e alertes.Echantillon) string {
		return e.Etiquettes.Get("instance") + "/" + e.Etiquettes.Get("name")
	}
	parSonde := func(e alertes.Echantillon) string { return e.Etiquettes.Get("sonde") }
	var marche, sain, succes, duree, lente map[string]float64
	besoin := map[string]bool{}
	for _, s := range services {
		for _, c := range s.Composants {
			besoin[c.Sorte] = true
		}
	}
	if besoin[Conteneur] {
		marche = lire(`max by (instance, name) (node_docker_container_running)`, parCt)
		sain = lire(`max by (instance, name) (node_docker_container_healthy)`, parCt)
	}
	if besoin[Sonde] {
		succes = lire(`max by (sonde) (mirador_sonde_succes)`, parSonde)
		duree = lire(`max by (sonde) (mirador_sonde_duree_secondes)`, parSonde)
		lente = lire(`max by (sonde) (mirador_sonde_lente)`, parSonde)
	}
	out := make([]EtatService, 0, len(services))
	for _, s := range services {
		e := EtatService{Service: s}
		for _, c := range s.Composants {
			ec := EtatComposant{Composant: c}
			switch c.Sorte {
			case Machine:
				r := parMachine[c.Nom]
				switch {
				case r != nil && r.panne:
					ec.Texte, ec.Classe = "Muette ou en panne", "erreur"
				case r != nil && r.critiques > 0:
					ec.Texte, ec.Classe = strconv.Itoa(r.critiques)+" alerte(s) critique(s)", "erreur"
				case r != nil && r.avert > 0:
					ec.Texte, ec.Classe = strconv.Itoa(r.avert)+" avertissement(s)", "attention"
				default:
					ec.Texte, ec.Classe = "OK", "ok"
				}
			case Conteneur:
				m, ok := marche[c.Nom]
				switch {
				case !ok:
					ec.Texte = "Inconnu"
				case m == 0:
					ec.Texte, ec.Classe = "Arrêté", "erreur"
				case sain[c.Nom] == 0 && hasKey(sain, c.Nom):
					ec.Texte, ec.Classe = "Non sain", "erreur"
				default:
					ec.Texte, ec.Classe = "En marche", "ok"
				}
			case Sonde:
				v, ok := succes[c.Nom]
				ms := strconv.FormatFloat(duree[c.Nom]*1000, 'f', 0, 64) + " ms"
				switch {
				case !ok:
					ec.Texte = "Pas d'essai"
				case v == 0:
					ec.Texte, ec.Classe = "Échec", "erreur"
				case lente[c.Nom] == 1:
					ec.Texte, ec.Classe = "Lente · "+ms, "attention"
				default:
					ec.Texte, ec.Classe = "OK · "+ms, "ok"
				}
			}
			// Inconnu (rien reçu) : compté comme un problème pour un élément
			// clé (on ne sait pas qu'il marche), ignoré sinon.
			probleme := ec.Classe == "erreur" || ec.Classe == "attention" || ec.Classe == "" && c.Cle
			switch {
			case probleme && c.Cle && ec.Classe != "attention":
				e.Etat = Panne
			case probleme && e.Etat < Degrade:
				e.Etat = Degrade
			}
			e.Composants = append(e.Composants, ec)
		}
		e.Texte, e.Classe = Texte(e.Etat)
		out = append(out, e)
	}
	return out
}

func hasKey(m map[string]float64, k string) bool { _, ok := m[k]; return ok }

// Texte : libellé et classe d'un état.
func Texte(etat int) (string, string) {
	switch etat {
	case Panne:
		return "En panne", "erreur"
	case Degrade:
		return "Dégradée", "attention"
	}
	return "OK", "ok"
}
