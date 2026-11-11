// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
)

func (c *Console) routesRegles() {
	m := c.mux
	m.HandleFunc("GET /regles", c.lecture(c.pageRegles))
	m.HandleFunc("GET /regles/nouvelle", c.lecture(c.pageNouvelleRegle))
	m.HandleFunc("POST /regles/nouvelle", c.admin(c.postNouvelleRegle))
	m.HandleFunc("GET /regles/modifier/{nom}", c.lecture(c.pageModifierRegle))
	m.HandleFunc("POST /regles/enregistrer", c.admin(c.postEnregistrerRegle))
	m.HandleFunc("POST /regles/action", c.admin(c.postActionRegle))
	m.HandleFunc("GET /regles/export.yaml", c.lecture(c.exportRegles))
	m.HandleFunc("POST /regles/importer", c.admin(c.postImporterRegles))
	m.HandleFunc("GET /regles/historique", c.lecture(c.pageHistoriqueRegles))
}

// --- Liste ---------------------------------------------------------------------------

type ligneRegle struct {
	R     alertes.Regle
	Sante alertes.SanteRegle
}

type donneesRegles struct {
	Groupes  map[string][]ligneRegle
	Noms     []string
	Total    int
	Actives  int
	Erreurs  int
	Modeles  []modeleRegle
	Rapport  *alertes.Rapport
	Compteur map[string]int
}

func (c *Console) pageRegles(q *req) {
	rs, err := alertes.ListerRegles(q.r.Context(), c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	sante := map[string]alertes.SanteRegle{}
	if c.opt.Alertes != nil {
		for _, s := range c.opt.Alertes.Sante() {
			sante[s.Nom] = s
		}
	}
	d := donneesRegles{Groupes: map[string][]ligneRegle{}, Modeles: modelesRegles}
	for _, r := range rs {
		d.Total++
		if r.Active {
			d.Actives++
		}
		if sante[r.Nom].Erreur != "" {
			d.Erreurs++
		}
		d.Groupes[r.Groupe] = append(d.Groupes[r.Groupe], ligneRegle{R: r, Sante: sante[r.Nom]})
	}
	for g := range d.Groupes {
		d.Noms = append(d.Noms, g)
	}
	sort.Strings(d.Noms)
	c.rendre(q, "regles", "Règles d'alerte", "regles", d)
}

// --- Modèles prêts à l'emploi (formulaire sans YAML) --------------------------------

// modeleRegle : une règle courante fabriquée à partir de quelques champs.
type modeleRegle struct {
	Cle, Titre, Aide       string
	Seuil                  float64
	Unite                  string
	Pour, Gravite          string
	Parametre, ParamDefaut string // ex. nom du service ; '' = sans paramètre
	expr                   func(filtre, param string, seuil float64) string
	resume                 string
}

const fsIgnores = `fstype!~"tmpfs|overlay|squashfs|ramfs|nsfs|fuse.lxcfs"`

var modelesRegles = []modeleRegle{
	{Cle: "cpu", Titre: "CPU élevé", Aide: "CPU utilisé (moyenne 5 min) au-dessus du seuil", Seuil: 90, Unite: "%",
		Pour: "15m", Gravite: alertes.Avertissement,
		expr: func(f, _ string, s float64) string {
			return fmt.Sprintf(`100 * (1 - avg by (instance) (rate(node_cpu_seconds_total{mode="idle"%s}[5m]))) > %g`, f, s)
		},
		resume: "CPU à {{ $value | humanize }} % sur {{ $labels.instance }}"},
	{Cle: "memoire", Titre: "Mémoire presque pleine", Aide: "mémoire utilisée au-dessus du seuil", Seuil: 92, Unite: "%",
		Pour: "10m", Gravite: alertes.Avertissement,
		expr: func(f, _ string, s float64) string {
			return fmt.Sprintf(`100 * (1 - node_memory_MemAvailable_bytes{%s} / node_memory_MemTotal_bytes{%s}) > %g`,
				strings.TrimPrefix(f, ","), strings.TrimPrefix(f, ","), s)
		},
		resume: "Mémoire utilisée à {{ $value | humanize }} % sur {{ $labels.instance }}"},
	{Cle: "disque", Titre: "Disque presque plein", Aide: "système de fichiers rempli au-delà du seuil", Seuil: 90, Unite: "%",
		Pour: "15m", Gravite: alertes.Avertissement,
		expr: func(f, _ string, s float64) string {
			return fmt.Sprintf(`100 * (1 - node_filesystem_avail_bytes{%s%s} / node_filesystem_size_bytes{%s%s}) > %g`,
				fsIgnores, f, fsIgnores, f, s)
		},
		resume: "{{ $labels.mountpoint }} rempli à {{ $value | humanize }} % sur {{ $labels.instance }}"},
	{Cle: "service", Titre: "Service systemd en échec", Aide: "unité systemd à l'état « failed »", Unite: "",
		Pour: "5m", Gravite: alertes.Avertissement, Parametre: "Nom du service (expression, ex. nginx.service ou .+)", ParamDefaut: ".+",
		expr: func(f, p string, _ float64) string {
			return fmt.Sprintf(`node_systemd_unit_state{state="failed",name=~"%s"%s} == 1`, echapperPromQL(p), f)
		},
		resume: "Service {{ $labels.name }} en échec sur {{ $labels.instance }}"},
	{Cle: "conteneur", Titre: "Conteneur arrêté", Aide: "conteneur Docker qui devrait tourner (always / unless-stopped)",
		Pour: "5m", Gravite: alertes.Avertissement, Parametre: "Nom du conteneur (expression)", ParamDefaut: ".+",
		expr: func(f, p string, _ float64) string {
			return fmt.Sprintf(`node_docker_container_running{restart_policy=~"always|unless-stopped",name=~"%s"%s} == 0`, echapperPromQL(p), f)
		},
		resume: "Conteneur {{ $labels.name }} arrêté sur {{ $labels.instance }}"},
	{Cle: "certificat", Titre: "Certificat bientôt expiré", Aide: "jours restants sous le seuil", Seuil: 14, Unite: "jours",
		Pour: "1h", Gravite: alertes.Avertissement,
		expr: func(f, _ string, s float64) string {
			return fmt.Sprintf(`(ssl_cert_not_after_timestamp_seconds{%s} - time()) / 86400 < %g`, strings.TrimPrefix(f, ","), s)
		},
		resume: "Certificat {{ $labels.cn }} expire dans {{ $value | humanize }} jours ({{ $labels.instance }})"},
	{Cle: "muette", Titre: "Machine muette", Aide: "plus aucune donnée depuis le seuil", Seuil: 5, Unite: "minutes",
		Pour: "0s", Gravite: alertes.Critique,
		expr: func(f, _ string, s float64) string {
			return fmt.Sprintf(`lag(node_uname_info{%s}[1d]) > %g`, strings.TrimPrefix(f, ","), s*60)
		},
		resume: "{{ $labels.instance }} n'envoie plus rien depuis {{ $value | humanizeDuration }}"},
}

func modele(cle string) *modeleRegle {
	for i := range modelesRegles {
		if modelesRegles[i].Cle == cle {
			return &modelesRegles[i]
		}
	}
	return nil
}

var reValeurSimple = regexp.MustCompile(`^[A-Za-z0-9._-]{1,63}$`)

// filtreCible : « », machine:NOM, site:X, env:X, groupe:G → morceau de
// sélecteur (commençant par une virgule). Un groupe vit dans la base, pas
// dans les données : il est remplacé par la liste de ses machines au moment
// de l'enregistrement.
func (c *Console) filtreCible(q *req, cible string) (string, error) {
	if cible == "" || cible == "toutes" {
		return "", nil
	}
	genre, v, ok := strings.Cut(cible, ":")
	if !ok || !reValeurSimple.MatchString(v) {
		return "", errors.New("cible : toutes, machine:NOM, site:X, env:X ou groupe:G")
	}
	switch genre {
	case "machine":
		return `,instance="` + v + `"`, nil
	case "site", "env":
		return "," + genre + `="` + v + `"`, nil
	case "groupe":
		ms, err := flotte.ListerMachines(q.r.Context(), c.db)
		if err != nil {
			return "", err
		}
		var noms []string
		for _, m := range ms {
			if m.Groupe == v && m.Statut == "approuvee" {
				noms = append(noms, regexp.QuoteMeta(m.Nom))
			}
		}
		if len(noms) == 0 {
			return "", fmt.Errorf("groupe %s : aucune machine", v)
		}
		return `,instance=~"` + echapperPromQL(strings.Join(noms, "|")) + `"`, nil
	}
	return "", errors.New("cible inconnue")
}

type donneesFormRegle struct {
	R        alertes.Regle
	Nouvelle bool
	Test     *resultatTest
	Modeles  []modeleRegle
	Machines []flotte.InfoMachine
	Etiq     string // étiquettes en texte « nom=valeur » par ligne
}

func (c *Console) pageNouvelleRegle(q *req) {
	ms, _ := flotte.ListerMachines(q.r.Context(), c.db)
	r := alertes.Regle{Groupe: "perso", Gravite: alertes.Avertissement, Active: true, Origine: "interface",
		Intervalle: alertes.Duree(time.Minute)}
	c.rendre(q, "regle-nouvelle", "Nouvelle règle", "regles", donneesFormRegle{R: r, Nouvelle: true, Modeles: modelesRegles, Machines: ms})
}

// postNouvelleRegle : le formulaire simple fabrique la règle, puis la page
// de modification s'ouvre avec la règle pré-remplie (rien n'est enregistré
// tant qu'on n'a pas cliqué « enregistrer »).
func (c *Console) postNouvelleRegle(q *req) {
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	err := func() error {
		m := modele(f("modele"))
		if m == nil {
			return errors.New("modèle inconnu")
		}
		filtre, err := c.filtreCible(q, f("cible"))
		if err != nil {
			return err
		}
		seuil := m.Seuil
		if m.Unite != "" {
			if seuil, err = strconv.ParseFloat(strings.Replace(f("seuil"), ",", ".", 1), 64); err != nil {
				return errors.New("seuil : nombre attendu")
			}
		}
		param := f("parametre")
		if param == "" {
			param = m.ParamDefaut
		}
		if len(param) > 200 {
			return errors.New("paramètre trop long")
		}
		r := alertes.Regle{Nom: f("nom"), Groupe: "perso", Intervalle: alertes.Duree(time.Minute), Expr: m.expr(filtre, param, seuil),
			Gravite: f("gravite"), Resume: m.resume, Active: true, Origine: "modèle " + m.Cle, Description: m.Aide}
		if r.Pour, err = alertes.LireDuree(f("pour")); err != nil {
			return errors.New("durée « pendant » illisible (ex. 5m)")
		}
		if _, err := r.Valider(); err != nil {
			return err
		}
		ms, _ := flotte.ListerMachines(q.r.Context(), c.db)
		d := donneesFormRegle{R: r, Nouvelle: true, Machines: ms}
		d.Test = c.testerRegle(q, r)
		c.flash(q, "règle préparée et testée sur 24 h : vérifier puis enregistrer", "")
		c.rendre(q, "regle", "Nouvelle règle", "regles", d)
		return errRedirige
	}()
	if errors.Is(err, errRedirige) {
		return
	}
	c.retour(q, "/regles/nouvelle", err, "")
}

func (c *Console) pageModifierRegle(q *req) {
	rs, err := alertes.ListerRegles(q.r.Context(), c.db)
	if err != nil {
		c.retour(q, "/regles", err, "")
		return
	}
	for _, r := range rs {
		if r.Nom == q.r.PathValue("nom") {
			c.rendre(q, "regle", r.Nom, "regles", donneesFormRegle{R: r, Etiq: texteEtiquettes(r.Etiquettes)})
			return
		}
	}
	c.rendreCode(q, http.StatusNotFound, "introuvable", "Règle introuvable", "regles", nil)
}

func texteEtiquettes(m map[string]string) string {
	var lignes []string
	for k, v := range m {
		lignes = append(lignes, k+"="+v)
	}
	sort.Strings(lignes)
	return strings.Join(lignes, "\n")
}

// regleDuFormulaire : tous les champs d'une règle.
func regleDuFormulaire(q *req) (alertes.Regle, error) {
	f := func(k string) string { return strings.TrimSpace(q.r.PostFormValue(k)) }
	r := alertes.Regle{Nom: f("nom"), Groupe: f("groupe"), Expr: f("expr"), Gravite: f("gravite"), Resume: f("resume"),
		Description: f("description"), Active: f("active") != "", Origine: f("origine"), Note: f("note")}
	var e1, e2, e3 error
	r.Intervalle, e1 = alertes.LireDuree(f("intervalle"))
	if f("pour") != "" {
		r.Pour, e2 = alertes.LireDuree(f("pour"))
	}
	if f("garder") != "" {
		r.Garder, e3 = alertes.LireDuree(f("garder"))
	}
	if err := errors.Join(e1, e2, e3); err != nil {
		return r, errors.New("durées illisibles (ex. 30s, 5m, 1h)")
	}
	for _, l := range strings.Split(f("etiquettes"), "\n") {
		if l = strings.TrimSpace(l); l == "" {
			continue
		}
		k, v, ok := strings.Cut(l, "=")
		if !ok {
			return r, fmt.Errorf("étiquette %q : nom=valeur attendu", l)
		}
		if r.Etiquettes == nil {
			r.Etiquettes = map[string]string{}
		}
		r.Etiquettes[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	r.Defauts()
	_, err := r.Valider()
	return r, err
}

// postEnregistrerRegle : « tester » (rien n'est enregistré) ou « enregistrer ».
func (c *Console) postEnregistrerRegle(q *req) {
	r, err := regleDuFormulaire(q)
	d := donneesFormRegle{R: r, Nouvelle: q.r.PostFormValue("nouvelle") != "", Etiq: q.r.PostFormValue("etiquettes")}
	if err != nil {
		c.flash(q, "", err.Error())
		c.rendre(q, "regle", "Règle", "regles", d)
		return
	}
	if q.r.PostFormValue("bouton") == "tester" {
		d.Test = c.testerRegle(q, r)
		c.rendre(q, "regle", r.Nom, "regles", d)
		return
	}
	if d.Nouvelle {
		if rs, err := alertes.ListerRegles(q.r.Context(), c.db); err == nil {
			for _, x := range rs {
				if x.Nom == r.Nom {
					c.flash(q, "", "une règle porte déjà ce nom")
					c.rendre(q, "regle", r.Nom, "regles", d)
					return
				}
			}
		}
	}
	_, err = alertes.Enregistrer(q.r.Context(), c.db, []alertes.Regle{r}, q.qui(), q.t)
	c.retour(q, "/regles", err, "règle "+r.Nom+" enregistrée (appliquée en 10 s au plus)")
}

func (c *Console) postActionRegle(q *req) {
	nom := q.r.PostFormValue("nom")
	var err error
	ok := "fait"
	switch q.r.PostFormValue("action") {
	case "activer":
		err = alertes.Activer(q.r.Context(), c.db, nom, true, q.qui(), q.t)
		ok = nom + " activée"
	case "desactiver":
		err = alertes.Activer(q.r.Context(), c.db, nom, false, q.qui(), q.t)
		ok = nom + " désactivée"
	case "supprimer":
		if err = c.verifierCode(q); err == nil {
			err = alertes.Supprimer(q.r.Context(), c.db, nom, q.qui(), q.t)
			ok = nom + " supprimée"
		}
	default:
		err = errors.New("action inconnue")
	}
	c.retour(q, "/regles", err, ok)
}

// --- « Tester la règle » : déclenchements sur les dernières 24 h --------------------

type declenchement struct {
	Serie            string
	Machine          string
	Fois             int
	Total            time.Duration
	Premier, Dernier time.Time
	ValeurMax        float64
}

type resultatTest struct {
	Erreur         string
	Declenchements []declenchement
	Series         int
	Pas            time.Duration
}

// testerRegle : la règle est évaluée sur 24 h (un point par intervalle, une
// minute au moins). Une alerte « aurait été envoyée » quand la condition est
// restée vraie au moins « pour » de suite.
func (c *Console) testerRegle(q *req, r alertes.Regle) *resultatTest {
	res := &resultatTest{}
	if c.lecteur == nil {
		res.Erreur = "métriques indisponibles"
		return res
	}
	pas := time.Duration(r.Intervalle)
	if pas < time.Minute {
		pas = time.Minute
	}
	fin := q.t.Truncate(pas)
	courbes, pasReel, err := c.lecteur.Plage(q.r.Context(), r.Expr, fin.Add(-24*time.Hour), fin, pas)
	if err != nil {
		res.Erreur = err.Error()
		return res
	}
	res.Pas, res.Series = pasReel, len(courbes)
	pour := time.Duration(r.Pour)
	for _, cb := range courbes {
		d := declenchement{Serie: nomSerie(cb.Etiquettes), Machine: cb.Etiquettes.Get("instance")}
		var debut, prec int64 = -1, -1
		fermer := func() {
			if debut < 0 {
				return
			}
			duree := time.Duration(prec-debut)*time.Millisecond + pasReel
			if duree > pour || (pour == 0 && duree > 0) {
				d.Fois++
				d.Total += duree
				if d.Premier.IsZero() {
					d.Premier = time.UnixMilli(debut)
				}
				d.Dernier = time.UnixMilli(prec)
			}
		}
		for i, t := range cb.T {
			if debut >= 0 && t-prec > pasReel.Milliseconds() {
				fermer()
				debut = -1
			}
			if debut < 0 {
				debut = t
			}
			prec = t
			if cb.V[i] > d.ValeurMax || i == 0 {
				d.ValeurMax = cb.V[i]
			}
		}
		fermer()
		if d.Fois > 0 {
			res.Declenchements = append(res.Declenchements, d)
		}
	}
	sort.Slice(res.Declenchements, func(i, j int) bool { return res.Declenchements[i].Total > res.Declenchements[j].Total })
	if len(res.Declenchements) > 200 {
		res.Declenchements = res.Declenchements[:200]
	}
	return res
}

// --- Import / export YAML --------------------------------------------------------------

func (c *Console) exportRegles(q *req) {
	rs, err := alertes.ListerRegles(q.r.Context(), c.db)
	if err != nil {
		http.Error(q.w, err.Error(), http.StatusInternalServerError)
		return
	}
	var b bytes.Buffer
	if err := alertes.Exporter(&b, rs); err != nil {
		http.Error(q.w, err.Error(), http.StatusInternalServerError)
		return
	}
	q.w.Header().Set("Content-Type", "application/yaml; charset=utf-8")
	q.w.Header().Set("Content-Disposition", `attachment; filename="mirador-regles-`+q.t.Format("2006-01-02")+`.yaml"`)
	_, _ = q.w.Write(b.Bytes())
}

// postImporterRegles : fichier YAML (mirador ou vmalert). « Essai » montre
// le rapport sans rien enregistrer.
func (c *Console) postImporterRegles(q *req) {
	err := func() error {
		if err := q.r.ParseMultipartForm(1 << 20); err != nil {
			return errors.New("fichier illisible (1 Mio au plus)")
		}
		defer q.r.MultipartForm.RemoveAll() //nolint:errcheck // rien sur disque sous 1 Mio
		fh, en, err := q.r.FormFile("fichier")
		if err != nil {
			return errors.New("aucun fichier")
		}
		defer fh.Close()
		rap, err := alertes.Importer(io.LimitReader(fh, 2<<20), en.Filename)
		if err != nil {
			return err
		}
		if q.r.PostFormValue("desactivees") != "" {
			for i := range rap.Regles {
				rap.Regles[i].Active = false
			}
		}
		d := donneesRegles{Rapport: rap, Compteur: rap.Compter(), Groupes: map[string][]ligneRegle{}, Modeles: modelesRegles}
		if q.r.PostFormValue("essai") == "" {
			n, err := alertes.Enregistrer(q.r.Context(), c.db, rap.Regles, q.qui(), q.t)
			if err != nil {
				return err
			}
			c.flash(q, fmt.Sprintf("%d règle(s) enregistrée(s) ou changée(s)", n), "")
		} else {
			c.flash(q, "essai : rien n'a été enregistré", "")
		}
		c.rendre(q, "regles-import", "Rapport d'import", "regles", d)
		return errRedirige
	}()
	if errors.Is(err, errRedirige) {
		return
	}
	c.retour(q, "/regles", err, "")
}

// --- Historique des modifications des règles ----------------------------------------

type ligneAudit struct {
	ID, Le                                    int64
	Qui, Action, Objet, Avant, Apres, Adresse string
}

func (c *Console) pageHistoriqueRegles(q *req) {
	ls, err := c.lireAudit(q, "action LIKE 'regle%'", nil, 300)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	c.rendre(q, "regles-historique", "Modifications des règles", "regles", ls)
}

// lireAudit : lignes du journal d'audit (condition fixée dans le code).
func (c *Console) lireAudit(q *req, condition string, args []any, limite int) ([]ligneAudit, error) {
	return c.lireAuditID(q, condition, args, limite)
}

// lireAuditID : la condition est toujours écrite dans le code (jamais par
// l'utilisateur) ; les valeurs passent en paramètres SQL.
func (c *Console) lireAuditID(q *req, condition string, args []any, limite int) ([]ligneAudit, error) {
	requete := `SELECT id, le, qui, action, objet, avant, apres, adresse FROM audit WHERE ` + condition + ` ORDER BY id DESC LIMIT ?` // #nosec G202 -- condition écrite dans le code ; valeurs en paramètres
	rows, err := c.db.QueryContext(q.r.Context(), requete, append(args, limite)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ligneAudit
	for rows.Next() {
		var l ligneAudit
		if err := rows.Scan(&l.ID, &l.Le, &l.Qui, &l.Action, &l.Objet, &l.Avant, &l.Apres, &l.Adresse); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
