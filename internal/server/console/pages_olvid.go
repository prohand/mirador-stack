// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"context"
	"errors"
	"html/template"
	"mirador-stack/internal/server/auth"
	"strconv"
	"strings"

	"mirador-stack/internal/server/notif"
)

// Page Olvid (décidé le 10/11) : lien d'invitation du bot et son QR code,
// invitations en cours, codes à 4 chiffres, contacts, choix de la
// discussion qui reçoit les alertes. Toujours avec la clé client du bot.

type donneesOlvid struct {
	Erreur        string
	Lien          string
	QR            template.URL
	Invitations   []notif.InvitationOlvid
	Contacts      []notif.ContactOlvid
	Discussions   []notif.DiscussionOlvid
	DiscussionAct uint64
}

// olvidActive : le canal Olvid est-il activé (case « Actif » des
// notifications) ? Sinon l'onglet et la page Olvid sont masqués.
func (c *Console) olvidActive(ctx context.Context) bool {
	r, err := notif.LireReglages(ctx, c.db)
	return err == nil && r.Canaux.Olvid != nil && r.Canaux.Olvid.Actif
}

func (c *Console) pageOlvid(q *req) {
	ctx := q.r.Context()
	if !c.olvidActive(ctx) {
		c.retour(q, "/reglages/notifications", errors.New("Olvid n'est pas activé : utiliser l'assistant d'installation (carte Olvid), ou cocher « Actif » et enregistrer"), "")
		return
	}
	var d donneesOlvid
	if r, err := notif.LireReglages(ctx, c.db); err == nil && r.Canaux.Olvid != nil {
		d.DiscussionAct = r.Canaux.Olvid.Discussion
	}
	a, err := notif.OuvrirAdminOlvid(ctx, c.db, c.opt.Cle)
	if err != nil {
		d.Erreur = err.Error()
		c.rendre(q, "olvid", "Olvid", "reglages", d)
		return
	}
	defer a.Fermer()
	var errs []error
	var e error
	if d.Lien, e = a.LienInvitation(ctx); e == nil && d.Lien != "" {
		d.QR, _ = auth.ImageQR(d.Lien)
	}
	errs = append(errs, e)
	d.Invitations, e = a.Invitations(ctx)
	errs = append(errs, e)
	d.Contacts, e = a.Contacts(ctx)
	errs = append(errs, e)
	d.Discussions, e = a.Discussions(ctx)
	errs = append(errs, e)
	if err := errors.Join(errs...); err != nil {
		d.Erreur = err.Error()
	}
	c.rendre(q, "olvid", "Olvid", "reglages", d)
}

func (c *Console) postOlvid(q *req) {
	if !c.olvidActive(q.r.Context()) {
		c.retour(q, "/reglages/notifications", errors.New("Olvid n'est pas activé"), "")
		return
	}
	ctx := q.r.Context()
	f := champs{q}
	id, _ := strconv.ParseUint(f.texte("id"), 10, 64)
	ok, err := func() (string, error) {
		if f.texte("action") == "discussion" {
			if err := c.verifierCode(q); err != nil {
				return "", err
			}
			return "les alertes Olvid partiront dans cette discussion", notif.ChoisirDiscussion(ctx, c.db, c.opt.Cle, id, q.qui(), q.t)
		}
		a, err := notif.OuvrirAdminOlvid(ctx, c.db, c.opt.Cle)
		if err != nil {
			return "", err
		}
		defer a.Fermer()
		var quoi string
		switch f.texte("action") {
		case "accepter":
			quoi, err = "invitation acceptée : échanger maintenant les codes à 4 chiffres", a.Accepter(ctx, id)
		case "refuser":
			quoi, err = "invitation refusée", a.Refuser(ctx, id)
		case "supprimer":
			quoi, err = "invitation supprimée", a.Supprimer(ctx, id)
		case "sas":
			quoi, err = "code envoyé", a.SaisirSAS(ctx, id, strings.TrimSpace(f.texte("sas")))
		case "inviter":
			quoi, err = "invitation envoyée : l'accepter sur le téléphone, puis échanger les codes", a.Inviter(ctx, f.texte("lien"))
		default:
			err = errors.New("action inconnue")
		}
		if err == nil {
			_ = Auditer(ctx, c.db, q.qui(), "olvid_"+f.texte("action"), strconv.FormatUint(id, 10), "", "", q.ip.String(), q.t)
		}
		return quoi, err
	}()
	c.retour(q, "/reglages/olvid", err, ok)
}

// --- Assistant d'installation (SPEC § 18.13) ----------------------------------------------

// adresseOlvidDefaut : nom du conteneur du profil « olvid » du compose de
// mirador (résolu par le DNS de Docker sur le réseau obs).
const adresseOlvidDefaut = "mirador-olvid:50051"

type donneesInstallOlvid struct {
	Adresse, Nom string
	Actif        bool
}

func (c *Console) pageInstallerOlvid(q *req) {
	d := donneesInstallOlvid{Adresse: adresseOlvidDefaut, Nom: "mirador"}
	if r, err := notif.LireReglages(q.r.Context(), c.db); err == nil && r.Canaux.Olvid != nil {
		if r.Canaux.Olvid.Adresse != "" {
			d.Adresse = r.Canaux.Olvid.Adresse
		}
		d.Actif = r.Canaux.Olvid.Actif
	}
	c.rendre(q, "olvid-installer", "Installer Olvid", "reglages", d)
}

func (c *Console) postInstallerOlvid(q *req) {
	f := champs{q}
	ctx := q.r.Context()
	adresse := f.texte("adresse")
	if f.texte("action") == "ping" {
		c.retour(q, "/reglages/olvid/installer", notif.PingOlvid(ctx, adresse), "le daemon répond à "+adresse)
		return
	}
	if err := c.verifierCode(q); err != nil {
		c.retour(q, "/reglages/olvid/installer", err, "")
		return
	}
	// La clé admin ne passe que par ici : jamais gardée, jamais écrite
	// dans un journal ni dans l'audit.
	res, err := notif.InstallerOlvid(ctx, c.db, c.opt.Cle, adresse, q.r.PostFormValue("cle_admin"), f.texte("nom"), q.qui(), q.t)
	if err != nil {
		c.retour(q, "/reglages/olvid/installer", err, "")
		return
	}
	quoi := "reprise"
	if res.Creee {
		quoi = "créée"
	}
	_ = Auditer(ctx, c.db, q.qui(), "olvid_installer", res.Nom, "", "identité "+quoi+", clé client créée", q.ip.String(), q.t)
	c.retour(q, "/reglages/olvid", nil, "bot « "+res.Nom+" » prêt (identité "+quoi+") et Olvid activé : invitez-le maintenant depuis votre téléphone (étape 1 ci-dessous)")
}
