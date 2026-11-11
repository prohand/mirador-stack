// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"
	"unicode"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"

	adminv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/admin/v1"
	cmdv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/command/v1"
	dtv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/datatypes/v1"
	svcv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/services/v1"
)

// Assistant d'installation Olvid (décidé le 10/11, SPEC § 18.13) : le
// daemon est lancé par l'utilisateur (profil « olvid » du compose de
// mirador) ; mirador ne pilote jamais Docker. L'assistant reçoit la clé
// ADMIN du daemon une seule fois, s'en sert pour créer (ou reprendre)
// l'identité du bot et une clé CLIENT dédiée, puis l'oublie : seule la clé
// client est gardée (chiffrée), comme avant.
//
// Parcours inspiré des « intégrations » de Gladys et de gladys-olvid
// (repris de gladys-olvid : un bot = une identité, une clé client par
// application), réécrit en Go.

// ResultatInstallOlvid : ce que l'assistant a fait.
type ResultatInstallOlvid struct {
	Identite uint64
	Nom      string
	Creee    bool // identité créée (sinon reprise)
}

// nomCleClient : nom de la clé client créée pour mirador dans le daemon.
const nomCleClient = "mirador-server"

// verifierNomBot : nom affiché du bot, 1 à 40 caractères visibles.
func verifierNomBot(s string) error {
	if s == "" || len([]rune(s)) > 40 {
		return errors.New("nom du bot : 1 à 40 caractères")
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return errors.New("nom du bot : caractère interdit")
		}
	}
	return nil
}

// PingOlvid : le daemon répond-il à cette adresse ? (sans clé)
func PingOlvid(ctx context.Context, adresse string) error {
	if _, _, err := net.SplitHostPort(strings.TrimSpace(adresse)); err != nil {
		return errors.New("adresse du daemon : hôte:port attendu (ex. mirador-olvid:50051)")
	}
	conn, err := grpc.NewClient(strings.TrimSpace(adresse), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("olvid : %w", err)
	}
	defer conn.Close()
	c, annuler := context.WithTimeout(ctx, 5*time.Second)
	defer annuler()
	if _, err := svcv1.NewToolCommandServiceClient(conn).Ping(c, &cmdv1.PingRequest{}); err != nil {
		return erreurGRPC("daemon injoignable", err)
	}
	return nil
}

// InstallerOlvid : identité du bot (créée si absente) + clé client, puis
// canal Olvid enregistré et activé. cleAdmin n'est ni gardée ni journalisée.
func InstallerOlvid(ctx context.Context, db *sql.DB, cleSecrets []byte, adresse, cleAdmin, nomBot, qui string, t time.Time) (ResultatInstallOlvid, error) {
	var res ResultatInstallOlvid
	adresse, nomBot, cleAdmin = strings.TrimSpace(adresse), strings.TrimSpace(nomBot), strings.TrimSpace(cleAdmin)
	if _, _, err := net.SplitHostPort(adresse); err != nil {
		return res, errors.New("adresse du daemon : hôte:port attendu (ex. 172.30.0.21:50051)")
	}
	if cleAdmin == "" || len(cleAdmin) > 200 {
		return res, errors.New("clé admin du daemon manquante")
	}
	if err := verifierNomBot(nomBot); err != nil {
		return res, err
	}
	conn, err := grpc.NewClient(adresse, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return res, fmt.Errorf("olvid : %w", err)
	}
	defer conn.Close()
	adm := &olvid{cle: cleAdmin} // même en-tête que la clé client
	appel := func() (context.Context, context.CancelFunc) {
		c, annuler := context.WithTimeout(ctx, delaiAppel)
		return adm.ctx(c), annuler
	}

	// 1. Identité du bot : reprise si une identité porte déjà ce nom.
	c, annuler := appel()
	flux, err := svcv1.NewIdentityAdminServiceClient(conn).IdentityList(c, &adminv1.IdentityListRequest{})
	if err != nil {
		annuler()
		return res, erreurGRPC("liste des identités (clé admin ?)", err)
	}
	err = lireFlux(flux, func(r *adminv1.IdentityListResponse) int {
		for _, x := range r.GetIdentities() {
			if res.Identite == 0 && strings.EqualFold(strings.TrimSpace(x.GetDisplayName()), nomBot) {
				res.Identite, res.Nom = x.GetId(), x.GetDisplayName()
			}
		}
		return len(r.GetIdentities())
	})
	annuler()
	if err = erreurNil("liste des identités (clé admin ?)", err); err != nil {
		return res, err
	}
	if res.Identite == 0 {
		c, annuler = appel()
		r, err := svcv1.NewIdentityAdminServiceClient(conn).IdentityNew(c, &adminv1.IdentityNewRequest{
			IdentityDetails: &dtv1.IdentityDetails{FirstName: &nomBot}})
		annuler()
		if err != nil {
			return res, erreurGRPC("création de l'identité du bot", err)
		}
		res.Identite, res.Nom, res.Creee = r.GetIdentity().GetId(), r.GetIdentity().GetDisplayName(), true
		if res.Identite == 0 {
			return res, errors.New("olvid : le daemon n'a pas rendu le numéro de l'identité")
		}
	}

	// 2. Clé client dédiée à mirador (une nouvelle à chaque passage : les
	// anciennes se suppriment dans le CLI si besoin).
	c, annuler = appel()
	ck, err := svcv1.NewClientKeyAdminServiceClient(conn).ClientKeyNew(c, &adminv1.ClientKeyNewRequest{
		Name: nomCleClient, IdentityId: res.Identite})
	annuler()
	if err != nil {
		return res, erreurGRPC("création de la clé client", err)
	}
	cle := ck.GetClientKey().GetKey()
	if cle == "" {
		return res, errors.New("olvid : le daemon n'a pas rendu de clé client")
	}

	// 3. Canal Olvid activé. La discussion se choisit ensuite dans la page
	// Olvid ; elle est gardée si l'identité n'a pas changé.
	r, err := LireReglages(ctx, db)
	if err != nil {
		return res, err
	}
	var o Olvid // confirmation 0 = 30 s par défaut
	if r.Canaux.Olvid != nil {
		o = *r.Canaux.Olvid
	}
	if res.Creee || o.Adresse != adresse {
		o.Discussion = 0
	}
	o.Actif, o.Adresse, o.Cle = true, adresse, cle
	r.Canaux.Olvid = &o
	return res, EcrireReglages(ctx, db, r, cleSecrets, qui, t)
}
