// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/agent/logs"
	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/sauvegarde"
)

func (c *Console) routesReglages() {
	m := c.mux
	m.HandleFunc("GET /reglages", c.lecture(func(q *req) { c.rendre(q, "reglages", "Réglages", "reglages", nil) }))
	m.HandleFunc("GET /reglages/notifications", c.lecture(c.pageNotifications))
	m.HandleFunc("POST /reglages/notifications", c.admin(c.postNotifications))
	m.HandleFunc("GET /reglages/conservation", c.lecture(c.pageConservation))
	m.HandleFunc("POST /reglages/conservation", c.admin(c.postConservation))
	m.HandleFunc("GET /reglages/sauvegardes", c.lecture(c.pageSauvegardes))
	m.HandleFunc("POST /reglages/sauvegardes", c.admin(c.postSauvegardes))
	m.HandleFunc("GET /reglages/sites", c.lecture(c.pageSites))
	m.HandleFunc("POST /reglages/sites", c.admin(c.postSites))
	m.HandleFunc("GET /reglages/agents", c.lecture(c.pageAgents))
	m.HandleFunc("POST /reglages/agents", c.admin(c.postAgents))
	m.HandleFunc("POST /reglages/agents/masquage", c.lecture(c.postTestMasquage))
	m.HandleFunc("GET /reglages/interface", c.lecture(c.pageInterface))
	m.HandleFunc("POST /reglages/interface", c.admin(c.postInterface))
	m.HandleFunc("GET /reglages/olvid", c.admin(c.pageOlvid)) // lien d'invitation, contacts : admins seulement
	m.HandleFunc("POST /reglages/olvid", c.admin(c.postOlvid))
	m.HandleFunc("GET /reglages/olvid/installer", c.admin(c.pageInstallerOlvid))
	m.HandleFunc("POST /reglages/olvid/installer", c.admin(c.postInstallerOlvid))
	m.HandleFunc("GET /audit", c.lecture(c.pageAudit))
	m.HandleFunc("GET /reglages/export.yaml", c.admin(c.exportConfiguration))
	m.HandleFunc("POST /reglages/importer", c.admin(c.postImporterConfiguration))
}

// champs : lecture des formulaires.
type champs struct{ q *req }

func (f champs) texte(k string) string { return strings.TrimSpace(f.q.r.PostFormValue(k)) }
func (f champs) coche(k string) bool   { return f.q.r.PostFormValue(k) != "" }
func (f champs) liste(k string) []string {
	var out []string
	for _, x := range strings.FieldsFunc(f.texte(k), func(r rune) bool { return r == ',' || r == '\n' || r == ' ' }) {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}
func (f champs) duree(k string, errs *[]error) alertes.Duree {
	d, err := alertes.LireDuree(f.texte(k))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s : durée illisible (ex. 30s, 5m, 2h)", k))
	}
	return d
}
func (f champs) entier(k string, errs *[]error) int {
	n, err := strconv.Atoi(f.texte(k))
	if err != nil {
		*errs = append(*errs, fmt.Errorf("%s : nombre entier attendu", k))
	}
	return n
}

// secret : champ vide = garder le secret enregistré (« écriture seule »).
func (f champs) secret(k string) string {
	if v := f.texte(k); v != "" {
		return v
	}
	return "***"
}

// --- Notifications ---------------------------------------------------------------------

type testCanal struct {
	ID                    int64
	Canal, Statut, Erreur string
	Demande, Fait         int64
}

type donneesNotif struct {
	ServicesHB []notif.ServiceHeartbeat
	Testables  []string       // canaux configurés et actifs (boutons « Tester »)
	R          notif.Reglages // secrets masqués
	Defaut     notif.Reglages
	Etat       []notif.EtatCanal
	HbOK       time.Time
	HbErr      string
	File       int
	Tests      []testCanal
	Vmalert    alertes.ReglagesVmalert
	Canaux     []string
}

func (c *Console) pageNotifications(q *req) {
	ctx := q.r.Context()
	r, err := notif.LireReglages(ctx, c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	d := donneesNotif{R: r.Masques(), Defaut: notif.ReglagesDefaut(), Canaux: []string{notif.NomTelegram, notif.NomOlvid, notif.NomNtfy, notif.NomEmail},
		ServicesHB: notif.ServicesHeartbeat}
	if c.opt.Postier != nil {
		d.Etat, d.HbOK, d.HbErr = c.opt.Postier.Etat()
		d.File = c.opt.Postier.TailleFile(ctx)
	}
	d.Vmalert, _ = alertes.LireReglagesVmalert(ctx, c.db)
	rows, err := c.db.QueryContext(ctx, `SELECT id, canal, demande_le, fait_le, statut, erreur FROM tests_canaux ORDER BY id DESC LIMIT 10`)
	if err == nil {
		for rows.Next() {
			var t testCanal
			if rows.Scan(&t.ID, &t.Canal, &t.Demande, &t.Fait, &t.Statut, &t.Erreur) == nil {
				d.Tests = append(d.Tests, t)
			}
		}
		_ = rows.Close() // lecture seule : une erreur de fermeture est sans effet
	}
	cn := d.R.Canaux
	for _, x := range []struct {
		nom   string
		actif bool
	}{{notif.NomTelegram, cn.Telegram != nil && cn.Telegram.Actif}, {notif.NomOlvid, cn.Olvid != nil && cn.Olvid.Actif},
		{notif.NomNtfy, cn.Ntfy != nil && cn.Ntfy.Actif}, {notif.NomEmail, cn.Email != nil && cn.Email.Actif}} {
		if x.actif {
			d.Testables = append(d.Testables, x.nom)
		}
	}
	// Des canaux non décrits s'affichent quand même (formulaire vide).
	if d.R.Canaux.Telegram == nil {
		d.R.Canaux.Telegram = &notif.Telegram{}
	}
	if d.R.Canaux.Ntfy == nil {
		d.R.Canaux.Ntfy = &notif.Ntfy{}
	}
	if d.R.Canaux.Email == nil {
		d.R.Canaux.Email = &notif.Email{Securite: "starttls"}
	}
	if d.R.Canaux.Olvid == nil {
		d.R.Canaux.Olvid = &notif.Olvid{Confirmation: alertes.Duree(30 * time.Second)}
	}
	if d.R.Canaux.Heartbeat == nil {
		d.R.Canaux.Heartbeat = &notif.Heartbeat{Intervalle: alertes.Duree(time.Minute)}
	}
	c.rendre(q, "notifications", "Notifications", "reglages", d)
}

func (c *Console) postNotifications(q *req) {
	ctx := q.r.Context()
	f := champs{q}
	action := f.texte("action")
	err := func() (string, error) {
		switch {
		case strings.HasPrefix(action, "tester:"):
			canal := strings.TrimPrefix(action, "tester:")
			if _, err := notif.DemanderTest(ctx, c.db, canal, q.t); err != nil {
				return "", err
			}
			return "test demandé : le résultat s'affiche ci-dessous dans quelques secondes (recharger la page)", nil
		case action == "test-effacer":
			id, _ := strconv.ParseInt(f.texte("id"), 10, 64)
			_, err := c.db.ExecContext(ctx, `DELETE FROM tests_canaux WHERE id = ? AND fait_le > 0`, id)
			return "test effacé", err
		case action == "tests-effacer":
			_, err := c.db.ExecContext(ctx, `DELETE FROM tests_canaux WHERE fait_le > 0`)
			return "tests terminés effacés", err
		case action == "tester-heartbeat":
			if c.opt.Postier == nil {
				return "", errors.New("heartbeat : service de notification absent")
			}
			if err := c.opt.Postier.TesterHeartbeat(ctx); err != nil {
				return "", err
			}
			return "heartbeat envoyé : le service a répondu", nil
		case action == "vmalert":
			v := alertes.ReglagesVmalert{Actif: f.coche("actif"), URL: f.texte("url"), Suffixe: f.texte("suffixe"),
				Ignorer: f.liste("ignorer"), Correspondances: map[string]string{}}
			for _, l := range strings.Split(f.q.r.PostFormValue("correspondances"), "\n") {
				if a, b, ok := strings.Cut(strings.TrimSpace(l), "="); ok {
					v.Correspondances[strings.TrimSpace(a)] = strings.TrimSpace(b)
				}
			}
			return "comparaison avec vmalert enregistrée", alertes.EcrireReglagesVmalert(ctx, c.db, v, q.qui(), q.t)
		}
		if err := c.verifierCode(q); err != nil {
			return "", err
		}
		r, err := notif.LireReglages(ctx, c.db)
		if err != nil {
			return "", err
		}
		ok := "réglages des notifications enregistrés (appliqués en 5 s au plus)"
		switch action {
		case "defaut":
			d := notif.ReglagesDefaut()
			d.Canaux = r.Canaux // les canaux décrits et leurs secrets restent
			d.Canaux.Principal, d.Canaux.Secours, d.Canaux.Tous = notif.CanauxDefaut().Principal, "", []string{}
			r = d
			ok = "valeurs par défaut rétablies (canaux décrits gardés)"
		case "inverser":
			if r.Canaux.Secours == "" {
				return "", errors.New("aucun canal de secours à inverser")
			}
			r.Canaux.Principal, r.Canaux.Secours = r.Canaux.Secours, r.Canaux.Principal
			ok = "canaux inversés : principal " + r.Canaux.Principal + ", secours " + r.Canaux.Secours
		default:
			var errs []error
			r.Mode = f.texte("mode")
			r.Lien = f.texte("lien")
			r.Fuseau = f.texte("fuseau")
			r.GrouperPar = f.liste("grouper_par")
			r.ReglesPanne = f.liste("regles_panne")
			for _, g := range []string{alertes.Critique, alertes.Avertissement, alertes.Info} {
				r.Routes[g] = notif.Route{Attente: f.duree("attente_"+g, &errs), Intervalle: f.duree("intervalle_"+g, &errs),
					Rappel: f.duree("rappel_"+g, &errs), Son: f.coche("son_" + g)}
			}
			cn := &r.Canaux
			cn.Principal, cn.Secours = f.texte("principal"), f.texte("secours")
			// Un seul canal à la fois (réponse du 10/11) : jamais d'envoi sur
			// tous les canaux en même temps ; le secours ne sert qu'en panne.
			cn.Tous = []string{}
			cn.Echecs, cn.Retours = f.entier("echecs", &errs), f.entier("retours", &errs)
			cn.VerifTelegram, cn.VerifAutres = f.duree("verif_telegram", &errs), f.duree("verif_autres", &errs)
			cn.ReseauPrive = f.coche("reseau_prive")
			var ids []int64
			for _, x := range f.liste("tg_acquitteurs") {
				n, e := strconv.ParseInt(x, 10, 64)
				if e != nil {
					errs = append(errs, errors.New("acquitteurs Telegram : identifiants numériques"))
				}
				ids = append(ids, n)
			}
			cn.Telegram = &notif.Telegram{Actif: f.coche("tg_actif"), Jeton: f.secret("tg_jeton"), Chat: f.texte("tg_chat"),
				Acquitter: f.coche("tg_acquitter"), Acquitteurs: ids}
			cn.Ntfy = &notif.Ntfy{Actif: f.coche("ntfy_actif"), URL: f.texte("ntfy_url"), Jeton: f.secret("ntfy_jeton")}
			cn.Email = &notif.Email{Actif: f.coche("mail_actif"), Serveur: f.texte("mail_serveur"), Securite: f.texte("mail_securite"),
				Utilisateur: f.texte("mail_utilisateur"), MotDePasse: f.secret("mail_mdp"), De: f.texte("mail_de"), A: f.liste("mail_a")}
			disc, _ := strconv.ParseUint(f.texte("olvid_discussion"), 10, 64)
			cn.Olvid = &notif.Olvid{Actif: f.coche("olvid_actif"), Adresse: f.texte("olvid_adresse"), Cle: f.secret("olvid_cle"),
				Discussion: disc, Confirmation: f.duree("olvid_confirmation", &errs)}
			cn.Heartbeat = &notif.Heartbeat{Actif: f.coche("hb_actif"), Service: f.texte("hb_service"), URL: f.secret("hb_url"), Intervalle: f.duree("hb_intervalle", &errs)}
			// Un canal jamais renseigné ne s'enregistre pas (pas de bloc vide à valider).
			if !cn.Ntfy.Actif && cn.Ntfy.URL == "" {
				cn.Ntfy = nil
			}
			if !cn.Email.Actif && cn.Email.Serveur == "" {
				cn.Email = nil
			}
			if !cn.Olvid.Actif && cn.Olvid.Adresse == "" {
				cn.Olvid = nil
			}
			if !cn.Heartbeat.Actif && f.texte("hb_url") == "" && (r.Canaux.Heartbeat == nil || r.Canaux.Heartbeat.URL == "***") {
				cn.Heartbeat = nil
			}
			if !cn.Telegram.Actif && cn.Telegram.Chat == "" && f.texte("tg_jeton") == "" {
				cn.Telegram = nil
			}
			if err := errors.Join(errs...); err != nil {
				return "", err
			}
		}
		return ok, notif.EcrireReglages(ctx, c.db, r, c.opt.Cle, q.qui(), q.t)
	}
	ok, e := err()
	c.retour(q, "/reglages/notifications", e, ok)
}

// --- Conservation des données ------------------------------------------------------------

type donneesConservation struct {
	C, Defaut        Conservation
	Attente          *ReductionEnAttente
	Etat             EtatStockage
	EstimM, EstimL   int64
	Proposee         *Conservation // estimation demandée (bouton « estimer »)
	EstimPM, EstimPL int64
}

// Les plafonds se saisissent en Mio.
func (f champs) mio(k string, errs *[]error) int64 {
	n, err := strconv.ParseInt(f.texte(k), 10, 64)
	if err != nil || n < 0 {
		*errs = append(*errs, fmt.Errorf("%s : nombre de Mio (0 = sans plafond)", k))
	}
	return n << 20
}

func (c *Console) donneesConservation(q *req) donneesConservation {
	ctx := q.r.Context()
	d := donneesConservation{Defaut: ConservationDefaut(), Etat: c.etatStockage(q.t)}
	var err error
	if d.C, err = LireConservation(ctx, c.db); err != nil {
		c.flash(q, "", err.Error())
	}
	d.Attente, _ = LireAttente(ctx, c.db)
	d.EstimM, d.EstimL = d.Etat.Estimation(d.C)
	return d
}

func (c *Console) pageConservation(q *req) {
	c.rendre(q, "conservation", "Conservation des données", "reglages", c.donneesConservation(q))
}

func (c *Console) postConservation(q *req) {
	f := champs{q}
	ctx := q.r.Context()
	switch f.texte("action") {
	case "annuler":
		c.retour(q, "/reglages/conservation", AnnulerReduction(ctx, c.db, q.qui(), q.ip.String(), q.t), "réduction annulée : rien n'a été supprimé")
		return
	}
	var errs []error
	n := Conservation{Brut: f.duree("brut", &errs), Agrege: f.duree("agrege", &errs), Logs: f.duree("logs", &errs),
		MaxBrut: f.mio("max_brut", &errs), MaxAgrege: f.mio("max_agrege", &errs), MaxLogs: f.mio("max_logs", &errs)}
	// Audit et historique : champ absent (ancien formulaire, script) = inchangé.
	actuel, _ := LireConservation(ctx, c.db)
	n.Audit, n.Historique = actuel.Audit, actuel.Historique
	if f.texte("audit") != "" {
		n.Audit = f.duree("audit", &errs)
	}
	if f.texte("historique") != "" {
		n.Historique = f.duree("historique", &errs)
	}
	if f.texte("action") == "defaut" {
		n, errs = ConservationDefaut(), nil
	}
	err := errors.Join(append(errs, n.Valider())...)
	if err == nil && f.texte("action") == "estimer" {
		d := c.donneesConservation(q)
		d.Proposee = &n
		d.EstimPM, d.EstimPL = d.Etat.Estimation(n)
		c.rendre(q, "conservation", "Conservation des données", "reglages", d)
		return
	}
	if err == nil {
		avant, _ := LireConservation(ctx, c.db)
		if avant.reduit(n) {
			// Garde-fou du brief : confirmation + 2FA avant toute réduction.
			if !f.coche("confirmer") {
				err = errors.New("cette modification supprimera des données : cocher la confirmation")
			} else {
				err = c.verifierCode(q)
			}
		}
	}
	ok := "réglages enregistrés et appliqués"
	if err == nil {
		var attente bool
		if attente, err = EcrireConservation(ctx, c.db, n, q.qui(), q.ip.String(), q.t); err == nil && attente {
			ok = "réduction programmée dans 5 minutes : elle peut encore être annulée"
		} else if err == nil {
			c.appliquerConservation(ctx, q.t)
		}
	}
	c.retour(q, "/reglages/conservation", err, ok)
}

// --- Sauvegardes ---------------------------------------------------------------------------

type donneesSauvegardes struct {
	R          sauvegarde.Reglages // masqués
	Defaut     sauvegarde.Reglages
	Historique []sauvegarde.Execution
	Archives   []sauvegarde.Archive
}

func (c *Console) pageSauvegardes(q *req) {
	ctx := q.r.Context()
	r, err := sauvegarde.LireReglages(ctx, c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	d := donneesSauvegardes{R: r.Masques(), Defaut: sauvegarde.ReglagesDefaut()}
	d.Historique, _ = sauvegarde.Historique(ctx, c.db, 30)
	d.Archives, _ = sauvegarde.Lister(sauvegarde.DossierArchives(c.opt.Donnees))
	if d.R.SFTP == nil {
		d.R.SFTP = &sauvegarde.SFTP{}
	}
	if d.R.S3 == nil {
		d.R.S3 = &sauvegarde.S3{}
	}
	c.rendre(q, "sauvegardes", "Sauvegardes", "reglages", d)
}

func (c *Console) postSauvegardes(q *req) {
	ctx := q.r.Context()
	f := champs{q}
	err := func() (string, error) {
		if f.texte("action") == "maintenant" {
			_, err := sauvegarde.Demander(ctx, c.db, "interface", f.coche("metriques"), f.coche("logs"), q.t)
			return "sauvegarde demandée : faite par le serveur dans la minute (historique ci-dessous)", err
		}
		if err := c.verifierCode(q); err != nil {
			return "", err
		}
		var errs []error
		r := sauvegarde.Reglages{Actif: f.coche("actif"), Heure: f.texte("heure"), Conserver: f.entier("conserver", &errs),
			Metriques: f.coche("metriques"), Logs: f.coche("logs")}
		if f.texte("action") == "defaut" {
			r, errs = sauvegarde.ReglagesDefaut(), nil
		}
		if f.texte("sftp_hote") != "" {
			r.SFTP = &sauvegarde.SFTP{Hote: f.texte("sftp_hote"), Utilisateur: f.texte("sftp_utilisateur"), Dossier: f.texte("sftp_dossier"),
				EmpreinteHote: f.texte("sftp_empreinte"), CleSSH: f.secret("sftp_cle")}
		}
		if f.texte("s3_adresse") != "" {
			r.S3 = &sauvegarde.S3{Adresse: f.texte("s3_adresse"), Region: f.texte("s3_region"), Seau: f.texte("s3_seau"),
				Prefixe: f.texte("s3_prefixe"), CleAcces: f.texte("s3_cle_acces"), Secret: f.secret("s3_secret"), StyleHote: f.coche("s3_style_hote")}
		}
		if err := errors.Join(errs...); err != nil {
			return "", err
		}
		avant, err := sauvegarde.LireReglages(ctx, c.db)
		if err != nil {
			return "", err
		}
		if r, err = sauvegarde.PreparerSecrets(r, avant, c.opt.Cle); err != nil {
			return "", err
		}
		return "réglages des sauvegardes enregistrés", sauvegarde.EcrireReglages(ctx, c.db, r, q.qui(), q.t)
	}
	ok, e := err()
	c.retour(q, "/reglages/sauvegardes", e, ok)
}

// --- Configuration des agents -----------------------------------------------------------
//
// Valeurs typées seulement (brief) : chaque champ du formulaire correspond à
// un champ du document de configuration distante ; vide = hérité de la
// portée au-dessus (défaut → groupe → machine). Le document est revérifié
// avec le code de l'agent avant d'être enregistré.

type champAgent struct {
	Nom, Titre, Genre, Aide string // genre : duree, entier, reel, texte, bool, liste
	Valeur                  string
}

var champsAgent = []champAgent{
	{Nom: "intervalle", Titre: "Intervalle de collecte", Genre: "duree", Aide: "10s à 5m (défaut 30s)"},
	{Nom: "intervalle_lent", Titre: "Intervalle des données lentes", Genre: "duree", Aide: "SMART, mises à jour, certificats"},
	{Nom: "recuperation", Titre: "Récupération de la configuration", Genre: "duree", Aide: "toutes les 1 à 60 min"},
	{Nom: "tampon_metriques", Titre: "Tampon des métriques (Mio)", Genre: "entier"},
	{Nom: "tampon_logs", Titre: "Tampon des logs (Mio)", Genre: "entier"},
	{Nom: "exclure_montages", Titre: "Montages exclus (expression)", Genre: "texte"},
	{Nom: "exclure_types_fs", Titre: "Types de systèmes de fichiers exclus (expression)", Genre: "texte"},
	{Nom: "exclure_disques", Titre: "Disques exclus (expression)", Genre: "texte"},
	{Nom: "exclure_interfaces", Titre: "Interfaces réseau exclues (expression)", Genre: "texte"},
	{Nom: "systemd", Titre: "Collecte systemd", Genre: "bool"},
	{Nom: "systemd_ignorer", Titre: "Unités systemd ignorées", Genre: "liste", Aide: "séparées par des virgules"},
	{Nom: "mises_a_jour", Titre: "Mises à jour en attente", Genre: "bool"},
	{Nom: "metriques", Titre: "Métriques de la machine", Genre: "bool", Aide: "non = logs seulement (l'agent n'envoie que ses propres métriques)"},
	{Nom: "journal", Titre: "Logs du journal", Genre: "bool", Aide: "non avec « Logs Docker » non = métriques seulement"},
	{Nom: "logs_docker", Titre: "Logs Docker", Genre: "bool"},
	{Nom: "logs_debit", Titre: "Débit de logs (lignes/s)", Genre: "reel"},
	{Nom: "logs_rafale", Titre: "Rafale de logs (lignes)", Genre: "reel"},
	{Nom: "logs_bruit", Titre: "Lignes de bruit écartées (expression)", Genre: "texte"},
	{Nom: "masquage", Titre: "Modèles de masquage", Genre: "liste", Aide: "parmi : " + strings.Join(logs.ModelesDisponibles(), ", ")},
	{Nom: "masquage_perso", Titre: "Masquages personnalisés (expressions, 10 au plus)", Genre: "liste", Aide: "une par ligne"},
	{Nom: "niveau_log", Titre: "Niveau du journal de l'agent", Genre: "texte", Aide: "debug, info, warn, error"},
}

type donneesAgents struct {
	Portee, Doc string
	Portees     []string
	Champs      []champAgent
	Machines    []flotte.InfoMachine
	Modeles     []string
	Ligne, Masq string
	Groupes     []groupeAgents // groupes de configuration et leurs machines
}

type groupeAgents struct {
	Nom      string
	Machines []string
}

func (c *Console) lirePortee(ctx context.Context, portee string) map[string]any {
	var doc string
	_ = c.db.QueryRowContext(ctx, `SELECT valeur FROM reglages WHERE cle = ?`, "config_agent:"+portee).Scan(&doc)
	m := map[string]any{}
	_ = json.Unmarshal([]byte(doc), &m)
	return m
}

func (c *Console) donneesAgents(q *req, portee string) donneesAgents {
	ctx := q.r.Context()
	if flotte.ValiderPortee(portee) != nil {
		portee = "defaut"
	}
	d := donneesAgents{Portee: portee, Modeles: logs.ModelesDisponibles()}
	vals := c.lirePortee(ctx, portee)
	for _, ch := range champsAgent {
		if v, ok := vals[ch.Nom]; ok {
			switch x := v.(type) {
			case []any:
				var s []string
				for _, e := range x {
					s = append(s, fmt.Sprint(e))
				}
				sep := ", "
				if ch.Nom == "masquage_perso" {
					sep = "\n"
				}
				ch.Valeur = strings.Join(s, sep)
			default:
				ch.Valeur = fmt.Sprint(x)
			}
		}
		d.Champs = append(d.Champs, ch)
	}
	rows, err := c.db.QueryContext(ctx, `SELECT substr(cle, 14) FROM reglages WHERE cle LIKE 'config\_agent:%' ESCAPE '\' ORDER BY cle`)
	if err == nil {
		for rows.Next() {
			var p string
			if rows.Scan(&p) == nil {
				d.Portees = append(d.Portees, p)
			}
		}
		_ = rows.Close() // lecture seule : une erreur de fermeture est sans effet
	}
	d.Machines, _ = flotte.ListerMachines(ctx, c.db)
	for _, g := range c.lireListes(ctx).Groupes {
		ga := groupeAgents{Nom: g}
		for _, m := range d.Machines {
			if m.Groupe == g {
				ga.Machines = append(ga.Machines, m.Nom)
			}
		}
		d.Groupes = append(d.Groupes, ga)
	}
	return d
}

func (c *Console) pageAgents(q *req) {
	c.rendre(q, "agents", "Configuration des agents", "reglages", c.donneesAgents(q, q.r.URL.Query().Get("portee")))
}

func (c *Console) postAgents(q *req) {
	f := champs{q}
	portee := f.texte("portee")
	vers := "/reglages/agents?portee=" + portee
	err := func() error {
		if err := flotte.ValiderPortee(portee); err != nil {
			return err
		}
		// Code 2FA : la configuration distante change le comportement de
		// toute une partie de la flotte (dont le masquage des secrets dans les
		// logs) ; revue de sécurité du 11/11.
		if err := c.verifierCode(q); err != nil {
			return err
		}
		doc := map[string]any{}
		if f.texte("action") != "retirer" {
			for _, ch := range champsAgent {
				v := f.texte(ch.Nom)
				if v == "" {
					continue // hérité
				}
				switch ch.Genre {
				case "entier":
					n, err := strconv.Atoi(v)
					if err != nil {
						return fmt.Errorf("%s : nombre entier", ch.Titre)
					}
					doc[ch.Nom] = n
				case "reel":
					x, err := strconv.ParseFloat(strings.Replace(v, ",", ".", 1), 64)
					if err != nil {
						return fmt.Errorf("%s : nombre", ch.Titre)
					}
					doc[ch.Nom] = x
				case "bool":
					doc[ch.Nom] = v == "oui"
				case "liste":
					sep := ","
					if ch.Nom == "masquage_perso" {
						sep = "\n"
					}
					var l []string
					for _, e := range strings.Split(v, sep) {
						if e = strings.TrimSpace(e); e != "" {
							l = append(l, e)
						}
					}
					doc[ch.Nom] = l
				default:
					doc[ch.Nom] = v
				}
			}
		}
		b, _ := json.Marshal(doc)
		if len(doc) == 0 {
			b = []byte("{}")
		}
		return flotte.DefinirConfig(q.r.Context(), c.db, portee, b, q.qui(), q.t)
	}()
	c.retour(q, vers, err, "configuration enregistrée : les agents la prennent à leur prochaine demande (5 min au plus)")
}

// postTestMasquage : « je colle une ligne, je vois le résultat » (brief), avec
// le code de masquage de l'agent lui-même.
func (c *Console) postTestMasquage(q *req) {
	f := champs{q}
	d := c.donneesAgents(q, f.texte("portee"))
	d.Ligne = q.r.PostFormValue("ligne")
	if len(d.Ligne) > 4096 {
		d.Ligne = d.Ligne[:4096]
	}
	m, err := logs.NewMasqueur(q.r.PostForm["modeles"], f.liste("perso"))
	if err != nil {
		c.flash(q, "", err.Error())
	} else {
		d.Masq = m.Masquer(d.Ligne)
	}
	c.rendre(q, "agents", "Configuration des agents", "reglages", d)
}

// --- Interface : fuseau, thème, mode debug temporaire ------------------------------------

// ReglagesInterface : clé « interface ».
type ReglagesInterface struct {
	Fuseau      string `json:"fuseau"`
	Theme       string `json:"theme"`        // auto, clair, sombre
	DebugJusqua int64  `json:"debug_jusqua"` // journal du serveur en « debug » jusqu'à (secondes Unix)
	GeoIP       bool   `json:"geoip"`        // base GéoIP téléchargée chaque mois (pays des flux bloqués)
	Menu        string `json:"menu"`         // gauche (défaut) ou haut
	MenuReplie  bool   `json:"menu_replie"`  // menu latéral replié par défaut
	// Ingestion : adresse publique d'ingestion (https://hôte[:port]) que les
	// agents utilisent ; sert à afficher la commande d'installation complète.
	Ingestion string `json:"ingestion,omitempty"`
}

func interfaceDefaut() ReglagesInterface {
	return ReglagesInterface{Fuseau: "Europe/Paris", Theme: "auto", GeoIP: true, Menu: "gauche"}
}

// normaliserIngestion : « https://hôte[:port] » seul (pas de chemin, de
// paramètres ni d'identifiants) ; vide = pas encore renseignée.
func normaliserIngestion(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	if !strings.Contains(s, "://") {
		s = "https://" + s
	}
	u, err := url.Parse(s)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil ||
		(u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" || len(s) > 200 {
		return "", errors.New("adresse d'ingestion : https://hôte ou https://hôte:port attendu (sans chemin)")
	}
	return "https://" + u.Host, nil
}

func (r ReglagesInterface) valider() error {
	if _, err := time.LoadLocation(r.Fuseau); err != nil || r.Fuseau == "" {
		return fmt.Errorf("fuseau %q inconnu (ex. Europe/Paris)", r.Fuseau)
	}
	if r.Theme != "auto" && r.Theme != "clair" && r.Theme != "sombre" {
		return errors.New("thème : auto, clair ou sombre")
	}
	if r.Menu != "gauche" && r.Menu != "haut" {
		return errors.New("menu : gauche ou haut")
	}
	if n, err := normaliserIngestion(r.Ingestion); err != nil || n != r.Ingestion {
		return errors.New("adresse d'ingestion : https://hôte ou https://hôte:port attendu (sans chemin)")
	}
	return nil
}

func lireInterface(ctx context.Context, db *sql.DB) ReglagesInterface {
	r := interfaceDefaut()
	if _, err := lireJSONReglage(ctx, db, "interface", &r); err != nil || r.valider() != nil {
		return interfaceDefaut()
	}
	return r
}

// appliquerInterface : fuseau d'affichage, thème, niveau du journal.
func (c *Console) appliquerInterface(ctx context.Context, t time.Time) {
	r := lireInterface(ctx, c.db)
	if l, err := time.LoadLocation(r.Fuseau); err == nil {
		fuseau.Store(l)
	}
	c.interfaceCourante.Store(&r)
	if c.opt.Niveau != nil {
		voulu := slog.LevelInfo
		if t.Unix() < r.DebugJusqua {
			voulu = slog.LevelDebug
		}
		if c.opt.Niveau.Level() != voulu {
			c.opt.Niveau.Set(voulu)
			c.journal.Info("niveau du journal du serveur", "niveau", voulu.String())
		}
	}
}

type donneesInterface struct {
	ReglagesInterface
	GeoFichier   time.Time
	GeoEnMemoire bool
	GeoErreur    string
	GeoEssai     time.Time // dernier essai de téléchargement
	GeoDemande   bool      // téléchargement demandé, pas encore fait
}

func (c *Console) pageInterface(q *req) {
	d := donneesInterface{ReglagesInterface: lireInterface(q.r.Context(), c.db)}
	if c.opt.GeoIP != nil {
		d.GeoFichier, d.GeoEnMemoire, d.GeoErreur = c.opt.GeoIP.Etat()
		d.GeoEssai, d.GeoDemande = c.opt.GeoIP.Essais()
	}
	c.rendre(q, "interface", "Interface", "reglages", d)
}

func (c *Console) postInterface(q *req) {
	f := champs{q}
	r := lireInterface(q.r.Context(), c.db)
	switch f.texte("action") {
	case "debug":
		var errs []error
		m := f.entier("minutes", &errs)
		if len(errs) > 0 || m < 0 || m > 240 {
			c.retour(q, "/reglages/interface", errors.New("mode debug : 0 à 240 minutes"), "")
			return
		}
		r.DebugJusqua = q.t.Add(time.Duration(m) * time.Minute).Unix()
	case "defaut":
		ing := r.Ingestion // l'adresse du serveur n'est pas un réglage d'affichage
		r = interfaceDefaut()
		r.Ingestion = ing
	case "geoip":
		// « Télécharger maintenant » : fait par le serveur dans la minute.
		if c.opt.GeoIP != nil && r.GeoIP {
			c.opt.GeoIP.Demander()
		}
		c.retour(q, "/reglages/interface", nil, "téléchargement de la base GéoIP demandé (dans la minute)")
		return
	case "adresse":
		var e error
		if r.Ingestion, e = normaliserIngestion(f.texte("ingestion")); e != nil {
			c.retour(q, "/reglages/interface", e, "")
			return
		}
	default:
		avantGeo := r.GeoIP
		r.Fuseau, r.Theme, r.GeoIP = f.texte("fuseau"), f.texte("theme"), f.coche("geoip")
		if r.GeoIP && !avantGeo && c.opt.GeoIP != nil {
			c.opt.GeoIP.Demander() // case cochée : essai tout de suite
		}
		if !r.GeoIP && avantGeo && c.opt.GeoIP != nil {
			c.opt.GeoIP.Supprimer() // case décochée : base effacée tout de suite
		}
		r.Menu, r.MenuReplie = f.texte("menu"), f.coche("menu_replie")
	}
	err := r.valider()
	if err == nil {
		b, _ := json.Marshal(r)
		// Lire l'ancienne valeur AVANT d'ouvrir la transaction : la base n'a
		// qu'une connexion, une lecture pendant la transaction se bloquerait.
		avant, _ := json.Marshal(lireInterface(q.r.Context(), c.db))
		var tx *sql.Tx
		if tx, err = c.db.BeginTx(q.r.Context(), nil); err == nil {
			err = errors.Join(ecrireReglage(q.r.Context(), tx, "interface", string(b), q.qui(), q.t),
				Auditer(q.r.Context(), tx, q.qui(), "reglages_interface", "interface", string(avant), string(b), q.ip.String(), q.t))
			if err == nil {
				err = tx.Commit()
			} else {
				_ = tx.Rollback()
			}
		}
	}
	if err == nil {
		c.appliquerInterface(q.r.Context(), q.t)
	}
	c.retour(q, "/reglages/interface", err, "réglages de l'interface enregistrés")
}

// --- Journal d'audit ----------------------------------------------------------------------

type donneesAudit struct {
	Lignes      []ligneAudit
	Qui, Action string
	Avant       int64
	Suivant     int64
}

func (c *Console) pageAudit(q *req) {
	v := q.r.URL.Query()
	d := donneesAudit{Qui: v.Get("qui"), Action: v.Get("action")}
	d.Avant, _ = strconv.ParseInt(v.Get("avant"), 10, 64)
	cond, args := "1 = 1", []any{}
	if d.Qui != "" {
		cond += " AND qui = ?"
		args = append(args, d.Qui)
	}
	if d.Action != "" {
		cond += " AND action LIKE ?"
		args = append(args, strings.ReplaceAll(d.Action, "%", "")+"%")
	}
	if d.Avant > 0 {
		cond += " AND id < ?"
		args = append(args, d.Avant)
	}
	ls, err := c.lireAuditID(q, cond, args, 200)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	d.Lignes = ls
	if len(ls) == 200 {
		d.Suivant = ls[len(ls)-1].ID
	}
	c.rendre(q, "audit", "Journal d'audit", "audit", d)
}
