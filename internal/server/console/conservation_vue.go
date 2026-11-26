// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Page « Conservation des données » (retour du 21/11, maquette validée) :
// place sur le disque, une carte par type de données (occupé, plus ancienne
// donnée, volume par jour, jauge du plafond), durées choisies dans une liste
// et plafonds en Go ; la prévision se recalcule pendant la saisie
// (mirador.js) et l'avertissement n'apparaît que si une durée ou un plafond
// baisse. Le fonctionnement ne change pas (bornes, délai de 5 min annulable
// avant une baisse, code 2FA, audit).

// optionDuree : une durée proposée dans la liste.
type optionDuree struct {
	Valeur, Texte string
	Secondes      int64
	Choisie       bool
}

// carteConservation : un type de données (métriques brutes, agrégées, logs).
type carteConservation struct {
	Cle, Titre, Sous, Couleur string
	Occupe                    int64
	Garde                     string  // âge de la donnée la plus ancienne (« 7 j »)
	ParJour                   float64 // octets par jour (mesurés)
	Plafond                   int64   // octets ; 0 = sans plafond
	PlafondGo                 string  // valeur du champ (Go)
	PctPlafond                int     // jauge occupé / plafond
	Duree                     alertes.Duree
	Options                   []optionDuree
	Prevu                     int64  // place prévue avec les réglages actuels
	TextePrevu                string // affiché sans JavaScript (recalculé par mirador.js)
}

// vueDisque : barre « Place sur le disque ».
type vueDisque struct {
	Total, Libre, Mirador, Base, Autres int64
	Prevu                               int64 // mirador au plus, avec les réglages (plafonds compris)
	PctBrut, PctAgrege, PctLogs         float64
	PctBase, PctAutres                  float64
	Classe, Texte                       string // pastille
}

// Durées proposées par type (bornes de Conservation.Valider).
var (
	joursBrut       = []int{1, 2, 3, 7, 14, 30, 60, 90}
	joursAgrege     = []int{7, 14, 30, 60, 90, 180, 365, 730}
	joursLogs       = []int{1, 3, 7, 14, 30, 60, 90, 180, 365}
	joursAudit      = []int{90, 180, 365, 730, 1825, 3650}
	joursHistorique = []int{30, 90, 180, 365, 730}
)

// texteJours : « 1 jour », « 30 jours », « 6 mois », « 1 an », « 2 ans ».
func texteJours(j int) string {
	switch {
	case j == 1:
		return "1 jour"
	case j == 180:
		return "6 mois"
	case j%365 == 0:
		if j == 365 {
			return "1 an"
		}
		return strconv.Itoa(j/365) + " ans"
	}
	return strconv.Itoa(j) + " jours"
}

// optionsDuree : liste proposée ; la valeur actuelle y est ajoutée si elle
// n'en fait pas partie (réglage fait à la main ou par l'import).
func optionsDuree(jours []int, actuelle alertes.Duree) []optionDuree {
	var out []optionDuree
	vue := false
	for _, j := range jours {
		d := alertes.Duree(time.Duration(j) * 24 * time.Hour)
		o := optionDuree{Valeur: strconv.Itoa(j) + "d", Texte: texteJours(j), Secondes: int64(j) * 86400, Choisie: d == actuelle}
		vue = vue || o.Choisie
		out = append(out, o)
	}
	if !vue && actuelle > 0 {
		o := optionDuree{Valeur: actuelle.String(), Texte: dureeCourte(actuelle) + " (réglage actuel)", Secondes: int64(time.Duration(actuelle) / time.Second), Choisie: true}
		i := 0
		for i < len(out) && out[i].Secondes < o.Secondes {
			i++
		}
		out = append(out[:i], append([]optionDuree{o}, out[i:]...)...)
	}
	return out
}

// goTexte : octets → valeur du champ en Go (« 5 », « 0.5 », « 0 »).
func goTexte(n int64) string {
	return strconv.FormatFloat(math.Round(float64(n)/(1<<30)*100)/100, 'f', -1, 64)
}

// prevision : place prévue pour une durée et un plafond, et le texte qui
// l'explique (même calcul que mirador.js).
func prevision(parJour float64, d alertes.Duree, plafond int64) (int64, string) {
	jours := time.Duration(d).Hours() / 24
	if parJour <= 0 {
		return 0, "Prévu : pas encore mesuré (moins d'une heure de données)"
	}
	brut := parJour * jours
	t := fmt.Sprintf("%s × %s", dureeCourte(d), virgule(tailleLisible(int64(parJour))))
	if plafond > 0 && brut > float64(plafond) {
		return plafond, fmt.Sprintf("Prévu : %s (%s = %s, limité par le plafond : ~%d j gardés)",
			virgule(tailleLisible(plafond)), t, virgule(tailleLisible(int64(brut))), int(float64(plafond)/parJour))
	}
	return int64(brut), fmt.Sprintf("Prévu : %s (%s)", virgule(tailleLisible(int64(brut))), t)
}

func pctDe(n, total int64) int {
	if total <= 0 {
		return 0
	}
	return int(math.Min(100, math.Round(100*float64(n)/float64(total))))
}

// cartesConservation : les trois cartes « volumineuses » et la barre du disque.
func (c *Console) cartesConservation(e EtatStockage, cons Conservation, t time.Time) ([]carteConservation, vueDisque) {
	garde := func(p time.Time) string {
		if p.IsZero() {
			return "—"
		}
		return dureeLisible(t.Sub(p))
	}
	cartes := []carteConservation{
		{Cle: "brut", Titre: "Métriques brutes", Sous: "une valeur toutes les 30 s", Couleur: "c-brut", Occupe: e.Brut, Garde: garde(e.PlusAncienBrut),
			ParJour: e.BrutParJour, Plafond: cons.MaxBrut, Duree: cons.Brut, Options: optionsDuree(joursBrut, cons.Brut)},
		{Cle: "agrege", Titre: "Métriques agrégées", Sous: "une valeur toutes les 5 min, pour les longues périodes", Couleur: "c-agrege", Occupe: e.Agrege,
			Garde: garde(e.PlusAncienAgrege), ParJour: e.AgregeParJour, Plafond: cons.MaxAgrege, Duree: cons.Agrege, Options: optionsDuree(joursAgrege, cons.Agrege)},
		{Cle: "logs", Titre: "Logs", Sous: "toutes les lignes reçues", Couleur: "c-logs", Occupe: e.Logs, Garde: garde(e.PlusAncienLogs),
			ParJour: e.LogsParJour, Plafond: cons.MaxLogs, Duree: cons.Logs, Options: optionsDuree(joursLogs, cons.Logs)},
	}
	d := vueDisque{Total: e.DisqueTotal, Libre: e.DisqueLibre, Base: e.Base}
	for i := range cartes {
		k := &cartes[i]
		k.PlafondGo = goTexte(k.Plafond)
		k.PctPlafond = pctDe(k.Occupe, k.Plafond)
		k.Prevu, k.TextePrevu = prevision(k.ParJour, k.Duree, k.Plafond)
		d.Mirador += k.Occupe
		d.Prevu += max(k.Prevu, k.Occupe)
	}
	d.Mirador += e.Base
	d.Prevu += e.Base
	if d.Total > 0 {
		d.Autres = max(0, d.Total-d.Libre-d.Mirador)
		p := func(n int64) float64 { return math.Round(1000*float64(n)/float64(d.Total)) / 10 }
		d.PctBrut, d.PctAgrege, d.PctLogs, d.PctBase, d.PctAutres = p(e.Brut), p(e.Agrege), p(e.Logs), p(e.Base), p(d.Autres)
		reste := d.Libre - (d.Prevu - d.Mirador)
		switch {
		case reste < 0:
			d.Classe, d.Texte = "erreur", "Pas assez de place"
		case reste < max(2<<30, d.Total/20): // moins de 2 Go ou de 5 % du disque
			d.Classe, d.Texte = "attention", "Peu de place"
		default:
			d.Classe, d.Texte = "ok", "Assez de place"
		}
	}
	return cartes, d
}

// tailleBase : base SQLite du serveur (avec son journal WAL).
func tailleBase(dossier string) int64 {
	var n int64
	for _, f := range []string{"mirador.db", "mirador.db-wal", "flux.db", "flux.db-wal", "flux-sortants.db", "flux-sortants.db-wal"} { // flux.db : SPEC § 135
		if info, err := os.Stat(filepath.Join(dossier, f)); err == nil {
			n += info.Size()
		}
	}
	return n
}

// lireGo : plafond saisi en Go (« 5 », « 0,5 », vide = 0 = sans plafond).
func (f champs) gio(k string, errs *[]error) int64 {
	s := strings.TrimSpace(strings.Replace(f.texte(k), ",", ".", 1))
	if s == "" {
		return 0
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || v < 0 || v > 4096 || math.IsNaN(v) {
		*errs = append(*errs, fmt.Errorf("%s : nombre de Go (0 = sans plafond)", k))
		return 0
	}
	return int64(math.Round(v * (1 << 30)))
}

// choixDuree : liste de durées en clair pour un réglage au format vmalert
// (« 10s », « 5m », « 2h ») ; zero : texte de « 0 » (« jamais », « tout de
// suite »). La valeur actuelle est ajoutée si elle n'est pas dans la liste
// (page Notifications, retour du 21/11).
func choixDuree(courante alertes.Duree, zero string, valeurs ...string) []optionDuree {
	var out []optionDuree
	vue := false
	for _, v := range valeurs {
		d, err := alertes.LireDuree(v)
		if err != nil {
			continue
		}
		t := dureeCourte(d)
		if d == 0 {
			t = zero
		}
		o := optionDuree{Valeur: d.String(), Texte: t, Secondes: int64(time.Duration(d) / time.Second), Choisie: d == courante}
		vue = vue || o.Choisie
		out = append(out, o)
	}
	if !vue {
		o := optionDuree{Valeur: courante.String(), Texte: dureeCourte(courante), Secondes: int64(time.Duration(courante) / time.Second), Choisie: true}
		if courante == 0 {
			o.Texte = zero
		}
		i := 0
		for i < len(out) && out[i].Secondes < o.Secondes {
			i++
		}
		out = append(out[:i], append([]optionDuree{o}, out[i:]...)...)
	}
	return out
}
