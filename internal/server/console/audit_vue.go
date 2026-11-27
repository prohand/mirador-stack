// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"encoding/csv"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Journal d'audit lisible (SPEC § 28, maquette validée le 15/11) : actions
// en français rangées par catégorie, seulement ce qui a changé, lignes
// groupées par jour, filtres (catégorie, qui, période, recherche, actions
// automatiques, sensibles), liens vers les objets, export CSV.

// categorieAudit : une catégorie et les débuts de codes d'action qu'elle
// regroupe (filtre SQL : action LIKE 'début%').
type categorieAudit struct {
	Cle, Titre string
	Prefixes   []string
}

var categoriesAudit = []categorieAudit{
	{"securite", "Sécurité", []string{"reglages_securite", "confirmer", "annuler_non_confirme", "mode_admin", "session", "adresses"}},
	{"comptes", "Comptes", []string{"utilisateur", "compte", "passkey", "code_secours"}},
	{"machines", "Machines", []string{"machine", "equipement", "approuvee", "en_attente", "enrolement", "jeton", "token", "config", "surveillance", "collecteur", "certificat_syslog", "certsyslog", "agent", "renomm", "supprim"}},
	{"modeles", "Modèles", []string{"modele"}},
	{"regles", "Règles", []string{"regle", "reglages_vmalert"}},
	{"alertes", "Alertes", []string{"silence", "maintenance", "inhibition", "dependance", "acquittement"}},
	{"notif", "Notifications", []string{"reglages_notifications", "olvid", "canal", "notif"}},
	{"maj", "Mises à jour", []string{"cles_publiques", "depot_versions", "version", "maj_"}},
	{"reglages", "Réglages", []string{"reglages", "conservation", "sites_types", "sauvegarde", "interface", "export", "import", "audit_purge", "geoip"}},
}

// categorieDe : catégorie d'un code d'action (préfixe le plus long).
func categorieDe(action string) categorieAudit {
	meilleur, long := categorieAudit{Cle: "autres", Titre: "Autres"}, 0
	for _, c := range categoriesAudit {
		for _, p := range c.Prefixes {
			if strings.HasPrefix(action, p) && len(p) > long {
				meilleur, long = c, len(p)
			}
		}
	}
	return meilleur
}

// actionsSensibles : codes d'action (début) signalés « sensible ».
var actionsSensibles = []string{"reglages_securite", "confirmer", "annuler_non_confirme", "mode_admin", "utilisateur", "passkey",
	"code_secours", "cles_publiques", "depot_versions", "token_manuel", "export_configuration", "audit_purge"}

func estSensible(action string) bool {
	for _, p := range actionsSensibles {
		if strings.HasPrefix(action, p) {
			return true
		}
	}
	return false
}

// phrasesAudit : phrase en français de chaque code connu ; %s = l'objet.
var phrasesAudit = map[string]string{ // #nosec G101 -- libellés d'actions (« token_remis »…), aucun secret
	"regle":                            "Règle %s créée ou modifiée",
	"regle_supprimee":                  "Règle %s supprimée",
	"reglages_vmalert":                 "Réglages de la comparaison vmalert changés",
	"silence":                          "Silence posé (%s)",
	"silence_annule":                   "Silence %s annulé",
	"maintenance":                      "Maintenance %s créée ou modifiée",
	"inhibition":                       "Inhibition %s créée ou modifiée",
	"dependance":                       "Dépendance %s créée ou modifiée",
	"acquittement":                     "Alerte %s acquittée",
	"utilisateur_creer":                "Compte %s créé",
	"utilisateur_2fa":                  "Double authentification du compte %s créée ou changée",
	"utilisateur_codes_secours":        "Codes de secours du compte %s recréés",
	"utilisateur_code_secours_utilise": "Code de secours utilisé pour le compte %s",
	"utilisateur_passkey_ajoutee":      "Passkey ajoutée au compte %s",
	"utilisateur_passkey_supprimee":    "Passkey supprimée du compte %s",
	"reglages_securite":                "Réglages de sécurité de l'interface changés",
	"confirmer":                        "Changement %s confirmé",
	"annuler_non_confirme":             "Changement %s annulé (pas confirmé à temps)",
	"reglages_notifications":           "Réglages des notifications changés",
	"olvid_installer":                  "Daemon Olvid installé ou relié",
	"reglages_interface":               "Réglages de l'interface changés",
	"reglages_sauvegarde":              "Réglages des sauvegardes changés",
	"conservation_reduction_appliquee": "Réduction de la conservation appliquée",
	"conservation_reduction_annulee":   "Réduction de la conservation annulée",
	"sites_types":                      "Listes des sites, types et groupes changées",
	"export_configuration":             "Configuration exportée",
	"audit_purge":                      "Ancien journal d'audit purgé",
	"cles_publiques":                   "Clés publiques de signature changées",
	"depot_versions":                   "Dépôt des versions changé",
	"version_cible":                    "Version cible des agents : %s",
	"version_publiee":                  "Version %s publiée",
	"version_supprimee":                "Version %s supprimée",
	"versions_gardees":                 "Nombre de versions gardées changé",
	"maj_fenetre":                      "Fenêtre de mise à jour changée",
	"maj_forcee":                       "Mise à jour demandée maintenant",
	"config_agent":                     "Configuration des agents changée (%s)",
	"jeton_cree":                       "Jeton d'enrôlement créé",
	"token_manuel":                     "Jeton d'une machine créé à la main (%s)",
	"token_remis":                      "Jeton remis à la machine %s",
	"enrolement_demande":               "Demande d'enrôlement de %s",
	"equipement_cree":                  "Équipement %s créé",
	"equipement_rattache":              "Équipement %s rattaché à un collecteur",
	"equipement_detache":               "Équipement %s détaché de son collecteur",
	"certificat_syslog_demande":        "Certificat syslog redemandé (%s)",
	"certsyslog_reglages":              "Réglages du certificat syslog changés (%s)",
	"modele_perso":                     "Modèle personnel %s enregistré",
	"modele_perso_supprime":            "Modèle personnel %s supprimé",
	"modele_services":                  "Services attendus du modèle %s changés",
	"modele_actif":                     "Modèle appliqué (%s)",
	"modele_ignore":                    "Modèle laissé de côté (%s)",
	"modele_retire":                    "Modèle retiré (%s)",
	"modeles_auto":                     "Application automatique des modèles changée",
	"approuvee":                        "Machine %s approuvée",
	"en_attente":                       "Machine %s remise en attente",
}

// phraseAudit : phrase lisible ; code inconnu : code « humanisé ».
func phraseAudit(action, objet string) string {
	if p, ok := phrasesAudit[action]; ok {
		if strings.Contains(p, "%s") {
			if objet == "" {
				return strings.TrimSpace(strings.NewReplacer(" %s", "", "(%s)", "", " : %s", "").Replace(p))
			}
			return fmt.Sprintf(p, objet)
		}
		return p
	}
	t := majusculeDebut(strings.ReplaceAll(action, "_", " "))
	if objet != "" {
		t += " — " + objet
	}
	return t
}

// changement : une valeur changée (ancienne → nouvelle).
type changement struct{ Cle, Avant, Apres string }

// clesChangees : clés changées entre deux documents JSON (aplatis), sinon
// les deux textes ; 8 au plus.
func clesChangees(avant, apres string) []changement {
	ja, okA := aplatir(avant)
	jb, okB := aplatir(apres)
	var out []changement
	switch {
	case okA && okB, okA && apres == "", avant == "" && okB:
		cles := map[string]bool{}
		for k := range ja {
			cles[k] = true
		}
		for k := range jb {
			cles[k] = true
		}
		var tri []string
		for k := range cles {
			if ja[k] != jb[k] {
				tri = append(tri, k)
			}
		}
		sort.Strings(tri)
		for _, k := range tri {
			out = append(out, changement{Cle: k, Avant: court(ja[k]), Apres: court(jb[k])})
		}
	case avant != apres:
		out = append(out, changement{Avant: court(resumeJSON(avant)), Apres: court(resumeJSON(apres))})
	}
	if len(out) > 8 {
		out = append(out[:8], changement{Cle: "…", Apres: fmt.Sprintf("%d autres", len(out)-8)})
	}
	return out
}

// resumeJSON : liste JSON en texte court (« cron.service, ssh.service », ou
// « 3 éléments » pour une liste d'objets) ; « null » → rien ; autre texte
// inchangé (audit lisible, SPEC § 75).
func resumeJSON(s string) string {
	t := strings.TrimSpace(s)
	if t == "null" {
		return ""
	}
	if !strings.HasPrefix(t, "[") {
		return s
	}
	var l []any
	if json.Unmarshal([]byte(t), &l) != nil {
		return s
	}
	if r, ok := aplatir(`{"l":` + t + `}`); ok {
		return r["l"]
	}
	return s
}

// aplatir : document JSON objet → clés « a.b » et valeurs en texte.
func aplatir(s string) (map[string]string, bool) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return nil, false
	}
	var v map[string]any
	if json.Unmarshal([]byte(s), &v) != nil {
		return nil, false
	}
	out := map[string]string{}
	var marcher func(p string, x any)
	marcher = func(p string, x any) {
		switch t := x.(type) {
		case map[string]any:
			for k, y := range t {
				if p != "" {
					k = p + "." + k
				}
				marcher(k, y)
			}
		case nil:
		case []any:
			// Liste : textes à la suite ; objets comptés (au lieu du JSON brut).
			var mots []string
			for _, y := range t {
				if s, ok := y.(string); ok {
					mots = append(mots, s)
				}
			}
			switch {
			case len(t) == 0:
				out[p] = "(vide)"
			case len(mots) == len(t):
				out[p] = strings.Join(mots, ", ")
			case len(t) == 1:
				out[p] = "1 élément"
			default:
				out[p] = strconv.Itoa(len(t)) + " éléments"
			}
		default:
			b, _ := json.Marshal(t)
			out[p] = strings.Trim(string(b), `"`)
		}
	}
	marcher("", v)
	return out, true
}

func court(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if r := []rune(s); len(r) > 80 {
		return string(r[:80]) + "…"
	}
	return s
}

// ligneAuditVue : une ligne prête à afficher.
type ligneAuditVue struct {
	ligneAudit
	Heure, Jour string // Jour : rempli sur la première ligne du jour
	Cat         categorieAudit
	Phrase      string
	Lien        string // page de l'objet
	Sensible    bool
	Changes     []changement
}

var reNomSimple = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// lienAudit : page de l'objet d'une action.
func lienAudit(action, objet string) string {
	if !reNomSimple.MatchString(objet) {
		return ""
	}
	switch {
	case strings.HasPrefix(action, "regle"):
		return "/regles/modifier/" + url.PathEscape(objet)
	case strings.HasPrefix(action, "modele_perso"), action == "modele_services":
		return "/modeles#" + url.PathEscape(objet)
	case strings.HasPrefix(action, "utilisateur"):
		return "/utilisateurs"
	case strings.HasPrefix(action, "equipement"), action == "approuvee", action == "en_attente", action == "token_remis":
		return "/machines/" + url.PathEscape(objet)
	}
	return ""
}

type donneesAudit struct {
	Lignes     []ligneAuditVue
	Categories []categorieAudit
	Quis       []string
	F          filtreAudit
	Masquees   int // actions automatiques masquées sur la période
	Suivant    int64
	Requete    string // filtres, pour « plus anciennes » et l'export
}

// filtreAudit : filtres de la page (paramètres d'URL).
type filtreAudit struct {
	Categorie, Qui, Periode, Recherche string
	Auto, Sensibles                    bool
	Avant                              int64
}

var periodesAudit = map[string]time.Duration{"24h": 24 * time.Hour, "7j": 7 * 24 * time.Hour, "30j": 30 * 24 * time.Hour, "tout": 0}

func lireFiltreAudit(v url.Values) filtreAudit {
	f := filtreAudit{Categorie: v.Get("categorie"), Qui: v.Get("qui"), Periode: v.Get("periode"), Recherche: strings.TrimSpace(v.Get("q")),
		Auto: v.Get("auto") != "", Sensibles: v.Get("sensibles") != ""}
	if _, ok := periodesAudit[f.Periode]; !ok {
		f.Periode = "7j"
	}
	f.Recherche = couperUTF8(f.Recherche, 100)
	f.Avant, _ = strconv.ParseInt(v.Get("avant"), 10, 64)
	return f
}

func (f filtreAudit) requete() string {
	v := url.Values{}
	for k, x := range map[string]string{"categorie": f.Categorie, "qui": f.Qui, "periode": f.Periode, "q": f.Recherche} {
		if x != "" {
			v.Set(k, x)
		}
	}
	if f.Auto {
		v.Set("auto", "1")
	}
	if f.Sensibles {
		v.Set("sensibles", "1")
	}
	return v.Encode()
}

// quisAuto : auteurs des actions automatiques (masquées par défaut).
const condAuto = "(qui = 'mirador' OR qui = 'auto' OR qui LIKE 'agent%')"

// condition : clause SQL du filtre (valeurs en paramètres).
func (f filtreAudit) condition(t time.Time, avecAuto bool) (string, []any) {
	cond, args := []string{"1 = 1"}, []any{}
	ou := func(prefixes []string) {
		var p []string
		for _, x := range prefixes {
			p = append(p, "action LIKE ?")
			args = append(args, x+"%")
		}
		cond = append(cond, "("+strings.Join(p, " OR ")+")")
	}
	for _, c := range categoriesAudit {
		if c.Cle == f.Categorie {
			ou(c.Prefixes)
		}
	}
	if f.Sensibles {
		ou(actionsSensibles)
	}
	if f.Qui != "" {
		cond = append(cond, "qui = ?")
		args = append(args, f.Qui)
	}
	if d := periodesAudit[f.Periode]; d > 0 {
		cond = append(cond, "le >= ?")
		args = append(args, t.Add(-d).Unix())
	}
	if f.Recherche != "" {
		// « _ » et « % » échappés (pas supprimés) : « mode_admin » doit trouver
		// l'action mode_admin.
		r := "%" + echapperLike(f.Recherche) + "%"
		cond = append(cond, `(objet LIKE ? ESCAPE '\' OR action LIKE ? ESCAPE '\' OR qui LIKE ? ESCAPE '\' OR apres LIKE ? ESCAPE '\')`)
		args = append(args, r, r, r, r)
	}
	if !f.Auto && !avecAuto {
		cond = append(cond, "NOT "+condAuto)
	}
	if f.Avant > 0 {
		cond = append(cond, "id < ?")
		args = append(args, f.Avant)
	}
	return strings.Join(cond, " AND "), args
}

func (c *Console) pageAudit(q *req) {
	f := lireFiltreAudit(q.r.URL.Query())
	d := donneesAudit{Categories: categoriesAudit, F: f, Requete: f.requete()}
	cond, args := f.condition(q.t, false)
	ls, err := c.lireAuditID(q, cond, args, 200)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	jour := ""
	noms := c.nomsMachinesParID(q.r.Context())
	for _, l := range ls {
		t := time.Unix(l.Le, 0).In(fuseau.Load())
		v := ligneAuditVue{ligneAudit: l, Heure: t.Format("15:04"), Cat: categorieDe(l.Action), Phrase: remplacerPortees(phraseAudit(l.Action, l.Objet), noms),
			Lien: lienAudit(l.Action, l.Objet), Sensible: estSensible(l.Action), Changes: clesChangees(l.Avant, l.Apres)}
		for i := range v.Changes { // « machine:2 » → nom, aussi dans les valeurs
			v.Changes[i].Avant, v.Changes[i].Apres = remplacerPortees(v.Changes[i].Avant, noms), remplacerPortees(v.Changes[i].Apres, noms)
		}
		if j := libelleJour(t, q.t); j != jour {
			v.Jour, jour = j, j
		}
		d.Lignes = append(d.Lignes, v)
	}
	if len(ls) == 200 {
		d.Suivant = ls[len(ls)-1].ID
	}
	if !f.Auto {
		fa := f
		fa.Avant = 0
		cAuto, aAuto := fa.condition(q.t, true)
		_ = c.db.QueryRowContext(q.r.Context(), `SELECT COUNT(*) FROM audit WHERE `+cAuto+` AND `+condAuto, aAuto...).Scan(&d.Masquees) // #nosec G202 -- conditions écrites dans le code ; valeurs en paramètres
	}
	if rows, err := c.db.QueryContext(q.r.Context(), `SELECT DISTINCT qui FROM audit ORDER BY qui LIMIT 100`); err == nil {
		defer rows.Close()
		for rows.Next() {
			var s string
			if rows.Scan(&s) == nil {
				d.Quis = append(d.Quis, s)
			}
		}
	}
	c.rendre(q, "audit", "Journal d'audit", "audit", d)
}

// libelleJour : « Aujourd'hui · samedi 15/11 », « Hier · … » ou
// « jeudi 13/11/2026 ».
func libelleJour(t, maintenant time.Time) string {
	m := maintenant.In(t.Location())
	jour := [...]string{"dimanche", "lundi", "mardi", "mercredi", "jeudi", "vendredi", "samedi"}[t.Weekday()]
	a, b := t.Format("2006-01-02"), m.Format("2006-01-02")
	switch {
	case a == b:
		return "Aujourd'hui · " + jour + " " + t.Format("02/01")
	case a == m.AddDate(0, 0, -1).Format("2006-01-02"):
		return "Hier · " + jour + " " + t.Format("02/01")
	}
	return jour + " " + t.Format("02/01/2006")
}

// csvAudit : la vue filtrée en CSV (10 000 lignes au plus).
func (c *Console) csvAudit(q *req) {
	f := lireFiltreAudit(q.r.URL.Query())
	cond, args := f.condition(q.t, false)
	ls, err := c.lireAuditID(q, cond, args, 10000)
	if err != nil {
		c.retour(q, "/audit", err, "")
		return
	}
	q.w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	q.w.Header().Set("Content-Disposition", `attachment; filename="mirador-audit.csv"`)
	w := csv.NewWriter(q.w)
	_ = w.Write([]string{"date", "qui", "adresse", "categorie", "action", "objet", "phrase", "avant", "apres"})
	for _, l := range ls {
		_ = w.Write([]string{time.Unix(l.Le, 0).In(fuseau.Load()).Format("2006-01-02 15:04:05"), l.Qui, l.Adresse, categorieDe(l.Action).Titre,
			l.Action, csvSur(l.Objet), csvSur(phraseAudit(l.Action, l.Objet)), csvSur(l.Avant), csvSur(l.Apres)})
	}
	w.Flush()
}

// csvSur : pas de formule exécutée à l'ouverture dans un tableur.
func csvSur(s string) string {
	if s != "" && strings.ContainsRune("=+-@\t\r", rune(s[0])) {
		return "'" + s
	}
	return s
}
