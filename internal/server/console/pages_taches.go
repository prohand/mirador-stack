// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/taches"
	"mirador-stack/internal/server/web"
)

// Tâches planifiées (SPEC § 80) : page Sondes › Tâches planifiées, et
// adresse /ping/<jeton> appelée par une tâche à la fin de son exécution.

// --- /ping/<jeton>[/echec | /debut | /code/<n>]?duree=<s>&message=… ---------------

// limitePing : au plus 60 appels par minute et par adresse (une tâche en
// boucle ne remplit pas le journal).
type limitePing struct {
	mu     sync.Mutex
	minute int64
	nb     map[string]int
}

func (l *limitePing) permis(ip string, t time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if m := t.Unix() / 60; m != l.minute || l.nb == nil {
		l.minute, l.nb = m, map[string]int{}
	}
	l.nb[ip]++
	return l.nb[ip] <= 60
}

// servirPing : appelé avant la liste d'adresses autorisées et sans
// session : le jeton (aléatoire, 144 bits, gardé haché) suffit, et il ne
// permet qu'une chose : noter « la tâche a tourné ».
func (c *Console) servirPing(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodGet && r.Method != http.MethodPost && r.Method != http.MethodHead {
		http.Error(w, "méthode refusée", http.StatusMethodNotAllowed)
		return
	}
	t := c.maintenant()
	ip := web.AdresseClient(r).String()
	if !c.pings.permis(ip, t) {
		http.Error(w, "trop d'appels", http.StatusTooManyRequests)
		return
	}
	parts := strings.Split(strings.Trim(strings.TrimPrefix(r.URL.Path, "/ping/"), "/"), "/")
	jeton, debut, code := parts[0], false, 0
	switch {
	case len(parts) == 1:
	case len(parts) == 2 && parts[1] == "echec":
		code = 1
	case len(parts) == 2 && parts[1] == "debut":
		debut = true
	case len(parts) == 3 && parts[1] == "code":
		n, err := strconv.Atoi(parts[2])
		if err != nil || n < 0 || n > 255 {
			http.Error(w, "code : 0 à 255", http.StatusBadRequest)
			return
		}
		code = n
	default:
		http.Error(w, "adresse inconnue", http.StatusNotFound)
		return
	}
	var duree time.Duration
	if s := r.URL.Query().Get("duree"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 || n > 7*24*3600 {
			http.Error(w, "durée : nombre de secondes", http.StatusBadRequest)
			return
		}
		duree = time.Duration(n) * time.Second
	}
	message := r.URL.Query().Get("message")
	if r.Method == http.MethodPost && message == "" {
		b, _ := io.ReadAll(io.LimitReader(r.Body, 200))
		message = strings.TrimSpace(string(b))
	}
	message = strings.Map(func(x rune) rune { // une ligne, sans caractère de contrôle
		if x < 32 || x == 127 {
			return ' '
		}
		return x
	}, message)
	err := taches.Signaler(r.Context(), c.db, jeton, debut, code, duree, message, t)
	switch {
	case errors.Is(err, taches.ErrJetonInconnu):
		c.journal.Warn("ping de tâche : jeton inconnu", "ip", ip)
		http.Error(w, "jeton inconnu", http.StatusNotFound)
	case err != nil:
		http.Error(w, "base indisponible", http.StatusServiceUnavailable)
	default:
		_, _ = io.WriteString(w, "OK\n")
	}
}

// --- Page ---------------------------------------------------------------------------

type ligneTache struct {
	T        taches.Tache
	Etat     taches.Etat
	Attendue string // « chaque jour, marge 1 h », « selon le timer, marge 1 h »
	Source   string // « Timer systemd », « Conteneur Docker »…
	Fin      time.Time
	Duree    string
	Message  string
	Classe   string // trait de la ligne : ok, echec
	// Description : ce que fait la tâche, en clair (« Sauvegarde des VM et
	// conteneurs… », retour du 20/11 : « à quoi correspond cette tâche ? »).
	Description string
	Puces       []puceTache // infos courtes (sauvegardes Proxmox, SPEC § 89)
	// Prochaine exécution (retour du 20/11) : annoncée par le timer ou le
	// planning Proxmox, sinon estimée (dernière fin + fréquence).
	Prochaine        time.Time
	ProchaineTexte   string // « dans 9 h », « prévue il y a 2 h »
	ProchaineEstimee bool
}

type tacheDetectee struct {
	taches.Detectee
	SourceTexte string
	Fin         time.Time
	Code        int
	Systeme     bool // tâche du système (apt, logrotate…) : rangée à part
	Description    string
	Puces          []puceTache
	Prochaine      time.Time
	ProchaineTexte string
}

// prochaineTache : prochaine exécution annoncée (timer systemd, planning
// Proxmox) ; sinon estimée d'après la dernière fin et la fréquence attendue.
func prochaineTache(r taches.Releve, periode time.Duration) (time.Time, bool) {
	if !r.Prochaine.IsZero() {
		return r.Prochaine, false
	}
	if periode > 0 && !r.Fin.IsZero() {
		return r.Fin.Add(periode), true
	}
	return time.Time{}, false
}

// prochaineTexte : « dans 9 h », « prévue il y a 2 h » ("" : inconnue).
func prochaineTexte(p, maintenant time.Time) string {
	switch {
	case p.IsZero():
		return ""
	case !p.Before(maintenant):
		return "dans " + dureeCourteTache(p.Sub(maintenant))
	}
	return "prévue il y a " + dureeCourteTache(maintenant.Sub(p))
}

// dureeCourteTache : « 2 j », « 9 h », « 25 min ».
func dureeCourteTache(d time.Duration) string {
	switch {
	case d >= 48*time.Hour:
		return strconv.Itoa(int(d/(24*time.Hour))) + " j"
	case d >= time.Hour:
		return strconv.Itoa(int(d/time.Hour)) + " h"
	}
	return strconv.Itoa(max(1, int(d/time.Minute))) + " min"
}

// puceTache : info courte affichée sous une tâche.
type puceTache struct{ Texte, Classe string }

// pucesTache : infos de la dernière sauvegarde Proxmox (SPEC § 89).
func pucesTache(r taches.Releve) []puceTache {
	var out []puceTache
	if n := len(r.VMs); n > 0 {
		out = append(out, puceTache{strconv.Itoa(n) + " VM/CT", ""})
		var echecs []string
		var octets float64
		for _, v := range r.VMs {
			if !v.OK {
				echecs = append(echecs, v.ID)
			}
			octets += v.Octets
		}
		if len(echecs) > 0 {
			t := strconv.Itoa(len(echecs)) + " en échec : " + strings.Join(echecs[:min(len(echecs), 4)], ", ")
			if len(echecs) > 4 {
				t += "…"
			}
			out = append(out, puceTache{t, "er"})
		}
		if octets > 0 {
			out = append(out, puceTache{tailleLisible(int64(octets)), ""})
		}
	}
	if r.Stockage != "" {
		out = append(out, puceTache{"vers " + r.Stockage, ""})
	}
	if r.Planning != "" {
		out = append(out, puceTache{"planning Proxmox : " + r.Planning, ""})
	}
	if r.Commentaire != "" {
		out = append(out, puceTache{"« " + r.Commentaire + " »", ""})
	}
	return out
}

type donneesTaches struct {
	Suivies              []ligneTache
	NbOK, NbProbleme     int
	Detectees, DuSysteme []tacheDetectee
	Machines             []string
	Nouveau              *nouveauPing // jeton tout juste créé (montré une fois)
	Ajout                bool         // formulaire « Nouvelle tâche à signaler » ouvert (en haut)
	Base                 string       // https://hôte
}

type nouveauPing struct{ Nom, URL string }

var textesSources = map[string]string{"systemd": "Timer systemd", "docker": "Conteneur Docker", "pve": "Sauvegarde Proxmox VE",
	"pbs": "Proxmox Backup Server", taches.SourcePing: "Signal (curl)"}

// descriptionTache : ce que fait une tâche, en clair, d'après sa source et son
// nom (tâches Proxmox : « type [identifiant] », lues dans le journal des
// tâches du nœud, SPEC § 80).
func descriptionTache(source, nom string) string {
	typ, id, _ := strings.Cut(nom, " ")
	switch source {
	case "pve":
		if typ == "vzdump" {
			if id == "" {
				return "Sauvegarde planifiée des VM et conteneurs (travail de Datacenter › Sauvegarde, lancé sur ce nœud)"
			}
			return "Sauvegarde de la VM ou du conteneur " + id
		}
	case "pbs":
		quoi := map[string]string{
			"backup": "Sauvegarde reçue par Proxmox Backup Server", "garbage_collection": "Nettoyage du datastore (garbage collection)",
			"verify": "Vérification des sauvegardes", "verificationjob": "Vérification planifiée des sauvegardes",
			"prune": "Élagage des anciennes sauvegardes", "prunejob": "Élagage planifié des anciennes sauvegardes",
			"syncjob": "Synchronisation avec un autre Proxmox Backup Server",
		}[typ]
		if quoi != "" && id != "" {
			return quoi + " (" + id + ")"
		}
		return quoi
	case "docker":
		return "Conteneur lancé pour une tâche, puis arrêté (code de sortie suivi)"
	case "systemd":
		return "Timer systemd : lance le service " + strings.TrimSuffix(nom, ".timer") + " à heure fixe"
	}
	return ""
}

// timerSysteme : timers livrés avec le système (rangés à part, rarement utiles).
func timerSysteme(nom string) bool {
	for _, p := range []string{"apt-daily", "man-db", "fstrim", "logrotate", "e2scrub", "dpkg-db-backup", "motd-news", "systemd-",
		"plocate", "mlocate", "sysstat", "phpsessionclean", "ua-timer", "update-notifier", "fwupd", "pve-daily-update",
		"anacron", "snapd", "apt-listbugs", "exim4-base", "certbot-renew-hook"} {
		if strings.HasPrefix(nom, p) {
			return true
		}
	}
	return false
}

func periodeTexte(d time.Duration) string {
	switch d {
	case 0:
		return "selon le timer"
	case time.Hour:
		return "chaque heure"
	case 24 * time.Hour:
		return "chaque jour"
	case 7 * 24 * time.Hour:
		return "chaque semaine"
	}
	return "toutes les " + dureeLisible(d)
}

// donneesTaches : tâches suivies et détectées ; machine non vide : seulement
// celles de cette machine (page de la machine).
func (c *Console) donneesTaches(q *req, machine string) donneesTaches {
	ctx := q.r.Context()
	d := donneesTaches{Base: "https://" + q.r.Host}
	det := taches.Detectees(func(expr string) ([]alertes.Echantillon, error) { return c.instant(ctx, expr, q.t) })
	ts, err := taches.Lister(ctx, c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	suivies := map[string]bool{}
	for _, t := range taches.AvecReleves(ts, det) {
		suivies[t.Cle] = true
		if machine != "" && t.Machine != machine {
			continue
		}
		l := ligneTache{T: t, Etat: taches.Calculer(t, t.Releve, q.t), Source: textesSources[t.Source], Fin: t.Releve.Fin, Message: t.Releve.Message,
			Description: descriptionTache(t.Source, t.Nom), Puces: pucesTache(t.Releve)}
		l.Prochaine, l.ProchaineEstimee = prochaineTache(t.Releve, t.Periode)
		l.ProchaineTexte = prochaineTexte(l.Prochaine, q.t)
		l.Attendue = periodeTexte(t.Periode) + ", marge " + dureeLisible(t.Marge)
		if t.Periode == 0 && t.Source == "pve" {
			l.Attendue = "selon le planning Proxmox, marge " + dureeLisible(t.Marge)
			if t.Releve.Prochaine.IsZero() {
				l.Attendue += " (planning non lu : retard non surveillé)"
			}
		}
		if !t.Releve.Debut.IsZero() && !t.Releve.Fin.IsZero() && !t.Releve.Fin.Before(t.Releve.Debut) {
			l.Duree = dureeLisible(t.Releve.Fin.Sub(t.Releve.Debut))
			if t.Releve.Fin.Sub(t.Releve.Debut) < time.Minute {
				l.Duree = strconv.Itoa(int(t.Releve.Fin.Sub(t.Releve.Debut)/time.Second)) + " s"
			}
		}
		switch {
		case l.Etat.Probleme:
			l.Classe = "echec"
			d.NbProbleme++
		case l.Etat.Classe == "ok":
			l.Classe = "ok"
			d.NbOK++
		}
		d.Suivies = append(d.Suivies, l)
	}
	for k, x := range det {
		if suivies[k] || x.Source == taches.SourcePing || machine != "" && x.Machine != machine {
			continue
		}
		td := tacheDetectee{Detectee: *x, SourceTexte: textesSources[x.Source], Fin: x.Releve.Fin, Code: x.Releve.Code,
			Systeme: x.Source == "systemd" && timerSysteme(x.Nom), Description: descriptionTache(x.Source, x.Nom), Puces: pucesTache(x.Releve),
			Prochaine: x.Releve.Prochaine, ProchaineTexte: prochaineTexte(x.Releve.Prochaine, q.t)}
		if td.Systeme {
			d.DuSysteme = append(d.DuSysteme, td)
		} else {
			d.Detectees = append(d.Detectees, td)
		}
	}
	for _, l := range [][]tacheDetectee{d.Detectees, d.DuSysteme} {
		sort.Slice(l, func(i, j int) bool {
			if l[i].Machine != l[j].Machine {
				return l[i].Machine < l[j].Machine
			}
			return l[i].Nom < l[j].Nom
		})
	}
	if machine != "" {
		return d
	}
	if rows, err := c.db.QueryContext(ctx, `SELECT nom FROM machines WHERE statut = 'approuvee' ORDER BY nom`); err == nil {
		for rows.Next() {
			var n string
			if rows.Scan(&n) == nil {
				d.Machines = append(d.Machines, n)
			}
		}
		_ = rows.Close()
	}
	return d
}

// tachesMachine : bloc de la page d'une machine (nil : aucune tâche).
func (c *Console) tachesMachine(q *req, machine string) *donneesTaches {
	d := c.donneesTaches(q, machine)
	if len(d.Suivies) == 0 && len(d.Detectees) == 0 {
		return nil
	}
	return &d
}

// nbTachesEnProbleme : compteur du menu (tâches suivies en échec ou en
// retard, d'après les séries écrites chaque minute par le serveur).
func (c *Console) nbTachesEnProbleme(ctx context.Context, t time.Time) int {
	if c.lecteur == nil {
		return 0
	}
	ech, err := c.instant(ctx, `count((mirador_tache_echeance_timestamp_seconds < time())
		or on (instance, source, tache) (mirador_tache_suivie and on (instance, source, tache) mirador_tache_code != 0)) or vector(0)`, t)
	if err != nil || len(ech) == 0 || math.IsNaN(ech[0].Valeur) {
		return 0
	}
	return int(ech[0].Valeur)
}

func (c *Console) pageTaches(q *req) {
	d := c.donneesTaches(q, "")
	d.Ajout = q.r.URL.Query().Get("ajout") == "1"
	c.rendre(q, "taches", "Tâches planifiées", "taches", d)
}

// lireReglagesTache : fréquence, marge, gravité du formulaire.
func lireReglagesTache(f url.Values) (taches.Tache, error) {
	var t taches.Tache
	t.Nom = strings.TrimSpace(f.Get("nom"))
	t.Libelle = strings.TrimSpace(f.Get("libelle"))
	t.Gravite = f.Get("gravite")
	if t.Gravite == "" {
		t.Gravite = "warning"
	}
	per := f.Get("periode")
	switch per {
	case "", "0":
	case "autre":
		n, err := strconv.Atoi(strings.TrimSpace(f.Get("minutes")))
		if err != nil || n < 1 {
			return t, errors.New("fréquence : nombre de minutes")
		}
		t.Periode = time.Duration(n) * time.Minute
	default:
		n, err := strconv.Atoi(per)
		if err != nil {
			return t, errors.New("fréquence inconnue")
		}
		t.Periode = time.Duration(n) * time.Second
	}
	h, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(f.Get("marge")), ",", "."), 64)
	if err != nil || h < 0 || h > 168 {
		return t, errors.New("marge : nombre d'heures (0 à 168)")
	}
	t.Marge = time.Duration(h * float64(time.Hour))
	return t, nil
}

func (c *Console) postTaches(q *req) {
	ctx := q.r.Context()
	f := q.r.PostForm
	id, _ := strconv.ParseInt(f.Get("id"), 10, 64)
	var err error
	msg := ""
	switch f.Get("action") {
	case "suivre":
		var t taches.Tache
		if t, err = lireReglagesTache(f); err == nil {
			t.Source, t.Machine = f.Get("source"), f.Get("machine")
			if err = taches.Suivre(ctx, c.db, t, q.t); err == nil {
				msg = "tâche « " + t.Nom + " » suivie"
				_ = Auditer(ctx, c.db, q.qui(), "tache_suivie", t.Machine+" "+t.Nom, "", "", q.ip.String(), q.t)
			}
		}
	case "creer":
		var t taches.Tache
		if t, err = lireReglagesTache(f); err == nil {
			t.Machine = f.Get("machine")
			var jeton string
			if jeton, err = taches.CreerPing(ctx, c.db, t, q.t); err == nil {
				_ = Auditer(ctx, c.db, q.qui(), "tache_creee", t.Nom, "", "", q.ip.String(), q.t)
				d := c.donneesTaches(q, "")
				d.Nouveau = &nouveauPing{Nom: t.Nom, URL: d.Base + "/ping/" + jeton}
				c.rendre(q, "taches", "Tâches planifiées", "taches", d)
				return
			}
		}
	case "jeton":
		var jeton string
		if jeton, err = taches.NouveauJeton(ctx, c.db, id); err == nil {
			_ = Auditer(ctx, c.db, q.qui(), "tache_jeton", strconv.FormatInt(id, 10), "", "", q.ip.String(), q.t)
			d := c.donneesTaches(q, "")
			d.Nouveau = &nouveauPing{Nom: f.Get("nom"), URL: d.Base + "/ping/" + jeton}
			c.rendre(q, "taches", "Tâches planifiées", "taches", d)
			return
		}
	case "modifier":
		var t taches.Tache
		if t, err = lireReglagesTache(f); err == nil {
			t.ID, t.Source = id, f.Get("source")
			if err = taches.Modifier(ctx, c.db, t); err == nil {
				msg = "tâche modifiée"
			}
		}
	case "retirer":
		if err = taches.Retirer(ctx, c.db, id); err == nil {
			msg = "tâche retirée"
			_ = Auditer(ctx, c.db, q.qui(), "tache_retiree", f.Get("nom"), "", "", q.ip.String(), q.t)
		}
	default:
		err = errors.New("action inconnue")
	}
	if err != nil && f.Get("action") == "creer" {
		c.retour(q, "/taches?ajout=1#nouvelle", err, msg) // formulaire rouvert pour corriger
		return
	}
	c.retour(q, "/taches", err, msg)
}
