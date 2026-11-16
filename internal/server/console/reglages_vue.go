// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"fmt"
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mirador-stack/internal/server/alertes"
	"mirador-stack/internal/server/auth"
	"mirador-stack/internal/server/certsyslog"
	"mirador-stack/internal/server/notif"
	"mirador-stack/internal/server/sauvegarde"
)

// Réglages, Utilisateurs et Sécurité plus clairs (SPEC § 38) : l'état du
// moment sur chaque carte, un bilan de sécurité, des pastilles. Tout est lu
// en base à l'affichage ; rien de plus n'est collecté ni gardé.

// pastilleEtat : un petit texte d'état sur une carte (classe : ok,
// attention, erreur, ou vide pour neutre).
type pastilleEtat struct {
	Texte, Classe string
}

// etatCarte : état d'une page de Réglages, montré sur sa carte.
type etatCarte struct {
	Pastilles []pastilleEtat
	Jauge     int    // 0 à 100, -1 = pas de jauge
	Texte     string // sous la jauge
}

// donneesReglages : page d'accueil des Réglages.
type donneesReglages struct {
	Etats map[string]etatCarte // par clé de section (notifications, conservation…)
	// Pages liées : un petit texte à droite du lien (« 3 comptes »…).
	Liees map[string]pastilleEtat
}

func (c *Console) pageReglages(q *req) {
	c.rendre(q, "reglages", "Réglages", "reglages", c.donneesReglages(q))
}

func (c *Console) donneesReglages(q *req) donneesReglages {
	ctx := q.r.Context()
	d := donneesReglages{Etats: map[string]etatCarte{}, Liees: map[string]pastilleEtat{}}
	sans := func(p ...pastilleEtat) etatCarte { return etatCarte{Pastilles: p, Jauge: -1} }

	// Notifications : principal (et son état), secours, mode.
	if r, err := notif.LireReglages(ctx, c.db); err == nil {
		var ps []pastilleEtat
		pannes := map[string]bool{}
		if c.opt.Postier != nil {
			etats, _, _ := c.opt.Postier.Etat()
			for _, e := range etats {
				pannes[e.Nom] = e.EnPanne
			}
		}
		switch {
		case r.Canaux.Principal == "":
			ps = append(ps, pastilleEtat{"aucun canal", "erreur"})
		case pannes[r.Canaux.Principal]:
			ps = append(ps, pastilleEtat{majusculeDebut(r.Canaux.Principal) + " en panne", "erreur"})
		default:
			ps = append(ps, pastilleEtat{majusculeDebut(r.Canaux.Principal) + " principal", "ok"})
		}
		if r.Canaux.Secours == "" {
			ps = append(ps, pastilleEtat{"pas de secours", "attention"})
		} else {
			ps = append(ps, pastilleEtat{"secours " + r.Canaux.Secours, ""})
		}
		if r.Mode == "actif" {
			ps = append(ps, pastilleEtat{"envoi actif", ""})
		} else {
			ps = append(ps, pastilleEtat{"mode ombre : rien n'est envoyé", "attention"})
		}
		d.Etats["notifications"] = sans(ps...)
	}

	// Conservation : place prise sur le plafond total.
	if cons, err := LireConservation(ctx, c.db); err == nil {
		var pris int64
		if c.opt.Metriques != nil {
			b, a := c.opt.Metriques.Taille()
			pris += b + a
		}
		if c.opt.Logs != nil {
			pris += c.opt.Logs.Taille()
		}
		e := etatCarte{Jauge: -1}
		if plafond := cons.MaxBrut + cons.MaxAgrege + cons.MaxLogs; plafond > 0 {
			e.Jauge = min(100, int(math.Round(100*float64(pris)/float64(plafond))))
			e.Texte = fmt.Sprintf("%s sur %s · %d %%", tailleLisible(pris), tailleLisible(plafond), e.Jauge)
		} else {
			e.Texte = tailleLisible(pris) + " (sans plafond)"
		}
		d.Etats["conservation"] = e
	}

	// Sauvegardes : dernière réussie, et rien depuis plus de 2 jours.
	{
		var ps []pastilleEtat
		if r, err := sauvegarde.LireReglages(ctx, c.db); err == nil && !r.Actif {
			ps = append(ps, pastilleEtat{"planification arrêtée", "attention"})
		}
		switch e, ok, _ := sauvegarde.DerniereReussie(ctx, c.db); {
		case !ok:
			ps = append(ps, pastilleEtat{"aucune sauvegarde réussie", "attention"})
		case q.t.Sub(time.Unix(e.Fin, 0)) > 48*time.Hour:
			ps = append(ps, pastilleEtat{"dernière " + depuis(e.Fin*1000), "attention"})
		default:
			ps = append(ps, pastilleEtat{"dernière " + depuis(e.Fin*1000), "ok"})
		}
		d.Etats["sauvegardes"] = sans(ps...)
	}

	// Certificats des collecteurs : le plus proche de sa fin.
	if cols, err := certsyslog.ListerCollecteurs(ctx, c.db); err == nil {
		var ps []pastilleEtat
		fin := int64(0)
		erreur := false
		for _, col := range cols {
			if col.Etat.Erreur != "" {
				erreur = true
			}
			if col.Etat.Fin > 0 && (fin == 0 || col.Etat.Fin < fin) {
				fin = col.Etat.Fin
			}
		}
		switch {
		case len(cols) == 0:
			ps = append(ps, pastilleEtat{"pas de collecteur", ""})
		case fin == 0:
			ps = append(ps, pastilleEtat{"aucun certificat obtenu", ""})
		default:
			jours := int(time.Unix(fin, 0).Sub(q.t).Hours() / 24)
			classe := "ok"
			if jours < 14 {
				classe = "attention"
			}
			ps = append(ps, pastilleEtat{"valable " + strconv.Itoa(jours) + " j", classe})
		}
		if erreur {
			ps = append(ps, pastilleEtat{"erreur de renouvellement", "erreur"})
		}
		d.Etats["certificats"] = sans(ps...)
	}

	// Sites, types et groupes : tailles des listes.
	l := c.lireListes(ctx)
	d.Etats["sites"] = sans(pastilleEtat{fmt.Sprintf("%d %s", len(l.Sites), accorder(len(l.Sites), "site(s)")), ""},
		pastilleEtat{fmt.Sprintf("%d %s", len(l.Types), accorder(len(l.Types), "type(s)")), ""},
		pastilleEtat{fmt.Sprintf("%d %s", len(l.Groupes), accorder(len(l.Groupes), "groupe(s)")), ""})

	// Interface : fuseau ; adresses à renseigner.
	{
		ri := lireInterface(ctx, c.db)
		ps := []pastilleEtat{{ri.Fuseau, ""}}
		if ri.Ingestion == "" {
			ps = append(ps, pastilleEtat{"adresse d'ingestion vide", "attention"})
		}
		if rn, err := notif.LireReglages(ctx, c.db); err == nil && rn.Lien == "" {
			ps = append(ps, pastilleEtat{"pas de lien dans les messages", ""})
		}
		d.Etats["interface"] = sans(ps...)
	}

	// Export : dernier export (journal d'audit).
	{
		var le int64
		_ = c.db.QueryRowContext(ctx, `SELECT COALESCE(MAX(le), 0) FROM audit WHERE action = 'export_configuration'`).Scan(&le)
		if le == 0 {
			d.Etats["export"] = sans(pastilleEtat{"dernier export : jamais", ""})
		} else {
			d.Etats["export"] = sans(pastilleEtat{"dernier export " + depuis(le*1000), ""})
		}
	}

	// Pages liées : utilisateurs et points de sécurité à regarder.
	if us, err := auth.ListerUtilisateurs(ctx, c.db); err == nil {
		d.Liees["/utilisateurs"] = pastilleEtat{fmt.Sprintf("%d %s", len(us), accorder(len(us), "compte(s)")), ""}
		if s, err := LireSecurite(ctx, c.db); err == nil {
			n := 0
			for _, p := range bilanSecurite(s, us) {
				if !p.OK {
					n++
				}
			}
			if n > 0 {
				d.Liees["/securite"] = pastilleEtat{fmt.Sprintf("%d %s", n, accorder(n, "point(s) à regarder")), "attention"}
			} else {
				d.Liees["/securite"] = pastilleEtat{"rien à signaler", "ok"}
			}
		}
	}
	return d
}

// --- Sécurité : bilan --------------------------------------------------------

// pointBilan : une ligne du bilan de sécurité (OK = rien à faire).
type pointBilan struct {
	OK          bool
	Texte, Lien string
}

// bilanSecurite : ce qui va bien et ce qui est à regarder, d'après les
// réglages et les comptes. Seulement des faits déjà en base.
func bilanSecurite(s Securite, us []auth.Utilisateur) []pointBilan {
	out := []pointBilan{{OK: true, Texte: "2FA obligatoire pour tous les comptes"}}
	var sans2FA []string
	admins := 0
	for _, u := range us {
		if u.Desactive {
			continue
		}
		if u.TOTP == "" {
			sans2FA = append(sans2FA, u.Nom)
		}
		if u.Role == auth.RoleAdmin {
			admins++
		}
	}
	if len(sans2FA) > 0 {
		out = append(out, pointBilan{Texte: fmt.Sprintf("%d %s pas encore créé sa 2FA (%s) : à faire à la première connexion",
			len(sans2FA), accorderVerbe(len(sans2FA), "compte(s) n'a(ont)"), strings.Join(sans2FA, ", ")), Lien: "/utilisateurs"})
	}
	if admins == 1 {
		out = append(out, pointBilan{OK: true, Texte: "Un seul administrateur : garder ses codes de secours à l'abri (« Mon compte »)", Lien: "/compte"})
	}
	if strings.TrimSpace(s.IPs) == "" {
		out = append(out, pointBilan{Texte: "Interface ouverte à toutes les adresses : la limiter dans « Accès » ci-dessous si possible", Lien: "#acces"})
	} else {
		out = append(out, pointBilan{OK: true, Texte: "Interface limitée aux adresses autorisées"})
	}
	out = append(out, pointBilan{OK: true, Texte: fmt.Sprintf("Verrouillage après %d échecs, pendant %s", s.Echecs, dureeCourte(time.Duration(s.Verrou)))})
	out = append(out, pointBilan{OK: true, Texte: fmt.Sprintf("Sessions fermées après %s sans activité, %s au plus",
		dureeCourte(time.Duration(s.Inactivite)), dureeCourte(time.Duration(s.DureeMax)))})
	return out
}

// accorderVerbe : remplace « (ont) » selon le nombre (« 1 compte n'a »,
// « 2 comptes n'ont »).
func accorderVerbe(n int, texte string) string {
	if n >= 2 {
		return strings.ReplaceAll(strings.ReplaceAll(texte, "a(ont)", "ont"), "(s)", "s")
	}
	return strings.ReplaceAll(strings.ReplaceAll(texte, "a(ont)", "a"), "(s)", "")
}

// --- Utilisateurs --------------------------------------------------------------

// ligneUtilisateur : un compte et ce qui protège sa connexion.
type ligneUtilisateur struct {
	auth.Utilisateur
	Passkeys, Secours int
	Moi               bool
}

type donneesUtilisateurs struct {
	Lignes            []ligneUtilisateur
	Admins, Sans2FA   int
	Actifs, Desactive int
}

func (c *Console) donneesUtilisateurs(q *req) (donneesUtilisateurs, error) {
	ctx := q.r.Context()
	us, err := auth.ListerUtilisateurs(ctx, c.db)
	var d donneesUtilisateurs
	for _, u := range us {
		l := ligneUtilisateur{Utilisateur: u, Moi: u.Nom == q.u.Nom}
		if pk, err := auth.ListerPasskeys(ctx, c.db, u.ID); err == nil {
			l.Passkeys = len(pk)
		}
		l.Secours, _ = auth.CodesSecoursRestants(ctx, c.db, u.ID)
		if u.Desactive {
			d.Desactive++
		} else {
			d.Actifs++
			if u.Role == auth.RoleAdmin {
				d.Admins++
			}
			if u.TOTP == "" {
				d.Sans2FA++
			}
		}
		d.Lignes = append(d.Lignes, l)
	}
	return d, err
}

// --- Sécurité : durées saisies avec leur unité -------------------------------

// enUnite : durée affichée dans l'unité du champ (« 30 » pour 30 min) si
// elle tombe juste, sinon telle quelle (« 1h30m »).
func enUnite(d alertes.Duree, unite string) string {
	u := uniteDuree(unite)
	if u > 0 && time.Duration(d)%u == 0 {
		return strconv.FormatInt(int64(time.Duration(d)/u), 10)
	}
	return d.String()
}

func uniteDuree(unite string) time.Duration {
	switch unite {
	case "min":
		return time.Minute
	case "h":
		return time.Hour
	}
	return 0
}

// lireDureeUnite : un nombre seul est compté dans l'unité du champ ; une
// durée écrite (« 90m », « 1h30m ») est toujours acceptée.
func lireDureeUnite(r *http.Request, champ, unite string) (alertes.Duree, error) {
	v := strings.TrimSpace(r.PostFormValue(champ))
	if n, err := strconv.ParseFloat(strings.ReplaceAll(v, ",", "."), 64); err == nil && uniteDuree(unite) > 0 {
		if n < 0 || n > 1e6 {
			return 0, fmt.Errorf("%s : nombre hors limites", champ)
		}
		return alertes.Duree(time.Duration(n * float64(uniteDuree(unite)))), nil
	}
	d, err := alertes.LireDuree(v)
	if err != nil {
		return 0, fmt.Errorf("%s : nombre de %s, ou durée (ex. 30m, 12h)", champ, unite)
	}
	return d, nil
}
