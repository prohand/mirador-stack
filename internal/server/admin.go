// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/prometheus/prometheus/model/labels"

	"mirador-stack/internal/server/stockage"
	"mirador-stack/internal/server/stockage/journaux"
	"mirador-stack/internal/server/stockage/metriques"
)

// Consultation des données en attendant l'interface (étape 5).
//
// Le serveur en marche est le seul à pouvoir lire la TSDB : les commandes
// voir-metriques et voir-logs lui parlent par un socket Unix dans le dossier
// des données (droits 0600, propriétaire = utilisateur du serveur). Ce n'est
// PAS un port réseau : seul un « docker exec » (même utilisateur) y accède.
// Lecture seule.

const nomSocket = "admin.sock"

var (
	reNomMetrique = regexp.MustCompile(`^[a-zA-Z_:][a-zA-Z0-9_:]{0,200}$`)
	reNomSimple   = regexp.MustCompile(`^[A-Za-z0-9._@:-]{1,255}$`)
)

// ouvrirAdmin écoute sur le socket Unix et sert les requêtes de lecture.
func (s *Serveur) ouvrirAdmin() error {
	chemin := s.chemin(nomSocket)
	if err := os.Remove(chemin); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	l, err := net.Listen("unix", chemin)
	if err != nil {
		return err
	}
	if err := os.Chmod(chemin, 0o600); err != nil {
		return errors.Join(err, l.Close())
	}
	m := http.NewServeMux()
	m.HandleFunc("GET /metriques", s.adminMetriques)
	m.HandleFunc("GET /logs", s.adminLogs)
	m.HandleFunc("GET /alertes", s.adminAlertes)
	m.HandleFunc("GET /regles", s.adminRegles)
	m.HandleFunc("GET /notif", s.adminNotif)
	m.HandleFunc("GET /vmalert", s.adminVmalert)
	s.admin = &http.Server{Handler: m, ReadHeaderTimeout: 5 * time.Second, WriteTimeout: time.Minute}
	go func() { _ = s.admin.Serve(l) }()
	return nil
}

func (s *Serveur) fermerAdmin() {
	if s.admin != nil {
		ctx, annuler := context.WithTimeout(context.Background(), 5*time.Second)
		defer annuler()
		_ = s.admin.Shutdown(ctx)
		_ = os.Remove(s.chemin(nomSocket))
	}
}

func lireDepuis(q url.Values) (time.Duration, error) {
	d, err := time.ParseDuration(q.Get("depuis"))
	if q.Get("depuis") == "" {
		d, err = time.Hour, nil
	}
	if err != nil || d <= 0 || d > 31*24*time.Hour {
		return 0, errors.New("« depuis » : durée entre 1s et 744h (ex. 30m, 6h, 7d n'existe pas : 168h)")
	}
	return d, nil
}

func (s *Serveur) adminMetriques(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	nom, machine := q.Get("nom"), q.Get("machine")
	depuis, err := lireDepuis(q)
	if err == nil && !reNomMetrique.MatchString(nom) {
		err = errors.New("nom de métrique invalide")
	}
	if err == nil && machine != "" && !reNomSimple.MatchString(machine) {
		err = errors.New("nom de machine invalide")
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	filtres := []*labels.Matcher{labels.MustNewMatcher(labels.MatchEqual, labels.MetricName, nom)}
	if machine != "" {
		filtres = append(filtres, labels.MustNewMatcher(labels.MatchEqual, "instance", machine))
	}
	maintenant := time.Now()
	series, err := s.met.Lire(r.Context(), maintenant.Add(-depuis).UnixMilli(), maintenant.UnixMilli(),
		metriques.LimitesLecture{MaxSeries: 500, MaxPoints: 2_000_000}, filtres...)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SÉRIE\tPOINTS\tDERNIÈRE VALEUR\tÂGE")
	sort.Slice(series, func(i, j int) bool { return etiquettes(series[i].Labels) < etiquettes(series[j].Labels) })
	for _, x := range series {
		if len(x.Points) == 0 {
			continue
		}
		d := x.Points[len(x.Points)-1]
		fmt.Fprintf(tw, "%s\t%d\t%s\t%s\n", etiquettes(x.Labels), len(x.Points), strconv.FormatFloat(d.V, 'g', 10, 64),
			maintenant.Sub(time.UnixMilli(d.T)).Round(time.Second))
	}
	_ = tw.Flush()
	fmt.Fprintf(w, "%d séries sur %s\n", len(series), depuis) // #nosec G705 -- texte brut lu par la commande, par un socket Unix (pas de navigateur)
}

func etiquettes(ls []stockage.Label) string {
	var morceaux []string
	for _, l := range ls {
		if l.Nom != labels.MetricName {
			morceaux = append(morceaux, fmt.Sprintf("%s=%q", l.Nom, l.Valeur))
		}
	}
	return "{" + strings.Join(morceaux, ", ") + "}"
}

func (s *Serveur) adminLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	depuis, err := lireDepuis(q)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	limite, _ := strconv.Atoi(q.Get("limite"))
	if limite <= 0 || limite > 1000 {
		limite = 50
	}
	champs := map[string]string{}
	for _, c := range []string{"host", "level", "unit", "job", "container"} {
		if v := q.Get(c); v != "" {
			if !reNomSimple.MatchString(v) {
				http.Error(w, c+" invalide", http.StatusBadRequest)
				return
			}
			champs[c] = v
		}
	}
	maintenant := time.Now()
	res, err := s.jx.Chercher(r.Context(), journaux.Requete{Debut: maintenant.Add(-depuis).UnixMilli(), Fin: maintenant.UnixMilli(),
		Champs: champs, Mots: q.Get("mots"), Contient: q.Get("contient"), Limite: limite}, journaux.LimitesRechercheDefaut())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	// Plus anciennes d'abord à l'écran (comme journalctl), les plus récentes en bas.
	for i := len(res.Lignes) - 1; i >= 0; i-- {
		l := res.Lignes[i]
		source := l.Champs["unit"]
		if source == "" {
			source = l.Champs["container"]
		}
		fmt.Fprintf(w, "%s %s %s [%s] %s\n", time.UnixMilli(l.T).Local().Format("02/01 15:04:05"), l.Champs["host"], source,
			l.Champs["level"], strings.ReplaceAll(l.Msg, "\n", "⏎"))
	}
	suite := ""
	if res.Tronque {
		suite = " (il y en a d'autres : augmenter -limite ou préciser la recherche)"
	}
	fmt.Fprintf(w, "%d lignes, %d blocs lus, %d sautés%s\n", len(res.Lignes), res.BlocsLus, res.BlocsSautes, suite)
}

// --- Côté commande ---------------------------------------------------------------------

// demanderAdmin appelle le serveur en marche par son socket et recopie la réponse.
func demanderAdmin(donnees, chemin string, q url.Values, sortie io.Writer) error {
	client := &http.Client{Timeout: time.Minute, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", filepath.Join(donnees, nomSocket))
		},
	}}
	resp, err := client.Get("http://admin" + chemin + "?" + q.Encode())
	if err != nil {
		return fmt.Errorf("serveur injoignable (est-il démarré ?) : %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return errors.New(strings.TrimSpace(string(b)))
	}
	_, err = io.Copy(sortie, io.LimitReader(resp.Body, 64<<20))
	return err
}
