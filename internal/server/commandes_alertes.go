// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"mirador-stack/internal/server/alertes"
)

// Commandes des alertes (en attendant l'interface, étape 5).

// AideAlertes : ajouté à AideCommandes.
const AideAlertes = `
Alertes :
  regles                             règles (groupe, gravité, active, origine)
  regles-importer [-essai] [-desactivees] FICHIER|-
                                     importe un fichier vmalert (system.yml…) ou mirador
                                     (export) ; rapport règle par règle ; -essai : rapport
                                     seul ; -desactivees : règles importées désactivées
  regles-exporter                    toutes les règles au format YAML mirador
  regle-activer NOM | regle-desactiver NOM | regle-supprimer NOM
  alertes [-limite 50]               historique des alertes (en cours et résolues)
  etat-regles                        évaluation de chaque règle (serveur en marche)
  alertes-en-cours                   alertes actives et en attente (serveur en marche)
  vmalert-reglages FICHIER|-         comparaison avec vmalert (JSON : actif, url, suffixe,
                                     correspondances, ignorer)
  comparer-vmalert [-csv]            écarts avec vmalert maintenant + épisodes des 7 derniers
                                     jours (-csv : 30 jours, pour un tableur)`

// commandeAlertes : renvoie traite = false si la commande n'est pas une
// commande des alertes.
func commandeAlertes(ctx context.Context, db *sql.DB, donnees, nom string, pos []string, essai, desactivees, csv bool,
	limite int, sortie io.Writer, maintenant time.Time) (traite, silencieuse bool, err error) {
	un := func() error {
		if len(pos) != 1 {
			return fmt.Errorf("1 argument attendu ; voir « aide »")
		}
		return nil
	}
	switch nom {
	case "regles":
		return true, true, listerRegles(ctx, db, sortie)
	case "regles-importer":
		if err := un(); err != nil {
			return true, false, err
		}
		return true, true, importerRegles(ctx, db, pos[0], essai, desactivees, sortie, maintenant)
	case "regles-exporter":
		rs, err := alertes.ListerRegles(ctx, db)
		if err != nil {
			return true, true, err
		}
		return true, true, alertes.Exporter(sortie, rs)
	case "regle-activer", "regle-desactiver":
		if err := un(); err != nil {
			return true, false, err
		}
		return true, false, alertes.Activer(ctx, db, pos[0], nom == "regle-activer", qui, maintenant)
	case "regle-supprimer":
		if err := un(); err != nil {
			return true, false, err
		}
		return true, false, alertes.Supprimer(ctx, db, pos[0], qui, maintenant)
	case "alertes":
		return true, true, historiqueAlertes(ctx, db, limite, sortie, maintenant)
	case "etat-regles":
		return true, true, demanderAdmin(donnees, "/regles", nil, sortie)
	case "alertes-en-cours":
		return true, true, demanderAdmin(donnees, "/alertes", nil, sortie)
	case "vmalert-reglages":
		if err := un(); err != nil {
			return true, false, err
		}
		return true, false, reglerVmalert(ctx, db, pos[0], maintenant)
	case "comparer-vmalert":
		q := url.Values{}
		if csv {
			q.Set("csv", "1")
		}
		return true, true, demanderAdmin(donnees, "/vmalert", q, sortie)
	}
	return false, false, nil
}

func listerRegles(ctx context.Context, db *sql.DB, sortie io.Writer) error {
	rs, err := alertes.ListerRegles(ctx, db)
	if err != nil {
		return err
	}
	tw := tabwriter.NewWriter(sortie, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "GROUPE\tNOM\tGRAVITÉ\tPOUR\tINTERVALLE\tACTIVE\tORIGINE")
	actives := 0
	for _, r := range rs {
		a := "non"
		if r.Active {
			a = "oui"
			actives++
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", r.Groupe, r.Nom, r.Gravite, r.Pour, r.Intervalle, a, vide(r.Origine))
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	fmt.Fprintf(sortie, "%d règle(s), %d active(s)\n", len(rs), actives)
	return nil
}

func importerRegles(ctx context.Context, db *sql.DB, fichier string, essai, desactivees bool, sortie io.Writer, maintenant time.Time) error {
	var src io.Reader
	nom := fichier
	if fichier == "-" {
		src, nom = entree, "entrée standard"
	} else {
		f, err := os.Open(fichier) // #nosec G304 -- fichier donné par l'administrateur
		if err != nil {
			return err
		}
		defer f.Close()
		src = f
	}
	rap, err := alertes.Importer(src, baseNom(nom))
	if err != nil {
		return err
	}
	if desactivees {
		for i := range rap.Regles {
			rap.Regles[i].Active = false
		}
		for i := range rap.Lignes {
			rap.Lignes[i].Active = false
		}
	}
	rap.Ecrire(sortie)
	if essai {
		fmt.Fprintln(sortie, "Essai : rien n'a été enregistré.")
		return nil
	}
	n, err := alertes.Enregistrer(ctx, db, rap.Regles, qui, maintenant)
	if err != nil {
		return err
	}
	fmt.Fprintf(sortie, "%d règle(s) enregistrée(s) ou modifiée(s) ; prises en compte par le serveur en 10 s au plus.\n", n)
	return nil
}

func baseNom(chemin string) string {
	if i := strings.LastIndexAny(chemin, `/\`); i >= 0 {
		return chemin[i+1:]
	}
	return chemin
}

func historiqueAlertes(ctx context.Context, db *sql.DB, limite int, sortie io.Writer, maintenant time.Time) error {
	if limite < 1 || limite > 10000 {
		return fmt.Errorf("limite entre 1 et 10 000")
	}
	h, err := alertes.Historique(ctx, db, limite)
	if err != nil {
		return err
	}
	ecrireAlertes(sortie, h, maintenant)
	return nil
}

// ecrireAlertes : tableau lisible (commandes alertes et alertes-en-cours).
func ecrireAlertes(sortie io.Writer, as []alertes.Alerte, maintenant time.Time) {
	tw := tabwriter.NewWriter(sortie, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "ÉTAT\tGRAVITÉ\tRÈGLE\tMACHINE\tDEPUIS\tDURÉE\tVALEUR\tRÉSUMÉ")
	for _, a := range as {
		depuis := a.Debut
		if !a.ActiveLe.IsZero() {
			depuis = a.ActiveLe
		}
		fin := maintenant
		if !a.Fin.IsZero() {
			fin = a.Fin
		}
		resume := a.Resume
		if len(resume) > 80 {
			resume = resume[:77] + "..."
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%.4g\t%s\n", a.Etat, a.Gravite, a.Regle, vide(a.Machine()),
			depuis.Local().Format("02/01 15:04"), fin.Sub(depuis).Round(time.Second), a.Valeur, resume)
	}
	_ = tw.Flush()
	fmt.Fprintf(sortie, "%d alerte(s)\n", len(as))
}

// --- Socket d'administration (serveur en marche) -------------------------------

func (s *Serveur) adminAlertes(w http.ResponseWriter, _ *http.Request) {
	var b bytes.Buffer
	as := s.alertes.EnCours()
	ecrireAlertes(&b, as, time.Now())
	n := 0
	for i := range as {
		if r := s.aiguille.RaisonSourdine(&as[i]); r != "" {
			if n == 0 {
				fmt.Fprintln(&b, "En sourdine :")
			}
			n++
			fmt.Fprintf(&b, "  %s %s : %s\n", as[i].Regle, vide(as[i].Machine()), r)
		}
	}
	fmt.Fprintf(&b, "Mode des notifications : %s\n", s.aiguille.Mode())
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b.Bytes()) // #nosec G705 -- texte brut lu par la commande, par un socket Unix
}

func (s *Serveur) adminRegles(w http.ResponseWriter, _ *http.Request) {
	var b bytes.Buffer
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "RÈGLE\tDERNIÈRE\tDURÉE\tRÉSULTATS\tEN ALERTE\tEN ATTENTE\tÉVALUATIONS\tERREURS\tDERNIÈRE ERREUR")
	var total time.Duration
	for _, r := range s.alertes.Sante() {
		total += r.Duree
		d := "-"
		if !r.Derniere.IsZero() {
			d = r.Derniere.Local().Format("15:04:05")
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d\t%d\t%d\t%s\n", r.Nom, d, r.Duree.Round(time.Microsecond), r.Resultats,
			r.EnAlerte, r.EnAttente, r.Evaluations, r.Erreurs, vide(r.Erreur))
	}
	_ = tw.Flush()
	fmt.Fprintf(&b, "Durée totale d'une évaluation de chaque règle : %s ; dernière évaluation complète réussie : %s\n",
		total.Round(time.Microsecond), s.alertes.DerniereEvaluationOK().Local().Format("15:04:05"))
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b.Bytes()) // #nosec G705 -- texte brut lu par la commande, par un socket Unix
}

func (s *Serveur) adminNotif(w http.ResponseWriter, r *http.Request) {
	var b bytes.Buffer
	canaux, hbOK, hbErr := s.postier.Etat()
	tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "CANAL\tRÔLE\tÉTAT\tDEPUIS\tVÉRIFIÉ\tERREUR")
	for _, c := range canaux {
		etat := "ok"
		if c.EnPanne {
			etat = "EN PANNE"
		}
		fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\n", c.Nom, c.Role, etat, c.Depuis.Local().Format("02/01 15:04"),
			c.Verifie.Local().Format("15:04:05"), vide(c.Erreur))
	}
	_ = tw.Flush()
	if len(canaux) == 0 {
		fmt.Fprintln(&b, "Aucun canal configuré (voir notif-reglages).")
	}
	fmt.Fprintf(&b, "Mode : %s ; messages en file d'attente : %d\n", s.aiguille.Mode(), s.postier.TailleFile(r.Context()))
	switch {
	case hbErr != "":
		fmt.Fprintf(&b, "Heartbeat : %s\n", hbErr)
	case hbOK.IsZero():
		fmt.Fprintln(&b, "Heartbeat : inactif ou pas encore appelé")
	default:
		fmt.Fprintf(&b, "Heartbeat : dernier appel réussi à %s\n", hbOK.Local().Format("15:04:05"))
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b.Bytes()) // #nosec G705 -- texte brut lu par la commande, par un socket Unix
}

func reglerVmalert(ctx context.Context, db *sql.DB, fichier string, t time.Time) error {
	var src io.Reader = entree
	if fichier != "-" {
		f, err := os.Open(fichier) // #nosec G304 -- fichier donné par l'administrateur
		if err != nil {
			return err
		}
		defer f.Close()
		src = f
	}
	r := alertes.ReglagesVmalertDefaut()
	dec := json.NewDecoder(io.LimitReader(src, 256<<10))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return fmt.Errorf("réglages illisibles : %w", err)
	}
	return alertes.EcrireReglagesVmalert(ctx, db, r, qui, t)
}

func (s *Serveur) adminVmalert(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	reg, err := alertes.LireReglagesVmalert(ctx, s.db)
	if err == nil && reg.URL == "" {
		err = errors.New("comparaison non réglée (vmalert-reglages)")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t := time.Now()
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.URL.Query().Get("csv") == "1" {
		eps, err := alertes.ListerEpisodes(ctx, s.db, t.Add(-30*24*time.Hour))
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/csv; charset=utf-8")
		c := csv.NewWriter(w)
		_ = c.Write([]string{"regle", "machine", "type", "debut", "fin", "duree_minutes"})
		for _, e := range eps {
			fin, d := "", t.Sub(e.Debut)
			if !e.Fin.IsZero() {
				fin, d = e.Fin.UTC().Format(time.RFC3339), e.Fin.Sub(e.Debut)
			}
			_ = c.Write([]string{e.Regle, e.Machine, e.Type, e.Debut.UTC().Format(time.RFC3339), fin, fmt.Sprintf("%.0f", d.Minutes())})
		}
		c.Flush()
		return
	}
	var b bytes.Buffer
	ecarts, err := s.ecartsVmalert(ctx, reg)
	if err != nil {
		fmt.Fprintf(&b, "Comparaison impossible maintenant : %v\n", err)
	} else {
		n := map[string]int{}
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "TYPE\tMACHINE\tRÈGLE\tVMALERT\tMIRADOR")
		for _, e := range ecarts {
			n[e.Type]++
			fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\n", e.Type, e.Machine, e.Regle, e.Vmalert, e.Mirador)
		}
		_ = tw.Flush()
		fmt.Fprintf(&b, "Maintenant : %d commune(s), %d manquante(s) chez mirador, %d en trop\n\n", n["commune"], n["manquante"], n["en_trop"])
	}
	eps, err := alertes.ListerEpisodes(ctx, s.db, t.Add(-7*24*time.Hour))
	if err == nil {
		fmt.Fprintf(&b, "Écarts des 7 derniers jours (%d) :\n", len(eps))
		tw := tabwriter.NewWriter(&b, 0, 0, 2, ' ', 0)
		for _, e := range eps {
			fin, d := "en cours", t.Sub(e.Debut)
			if !e.Fin.IsZero() {
				fin, d = e.Fin.Local().Format("02/01 15:04"), e.Fin.Sub(e.Debut)
			}
			fmt.Fprintf(tw, "  %s\t%s\t%s\t%s → %s\t%s\n", e.Type, e.Machine, e.Regle, e.Debut.Local().Format("02/01 15:04"), fin,
				d.Round(time.Minute))
		}
		_ = tw.Flush()
		fmt.Fprintln(&b, "Un écart de moins de 10 min vient souvent d'un simple décalage d'évaluation entre les deux.")
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	_, _ = w.Write(b.Bytes()) // #nosec G705 -- texte brut lu par la commande, par un socket Unix
}
