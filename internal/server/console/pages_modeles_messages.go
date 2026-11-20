// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"errors"
	"html/template"
	"net/url"
	"regexp"
	"time"

	"mirador-stack/internal/server/notif"
)

// Modèles des messages (SPEC § 82) : Réglages › Notifications › Modèles des
// messages. Un modèle par type de message (alerte, résolution, rappel,
// escalade, site injoignable), pour tous les canaux ou pour un seul.

type apercuModeleMsg struct {
	Cle, Nom string
	Titre    string        // titre montré à part (ntfy, e-mail, téléphone, webhook)
	HTML     template.HTML // Telegram : mise en forme vérifiée (b, i, code, pre seulement)
	Texte    string        // les autres canaux : texte brut
}

type donneesModelesMsg struct {
	Type, Canal string
	Types       [][2]string
	Canaux      []string
	Perso       bool // un modèle de l'administrateur existe pour (type, canal)
	M           notif.Modele
	Apercus     []apercuModeleMsg
	Erreur      string // erreur du modèle saisi (aperçu)
	Testables   []string
}

// balisesTelegram : seules balises produites par la mise en forme Telegram
// de mirador ; tout autre « < » est échappé par elle.
var balisesTelegram = regexp.MustCompile(`<[^>]*>`)

func htmlTelegramSur(s string) (template.HTML, bool) {
	for _, b := range balisesTelegram.FindAllString(s, -1) {
		switch b {
		case "<b>", "</b>", "<i>", "</i>", "<code>", "</code>", "<pre>", "</pre>":
		default:
			return "", false
		}
	}
	// #nosec G203 -- balises vérifiées une à une ci-dessus ; le texte est déjà échappé par notif.Texte.
	return template.HTML(s), true
}

// apercus : le message d'exemple du type, tel que chaque canal l'enverrait.
func apercusMsg(typ string, md *notif.Modele, canal, lien string, loc *time.Location) []apercuModeleMsg {
	m := notif.MessageExemple(typ, time.Now())
	if lien == "" {
		lien = "https://mirador.exemple.fr"
	}
	var out []apercuModeleMsg
	for _, c := range []struct{ cle, nom string }{
		{notif.NomTelegram, "Telegram"}, {notif.NomNtfy, "ntfy / téléphone"}, {notif.NomEmail, "E-mail"},
		{notif.NomOlvid, "Olvid"}, {notif.NomWebhook, "Webhook (Discord, Slack…)"},
	} {
		o := notif.Mise{Fuseau: loc, Lien: lien, Canal: c.cle}
		if md != nil {
			cleM := typ
			if canal != "" {
				cleM = notif.CleModele(typ, canal)
			}
			o.Modeles = map[string]notif.Modele{cleM: *md}
		}
		a := apercuModeleMsg{Cle: c.cle, Nom: c.nom}
		switch c.cle {
		case notif.NomTelegram:
			o.HTML, o.Max = true, 4096
			if h, ok := htmlTelegramSur(notif.Texte(m, o)); ok {
				a.HTML = h
			} else {
				o.HTML = false
				a.Texte = notif.Texte(m, o)
			}
		case notif.NomNtfy, notif.NomEmail:
			o.TitreAPart, o.Max = true, 4000
			a.Titre = notif.TitreModele(m, o)
			a.Texte = notif.Texte(m, o)
		default:
			o.Max = 4000
			a.Texte = notif.Texte(m, o)
		}
		out = append(out, a)
	}
	return out
}

func typeModeleValide(t string) bool {
	for _, x := range notif.TypesModele {
		if x[0] == t {
			return true
		}
	}
	return false
}

func (c *Console) donneesModelesMsg(q *req, typ, canal string) (donneesModelesMsg, notif.Reglages) {
	r, err := notif.LireReglages(q.r.Context(), c.db)
	if err != nil {
		c.flash(q, "", err.Error())
	}
	if !typeModeleValide(typ) {
		typ = notif.ModeleAlerte
	}
	d := donneesModelesMsg{Type: typ, Canal: canal, Types: notif.TypesModele,
		Canaux: []string{notif.NomTelegram, notif.NomNtfy, notif.NomEmail, notif.NomOlvid, notif.NomApplication, notif.NomWebhook}}
	known := canal == ""
	for _, x := range d.Canaux {
		known = known || x == canal
	}
	if !known {
		d.Canal = ""
	}
	md, ok := r.Modeles[notif.CleModele(d.Type, d.Canal)]
	d.Perso = ok
	if !ok {
		md = notif.ModeleDepart(d.Type)
	}
	d.M = md
	loc, _ := time.LoadLocation(r.Fuseau)
	if loc == nil {
		loc = time.UTC
	}
	if ok {
		d.Apercus = apercusMsg(d.Type, &md, d.Canal, r.Lien, loc)
	} else {
		d.Apercus = apercusMsg(d.Type, nil, "", r.Lien, loc)
	}
	cn := r.Canaux
	for _, x := range d.Canaux {
		actif := map[string]bool{notif.NomTelegram: cn.Telegram != nil && cn.Telegram.Actif, notif.NomNtfy: cn.Ntfy != nil && cn.Ntfy.Actif,
			notif.NomEmail: cn.Email != nil && cn.Email.Actif, notif.NomOlvid: cn.Olvid != nil && cn.Olvid.Actif,
			notif.NomApplication: cn.Application != nil && cn.Application.Actif, notif.NomWebhook: cn.Webhook != nil && cn.Webhook.Actif}[x]
		if actif {
			d.Testables = append(d.Testables, x)
		}
	}
	return d, r
}

func (c *Console) pageModelesMsg(q *req) {
	v := q.r.URL.Query()
	d, _ := c.donneesModelesMsg(q, v.Get("type"), v.Get("canal"))
	c.rendre(q, "modeles_notif", "Modèles des messages", "reglages", d)
}

func (c *Console) postModelesMsg(q *req) {
	ctx := q.r.Context()
	f := champs{q}
	d, r := c.donneesModelesMsg(q, f.texte("type"), f.texte("canal"))
	cle := notif.CleModele(d.Type, d.Canal)
	vers := "/reglages/notifications/modeles?type=" + url.QueryEscape(d.Type) + "&canal=" + url.QueryEscape(d.Canal)
	saisi := notif.Modele{Titre: q.r.PostFormValue("titre"), Corps: q.r.PostFormValue("corps")}
	switch f.texte("action") {
	case "apercu": // sans enregistrer
		d.M, d.Perso = saisi, true
		if err := saisi.Valider(); err != nil {
			d.Erreur = err.Error()
		}
		loc, _ := time.LoadLocation(r.Fuseau)
		if loc == nil {
			loc = time.UTC
		}
		d.Apercus = apercusMsg(d.Type, &saisi, d.Canal, r.Lien, loc)
		c.rendre(q, "modeles_notif", "Modèles des messages", "reglages", d)
		return
	case "defaut":
		if _, ok := r.Modeles[cle]; !ok {
			c.retour(q, vers, nil, "déjà le modèle par défaut")
			return
		}
		delete(r.Modeles, cle)
		c.retour(q, vers, notif.EcrireReglages(ctx, c.db, r, c.opt.Cle, q.qui(), q.t), "modèle par défaut rétabli")
		return
	case "enregistrer", "essai":
		if err := saisi.Valider(); err != nil {
			d.M, d.Perso, d.Erreur = saisi, true, err.Error()
			c.flash(q, "", "Modèle non enregistré : "+err.Error())
			c.rendre(q, "modeles_notif", "Modèles des messages", "reglages", d)
			return
		}
		if r.Modeles == nil {
			r.Modeles = map[string]notif.Modele{}
		}
		r.Modeles[cle] = saisi
		if err := notif.EcrireReglages(ctx, c.db, r, c.opt.Cle, q.qui(), q.t); err != nil {
			c.retour(q, vers, err, "")
			return
		}
		if f.texte("action") == "enregistrer" {
			c.retour(q, vers, nil, "modèle enregistré")
			return
		}
		canal := f.texte("canal_essai")
		if canal == "" {
			canal = r.Canaux.Principal
		}
		_, err := notif.DemanderEssaiModele(ctx, c.db, canal, d.Type, q.t)
		c.retour(q, vers, err, "modèle enregistré ; essai demandé sur "+canal+" (résultat dans Notifications › Tests)")
		return
	}
	c.retour(q, vers, errors.New("action inconnue"), "")
}
