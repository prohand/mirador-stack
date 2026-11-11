// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sort"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"

	cmdv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/command/v1"
	dtv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/datatypes/v1"
	svcv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/services/v1"
	"mirador-stack/internal/server/secrets"
)

// Gestion des invitations Olvid depuis l'interface (décidé le 10/11, SPEC
// § 17.10) : lien d'invitation du bot, invitations en cours, codes SAS,
// contacts et discussions. Uniquement avec la clé CLIENT du bot (jamais la
// clé admin du daemon) : on ne peut agir que sur l'identité du bot.
//
// Logique reprise de gladys-olvid (invitation par lien, échange des codes à
// 4 chiffres des deux côtés), réécrite en Go.

// AdminOlvid : connexion au daemon pour la page Olvid.
type AdminOlvid struct {
	conn        *grpc.ClientConn
	cle         string
	identite    svcv1.IdentityCommandServiceClient
	invitations svcv1.InvitationCommandServiceClient
	contacts    svcv1.ContactCommandServiceClient
	discussions svcv1.DiscussionCommandServiceClient
}

// ErrOlvidNonConfigure : adresse ou clé absente dans les réglages.
var ErrOlvidNonConfigure = errors.New("Olvid non configuré : renseigner l'adresse du daemon et la clé client (Réglages › Notifications)")

// OuvrirAdminOlvid lit les réglages (clé déchiffrée en mémoire seulement).
func OuvrirAdminOlvid(ctx context.Context, db *sql.DB, cleSecrets []byte) (*AdminOlvid, error) {
	r, err := LireReglages(ctx, db)
	if err != nil {
		return nil, err
	}
	o := r.Canaux.Olvid
	if o == nil || o.Adresse == "" || o.Cle == "" {
		return nil, ErrOlvidNonConfigure
	}
	cle, err := secrets.Dechiffrer(cleSecrets, usageOlvid, o.Cle)
	if err != nil {
		return nil, fmt.Errorf("clé Olvid illisible : %w", err)
	}
	return nouvelAdminOlvid(o.Adresse, cle)
}

func nouvelAdminOlvid(adresse, cle string) (*AdminOlvid, error) {
	conn, err := grpc.NewClient(adresse, grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4<<20)))
	if err != nil {
		return nil, fmt.Errorf("olvid : %w", err)
	}
	return &AdminOlvid{conn: conn, cle: cle, identite: svcv1.NewIdentityCommandServiceClient(conn),
		invitations: svcv1.NewInvitationCommandServiceClient(conn), contacts: svcv1.NewContactCommandServiceClient(conn),
		discussions: svcv1.NewDiscussionCommandServiceClient(conn)}, nil
}

// Fermer libère la connexion.
func (a *AdminOlvid) Fermer() { _ = a.conn.Close() }

// appel : délai de 15 s et clé client (comme pour les envois).
func (a *AdminOlvid) appel(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, annuler := context.WithTimeout(parent, delaiAppel)
	return (&olvid{cle: a.cle}).ctx(ctx), annuler
}

// LienInvitation : lien à ouvrir (ou QR à scanner) depuis l'application Olvid.
func (a *AdminOlvid) LienInvitation(ctx context.Context) (string, error) {
	c, annuler := a.appel(ctx)
	defer annuler()
	r, err := a.identite.IdentityGetInvitationLink(c, &cmdv1.IdentityGetInvitationLinkRequest{})
	if err != nil {
		return "", erreurGRPC("lien d'invitation", err)
	}
	return r.GetInvitationLink(), nil
}

// InvitationOlvid : une invitation en cours.
type InvitationOlvid struct {
	ID        uint64
	Nom       string
	Etat      string // texte lisible
	Le        time.Time
	SAS       string // code à donner à l'autre personne (quand il existe)
	Accepter  bool   // l'invitation attend notre acceptation
	SaisirSAS bool   // on doit saisir le code de l'autre personne
}

func etatInvitation(s dtv1.Invitation_Status) (texte string, accepter, saisir bool) {
	switch s {
	case dtv1.Invitation_STATUS_INVITATION_WAIT_YOU_TO_ACCEPT, dtv1.Invitation_STATUS_INTRODUCTION_WAIT_YOU_TO_ACCEPT,
		dtv1.Invitation_STATUS_ONE_TO_ONE_INVITATION_WAIT_YOU_TO_ACCEPT, dtv1.Invitation_STATUS_GROUP_INVITATION_WAIT_YOU_TO_ACCEPT:
		return "attend votre acceptation", true, false
	case dtv1.Invitation_STATUS_INVITATION_WAIT_YOU_FOR_SAS_EXCHANGE:
		return "attend le code de l'autre personne", false, true
	case dtv1.Invitation_STATUS_INVITATION_WAIT_IT_FOR_SAS_EXCHANGE:
		return "l'autre personne doit saisir votre code", false, false
	case dtv1.Invitation_STATUS_INVITATION_WAIT_IT_TO_ACCEPT, dtv1.Invitation_STATUS_INTRODUCTION_WAIT_IT_TO_ACCEPT,
		dtv1.Invitation_STATUS_ONE_TO_ONE_INVITATION_WAIT_IT_TO_ACCEPT:
		return "attend l'acceptation de l'autre personne", false, false
	case dtv1.Invitation_STATUS_INVITATION_STATUS_IN_PROGRESS:
		return "en cours", false, false
	}
	return "état inconnu", false, false
}

// lireFlux : réponses d'un appel en flux (liste), 1 000 éléments au plus.
func lireFlux[R any](flux grpc.ServerStreamingClient[R], f func(*R) int) error {
	n := 0
	for n < 1000 {
		r, err := flux.Recv()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		n += f(r)
	}
	return nil
}

// Invitations : invitations en cours, plus récentes d'abord.
func (a *AdminOlvid) Invitations(ctx context.Context) ([]InvitationOlvid, error) {
	c, annuler := a.appel(ctx)
	defer annuler()
	flux, err := a.invitations.InvitationList(c, &cmdv1.InvitationListRequest{})
	if err != nil {
		return nil, erreurGRPC("invitations", err)
	}
	var out []InvitationOlvid
	err = lireFlux(flux, func(r *cmdv1.InvitationListResponse) int {
		for _, i := range r.GetInvitations() {
			txt, acc, sas := etatInvitation(i.GetStatus())
			// Horodatage du daemon : millisecondes (comme les messages).
			out = append(out, InvitationOlvid{ID: i.GetId(), Nom: i.GetDisplayName(), Etat: txt, SAS: i.GetSas(),
				Le: time.UnixMilli(int64(i.GetTimestamp())), Accepter: acc, SaisirSAS: sas}) // #nosec G115 -- horodatage
		}
		return len(r.GetInvitations())
	})
	if err != nil {
		return nil, erreurGRPC("invitations", err)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Le.After(out[j].Le) })
	return out, nil
}

var (
	reLienOlvid = regexp.MustCompile(`^https://invitation\.olvid\.io/#[A-Za-z0-9_-]{20,1000}$`)
	reSAS       = regexp.MustCompile(`^[0-9]{4}$`)
)

// Inviter : invitation envoyée à une personne à partir de SON lien Olvid
// (seuls les liens https://invitation.olvid.io/#… sont acceptés).
func (a *AdminOlvid) Inviter(ctx context.Context, lien string) error {
	if !reLienOlvid.MatchString(lien) {
		return errors.New("lien Olvid attendu : https://invitation.olvid.io/#…")
	}
	c, annuler := a.appel(ctx)
	defer annuler()
	if _, err := a.invitations.InvitationNew(c, &cmdv1.InvitationNewRequest{InvitationUrl: lien}); err != nil {
		return erreurGRPC("invitation", err)
	}
	return nil
}

// Accepter, Refuser, Supprimer : actions sur une invitation.
func (a *AdminOlvid) Accepter(ctx context.Context, id uint64) error {
	c, annuler := a.appel(ctx)
	defer annuler()
	_, err := a.invitations.InvitationAccept(c, &cmdv1.InvitationAcceptRequest{InvitationId: id})
	return erreurNil("acceptation", err)
}

func (a *AdminOlvid) Refuser(ctx context.Context, id uint64) error {
	c, annuler := a.appel(ctx)
	defer annuler()
	_, err := a.invitations.InvitationDecline(c, &cmdv1.InvitationDeclineRequest{InvitationId: id})
	return erreurNil("refus", err)
}

func (a *AdminOlvid) Supprimer(ctx context.Context, id uint64) error {
	c, annuler := a.appel(ctx)
	defer annuler()
	_, err := a.invitations.InvitationDelete(c, &cmdv1.InvitationDeleteRequest{InvitationId: id})
	return erreurNil("suppression", err)
}

// SaisirSAS : code à 4 chiffres affiché sur le téléphone de l'autre personne.
func (a *AdminOlvid) SaisirSAS(ctx context.Context, id uint64, sas string) error {
	if !reSAS.MatchString(sas) {
		return errors.New("code : 4 chiffres")
	}
	c, annuler := a.appel(ctx)
	defer annuler()
	_, err := a.invitations.InvitationSas(c, &cmdv1.InvitationSasRequest{InvitationId: id, Sas: sas})
	return erreurNil("code SAS", err)
}

func erreurNil(quoi string, err error) error {
	if err != nil {
		return erreurGRPC(quoi, err)
	}
	return nil
}

// ContactOlvid : un contact du bot.
type ContactOlvid struct {
	ID        uint64
	Nom       string
	TeteATete bool
	Appareils uint32
	Canaux    uint32
}

// Contacts : contacts du bot.
func (a *AdminOlvid) Contacts(ctx context.Context) ([]ContactOlvid, error) {
	c, annuler := a.appel(ctx)
	defer annuler()
	flux, err := a.contacts.ContactList(c, &cmdv1.ContactListRequest{})
	if err != nil {
		return nil, erreurGRPC("contacts", err)
	}
	var out []ContactOlvid
	err = lireFlux(flux, func(r *cmdv1.ContactListResponse) int {
		for _, x := range r.GetContacts() {
			out = append(out, ContactOlvid{ID: x.GetId(), Nom: x.GetDisplayName(), TeteATete: x.GetHasOneToOneDiscussion(),
				Appareils: x.GetDeviceCount(), Canaux: x.GetEstablishedChannelCount()})
		}
		return len(r.GetContacts())
	})
	return out, erreurNil("contacts", err)
}

// DiscussionOlvid : une discussion (tête-à-tête ou groupe).
type DiscussionOlvid struct {
	ID    uint64
	Titre string
}

// Discussions : discussions du bot (pour choisir où partent les alertes).
func (a *AdminOlvid) Discussions(ctx context.Context) ([]DiscussionOlvid, error) {
	c, annuler := a.appel(ctx)
	defer annuler()
	flux, err := a.discussions.DiscussionList(c, &cmdv1.DiscussionListRequest{})
	if err != nil {
		return nil, erreurGRPC("discussions", err)
	}
	var out []DiscussionOlvid
	err = lireFlux(flux, func(r *cmdv1.DiscussionListResponse) int {
		for _, x := range r.GetDiscussions() {
			out = append(out, DiscussionOlvid{ID: x.GetId(), Titre: x.GetTitle()})
		}
		return len(r.GetDiscussions())
	})
	return out, erreurNil("discussions", err)
}

// ChoisirDiscussion : les alertes partent dans cette discussion.
func ChoisirDiscussion(ctx context.Context, db *sql.DB, cle []byte, id uint64, qui string, t time.Time) error {
	r, err := LireReglages(ctx, db)
	if err != nil {
		return err
	}
	if r.Canaux.Olvid == nil {
		return ErrOlvidNonConfigure
	}
	o := *r.Canaux.Olvid
	o.Discussion = id
	r.Canaux.Olvid = &o
	return EcrireReglages(ctx, db, r, cle, qui, t)
}
