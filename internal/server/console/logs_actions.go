// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"hash/fnv"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

// Page Logs (SPEC § 33) : mode direct, export, recherches enregistrées.

// --- Mode direct ----------------------------------------------------------------------

const (
	directLignesMax = 200
	directRecul     = 15 * time.Minute // « depuis » plus ancien : ramené à 15 min
)

type ligneDirect struct {
	T       int64  `json:"t"`
	Heure   string `json:"heure"`
	Host    string `json:"host"`
	Job     string `json:"job"`
	Niveau  string `json:"niveau"`
	Origine string `json:"origine"`
	Classe  string `json:"classe"`
	Msg     string `json:"msg"`
}

type reponseDirect struct {
	Lignes []ligneDirect `json:"lignes"`
	Fin    int64         `json:"fin"`
}

// margeRetard : le flux relit les dernières secondes avant « depuis ». Le
// curseur suit l'heure des lignes, pas leur arrivée : le paquet d'une
// machine qui envoie après une autre, avec des heures plus anciennes, était
// sauté. Les lignes déjà envoyées sont reconnues (vus) et pas renvoyées.
const margeRetard = 30 * time.Second

// cleLigneVue : empreinte d'une ligne déjà envoyée (heure, machine, message).
type cleLigneVue struct {
	T    int64
	Host string
	Msg  uint64
}

// suiviDirect : état d'un flux en direct. debut : dernière ligne déjà
// affichée par la page à l'ouverture ; la marge ne relit jamais avant.
type suiviDirect struct {
	vus   map[cleLigneVue]bool
	debut int64
}

func empreinteLigne(t int64, host, msg string) cleLigneVue {
	h := fnv.New64a()
	_, _ = io.WriteString(h, msg)
	return cleLigneVue{T: t, Host: host, Msg: h.Sum64()}
}

// lignesDepuis : lignes arrivées après « depuis » (millisecondes), mêmes
// filtres que la page. suivi (facultatif, flux seulement) : lignes déjà
// envoyées ; la recherche repart alors margeRetard avant « depuis ».
func (c *Console) lignesDepuis(ctx context.Context, f filtresLogs, depuis int64, t time.Time, suivi *suiviDirect) (reponseDirect, error) {
	f.Fin = t.UnixMilli()
	plancher := t.Add(-directRecul).UnixMilli()
	f.Debut = max(depuis+1, plancher)
	var vus map[cleLigneVue]bool
	if suivi != nil {
		vus = suivi.vus
		f.Debut = max(depuis+1-margeRetard.Milliseconds(), suivi.debut+1, plancher)
	}
	out := reponseDirect{Lignes: []ligneDirect{}, Fin: max(depuis, f.Debut-1)}
	res, err := c.chercherLogs(ctx, f.requete(directLignesMax))
	if err != nil {
		return out, err
	}
	loc := fuseau.Load()
	for _, l := range res.Lignes {
		host := l.Champs["host"]
		if vus != nil {
			k := empreinteLigne(l.T, host, l.Msg)
			if vus[k] {
				continue
			}
			vus[k] = true
		}
		o := l.Champs["unit"]
		if o == "" {
			o = l.Champs["container"]
		}
		out.Lignes = append(out.Lignes, ligneDirect{T: l.T, Heure: time.UnixMilli(l.T).In(loc).Format("15:04:05"), Host: host,
			Job: l.Champs["job"], Niveau: l.Champs["level"], Origine: o, Classe: classeNiveau(l.Champs["level"]), Msg: l.Msg})
		if l.T > out.Fin {
			out.Fin = l.T
		}
	}
	// Rafale : seules les directLignesMax plus récentes sont lues. On le dit
	// (ligne d'avertissement en bas du lot) au lieu de sauter les autres
	// sans rien dire.
	if n := len(out.Lignes); n > 0 && (res.Tronque || len(res.Lignes) >= directLignesMax) {
		out.Lignes = append(out.Lignes, ligneDirect{T: out.Lignes[n-1].T, Heure: out.Lignes[n-1].Heure, Host: "mirador", Origine: "mode direct",
			Classe: "warn", Niveau: "warning", Msg: "Rafale : des lignes plus anciennes de ce lot ne sont pas affichées. Relancer la recherche pour tout voir."})
	}
	// Mémoire bornée : on oublie ce qui est sorti de la marge.
	for k := range vus {
		if k.T < out.Fin-margeRetard.Milliseconds() {
			delete(vus, k)
		}
	}
	return out, nil
}

// logsDirect : lignes arrivées depuis « depuis » (navigateur sans
// EventSource : lu toutes les 5 s).
func (c *Console) logsDirect(q *req) {
	w := q.w
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	v := q.r.URL.Query()
	f, err := lireFiltresLogs(v, q.t)
	if err != nil || c.opt.Logs == nil {
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"erreur": "recherche invalide"})
		return
	}
	depuis, _ := strconv.ParseInt(v.Get("depuis"), 10, 64)
	out, err := c.lignesDepuis(q.r.Context(), f, depuis, q.t, nil)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_ = json.NewEncoder(w).Encode(map[string]string{"erreur": err.Error()})
		return
	}
	_ = json.NewEncoder(w).Encode(out)
}

// Logs en direct poussés par le serveur (SPEC § 99) : la page ouvre un flux
// (EventSource) ; le serveur le réveille quand des lignes arrivent (au plus
// une recherche par seconde et par page), et ne fait rien entre deux. 30 min
// au plus ; 20 pages au plus en même temps.
const (
	dureeMaxFluxLogs = 30 * time.Minute
	maxFluxLogs      = 20
)

var nbFluxLogs atomic.Int32

func (c *Console) logsFlux(q *req) {
	v := q.r.URL.Query()
	f, err := lireFiltresLogs(v, q.t)
	if err != nil || c.opt.Logs == nil {
		http.Error(q.w, "recherche invalide", http.StatusBadRequest)
		return
	}
	if nbFluxLogs.Add(1) > maxFluxLogs {
		nbFluxLogs.Add(-1)
		http.Error(q.w, "trop de pages de logs en direct", http.StatusTooManyRequests)
		return
	}
	defer nbFluxLogs.Add(-1)
	depuis, _ := strconv.ParseInt(v.Get("depuis"), 10, 64)
	// Reconnexion seule du navigateur (réseau coupé) : il renvoie le dernier
	// « id » reçu ; on repart de là au lieu de renvoyer les mêmes lignes
	// (relecture du 21/11 : lignes en double à chaque reconnexion).
	if id, err := strconv.ParseInt(q.r.Header.Get("Last-Event-ID"), 10, 64); err == nil && id > depuis {
		depuis = id
	}
	if depuis <= 0 {
		depuis = c.maintenant().Add(-time.Minute).UnixMilli()
	}
	h := q.w.Header()
	h.Set("Content-Type", "text/event-stream; charset=utf-8")
	h.Set("Cache-Control", "no-store")
	h.Set("X-Accel-Buffering", "no")
	rc := http.NewResponseController(q.w)
	ecrire := func(s string) bool {
		_ = rc.SetWriteDeadline(time.Now().Add(delaiEcritFlux))
		if _, err := io.WriteString(q.w, s); err != nil {
			return false
		}
		return rc.Flush() == nil
	}
	q.w.WriteHeader(http.StatusOK)
	if !ecrire(": \n\n") {
		return
	}
	fin := time.NewTimer(dureeMaxFluxLogs)
	defer fin.Stop()
	bat := time.NewTicker(battementFlux)
	defer bat.Stop()
	var derniere time.Time
	suivi := &suiviDirect{vus: map[cleLigneVue]bool{}, debut: depuis}
	for {
		arrivees := c.opt.Logs.Arrivees()
		// Lignes déjà là (ouverture, ou arrivées pendant la recherche précédente).
		if pause := time.Second - time.Since(derniere); pause > 0 {
			select {
			case <-q.r.Context().Done():
				return
			case <-time.After(pause):
			}
		}
		derniere = time.Now()
		out, err := c.lignesDepuis(q.r.Context(), f, depuis, c.maintenant(), suivi)
		if err == nil && len(out.Lignes) > 0 {
			depuis = out.Fin
			b, _ := json.Marshal(out)
			if !ecrire("id: " + strconv.FormatInt(out.Fin, 10) + "\nevent: lignes\ndata: " + string(b) + "\n\n") {
				return
			}
		}
		select {
		case <-q.r.Context().Done():
			return
		case <-arrivees:
		case <-bat.C:
			if !ecrire(": \n\n") {
				return
			}
		case <-fin.C:
			ecrire("event: fin\ndata: {}\n\n")
			return
		}
	}
}

// --- Export ---------------------------------------------------------------------------

// celluleCSV : pas de formule exécutée par le tableur (« = + - @ » en tête).
func celluleCSV(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}

// logsExport : la recherche en CSV (tableur français) ou en texte brut,
// 10 000 lignes au plus.
func (c *Console) logsExport(q *req) {
	v := q.r.URL.Query()
	f, err := lireFiltresLogs(v, q.t)
	if err != nil || c.opt.Logs == nil {
		http.Error(q.w, "recherche invalide", http.StatusBadRequest)
		return
	}
	res, err := c.chercherLogs(q.r.Context(), f.requete(exportMax))
	if err != nil {
		http.Error(q.w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	ls := res.Lignes
	if m := v.Get("motif"); m != "" {
		ls = lignesDuMotif(ls, m)
	}
	loc := fuseau.Load()
	nom := "logs-" + q.t.In(loc).Format("20060102-1504")
	w := q.w
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if v.Get("format") == "txt" {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+nom+`.txt"`)
		for _, l := range ls {
			_, _ = w.Write([]byte(time.UnixMilli(l.T).In(loc).Format("2006-01-02 15:04:05") + " " + l.Champs["host"] + " " +
				l.Champs["job"] + "/" + l.Champs["level"] + " " + strings.ReplaceAll(l.Msg, "\n", " ") + "\n"))
		}
		return
	}
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	w.Header().Set("Content-Disposition", `attachment; filename="`+nom+`.csv"`)
	_, _ = w.Write([]byte("\ufeff")) // BOM : accents lus par le tableur
	cw := csv.NewWriter(w)
	cw.Comma = ';'
	_ = cw.Write([]string{"Heure", "Machine", "Source", "Niveau", "Unité / conteneur", "Message"})
	for _, l := range ls {
		o := l.Champs["unit"]
		if o == "" {
			o = l.Champs["container"]
		}
		_ = cw.Write([]string{time.UnixMilli(l.T).In(loc).Format("2006-01-02 15:04:05"), celluleCSV(l.Champs["host"]), celluleCSV(l.Champs["job"]),
			celluleCSV(l.Champs["level"]), celluleCSV(o), celluleCSV(l.Msg)})
	}
	cw.Flush()
}

// --- Recherches enregistrées -------------------------------------------------------------

const (
	recherchesMax   = 30
	nomRechercheMax = 60
)

// paramsRecherche : paramètres gardés dans une recherche enregistrée.
var paramsRecherche = map[string]bool{"host": true, "job": true, "level": true, "unit": true, "container": true, "site": true,
	"env": true, "groupe": true, "sans": true, "q": true, "periode": true, "replier": true, "limite": true, "vue": true, "tri": true}

// nettoyerRequete : seulement les paramètres connus, revérifiés.
func nettoyerRequete(brut string) (string, error) {
	v, err := url.ParseQuery(strings.TrimPrefix(brut, "?"))
	if err != nil {
		return "", errors.New("recherche illisible")
	}
	w := url.Values{}
	for k, x := range v {
		if paramsRecherche[k] {
			w[k] = x
		}
	}
	if _, err := lireFiltresLogs(w, time.Now()); err != nil {
		return "", err
	}
	s := w.Encode()
	if len(s) > 2000 {
		return "", errors.New("recherche trop longue")
	}
	return s, nil
}

// recherchesLogs : recherches enregistrées d'un utilisateur.
func (c *Console) recherchesLogs(ctx context.Context, utilisateur int64) ([]raccourciLogs, error) {
	rows, err := c.db.QueryContext(ctx, `SELECT id, nom, requete FROM recherches_logs WHERE utilisateur_id = ? ORDER BY nom`, utilisateur)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []raccourciLogs
	for rows.Next() {
		var r raccourciLogs
		var req string
		if err := rows.Scan(&r.ID, &r.Nom, &req); err != nil {
			return nil, err
		}
		r.Lien, r.Perso = "/logs?"+req, true
		out = append(out, r)
	}
	return out, rows.Err()
}

// postRechercheLogs : enregistrer ou supprimer une recherche (préférence
// personnelle : pas de code 2FA ni d'audit, comme le thème).
func (c *Console) postRechercheLogs(q *req) {
	ctx := q.r.Context()
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	retour := "/logs"
	err := func() error {
		switch f("action") {
		case "ajouter":
			nom := f("nom")
			if nom == "" || utf8.RuneCountInString(nom) > nomRechercheMax || strings.ContainsAny(nom, "\r\n<>") {
				return errors.New("nom de la recherche : 1 à 60 caractères")
			}
			req, err := nettoyerRequete(f("requete"))
			if err != nil {
				return err
			}
			retour = "/logs?" + req
			var n int
			if err := c.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM recherches_logs WHERE utilisateur_id = ?`, q.u.ID).Scan(&n); err != nil {
				return err
			}
			if n >= recherchesMax {
				return errors.New("30 recherches enregistrées au plus : en supprimer une")
			}
			_, err = c.db.ExecContext(ctx, `INSERT INTO recherches_logs(utilisateur_id, nom, requete, cree_le) VALUES (?, ?, ?, ?)`, q.u.ID, nom, req, q.t.Unix())
			return err
		case "supprimer":
			id, _ := strconv.ParseInt(f("id"), 10, 64)
			res, err := c.db.ExecContext(ctx, `DELETE FROM recherches_logs WHERE id = ? AND utilisateur_id = ?`, id, q.u.ID)
			if err != nil {
				return err
			}
			if n, _ := res.RowsAffected(); n == 0 {
				return errors.New("recherche inconnue")
			}
			return nil
		}
		return errors.New("action inconnue")
	}()
	c.retour(q, retour, err, map[bool]string{true: "recherche enregistrée", false: "recherche supprimée"}[f("action") == "ajouter"])
}
