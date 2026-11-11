// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"mirador-stack/internal/server/alertes"
	cmdv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/command/v1"
	dtv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/datatypes/v1"
	notv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/notification/v1"
	svcv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/services/v1"
)

// Canal Olvid (SPEC.md § 10, § 17.10, § 17.11) : daemon officiel pour bots
// (olvid/bot-daemon), API gRPC, sur le réseau Docker interne.
//
// Leçons des essais du 10/11 :
//   - MessageSend répond « envoyé » même quand le daemon ne joint plus les
//     serveurs Olvid : il garde le message et le livre plus tard. Un envoi ne
//     compte donc comme réussi qu'avec la notification MessageUploaded
//     (message arrivé sur le serveur Olvid) ;
//   - sans confirmation à temps, le message est ANNULÉ dans le daemon
//     (MessageDelete) avant de passer au secours : pas de doublon en retard
//     (demande de l'utilisateur).
//
// Repris de gladys-olvid (écrit par l'utilisateur) : délai sur chaque appel,
// keepalive HTTP/2 (un daemon disparu sans fermer la connexion ne bloque rien).

// NomOlvid : nom du canal.
const NomOlvid = "olvid"

// Olvid : configuration du canal.
type Olvid struct {
	Actif      bool   `json:"actif"`
	Adresse    string `json:"adresse"`    // hôte:port du daemon (réseau Docker interne), ex. 172.30.0.21:50051
	Cle        string `json:"cle"`        // clé client du bot (secret)
	Discussion uint64 `json:"discussion"` // numéro de discussion (tête-à-tête ou groupe), voir « discussion get »
	// Confirmation : attente de MessageUploaded avant de juger l'envoi raté.
	Confirmation alertes.Duree `json:"confirmation"`
}

const usageOlvid = "notif.olvid.cle"

// valider : utilise = Olvid est le canal principal ou de secours. Le numéro
// de discussion n'est exigé qu'alors : on peut activer Olvid (adresse + clé)
// puis choisir la discussion dans la page « Olvid ».
func (o *Olvid) valider(utilise bool) error {
	var errs []error
	if _, _, err := net.SplitHostPort(o.Adresse); err != nil {
		errs = append(errs, errors.New("olvid.adresse : hôte:port du daemon attendu"))
	}
	if o.Discussion == 0 && utilise {
		errs = append(errs, errors.New("olvid.discussion : numéro de discussion attendu (voir « discussion get » dans le CLI)"))
	}
	if d := time.Duration(o.Confirmation); d != 0 && (d < 5*time.Second || d > 5*time.Minute) {
		errs = append(errs, errors.New("olvid.confirmation : entre 5 s et 5 min"))
	}
	if o.Cle == "" {
		errs = append(errs, errors.New("olvid.cle : clé client du bot manquante"))
	}
	return errors.Join(errs...)
}

// delaiAppel : chaque appel gRPC simple est borné (repris de gladys-olvid).
const delaiAppel = 15 * time.Second

type olvid struct {
	cfg     Olvid
	cle     string
	conn    *grpc.ClientConn
	outils  svcv1.ToolCommandServiceClient
	msgs    svcv1.MessageCommandServiceClient
	notifs  svcv1.MessageNotificationServiceClient
	attente time.Duration
}

// nouvelOlvid prépare la connexion (établie au premier appel, rétablie
// toute seule ensuite).
func nouvelOlvid(cfg Olvid, cle string) (*olvid, error) {
	conn, err := grpc.NewClient(cfg.Adresse,
		// Réseau Docker interne, sans TLS (le daemon ne publie aucun port).
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithKeepaliveParams(keepalive.ClientParameters{Time: 30 * time.Second, Timeout: 10 * time.Second, PermitWithoutStream: true}),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(4<<20)))
	if err != nil {
		return nil, fmt.Errorf("olvid : %w", err)
	}
	attente := time.Duration(cfg.Confirmation)
	if attente == 0 {
		attente = 30 * time.Second
	}
	return &olvid{cfg: cfg, cle: cle, conn: conn, outils: svcv1.NewToolCommandServiceClient(conn),
		msgs: svcv1.NewMessageCommandServiceClient(conn), notifs: svcv1.NewMessageNotificationServiceClient(conn),
		attente: attente}, nil
}

func (o *olvid) Nom() string { return NomOlvid }

// Fermer : connexion libérée quand la configuration change.
func (o *olvid) Fermer() { _ = o.conn.Close() }

// DelaiEnvoi : un envoi peut attendre la confirmation (le postier en tient compte).
func (o *olvid) DelaiEnvoi() time.Duration { return o.attente + 2*delaiAppel }

func (o *olvid) ctx(parent context.Context) context.Context {
	return metadata.AppendToOutgoingContext(parent, "daemon-client-key", o.cle)
}

// erreurGRPC : message court, sans la clé.
func erreurGRPC(quoi string, err error) error {
	if s, ok := status.FromError(err); ok {
		switch s.Code() {
		case codes.Unauthenticated, codes.PermissionDenied:
			return fmt.Errorf("olvid %s : clé client refusée", quoi)
		case codes.Unavailable:
			return fmt.Errorf("olvid %s : daemon injoignable", quoi)
		case codes.DeadlineExceeded:
			return fmt.Errorf("olvid %s : pas de réponse du daemon", quoi)
		}
		return fmt.Errorf("olvid %s : %s", quoi, s.Message())
	}
	return fmt.Errorf("olvid %s : %w", quoi, err)
}

// Envoyer : abonnement à MessageUploaded, envoi, attente de la confirmation ;
// sans confirmation à temps, le message est annulé dans le daemon.
func (o *olvid) Envoyer(ctx context.Context, m *Message, mise Mise) error {
	if o.cfg.Discussion == 0 {
		return errors.New("olvid : aucune discussion choisie (page Réglages › Olvid)")
	}
	mise.HTML, mise.Max = false, 8000
	ctx = o.ctx(ctx)
	ctxAb, finAb := context.WithTimeout(ctx, o.attente+delaiAppel)
	defer finAb()
	sortant := dtv1.MessageId_TYPE_OUTBOUND
	flux, err := o.notifs.MessageUploaded(ctxAb, &notv1.SubscribeToMessageUploadedNotification{
		Filter: &dtv1.MessageFilter{Type: &sortant, DiscussionId: &o.cfg.Discussion}})
	if err != nil {
		return erreurGRPC("abonnement", err)
	}
	// Le daemon enregistre l'abonnement en quelques millisecondes ; on lui
	// laisse ce temps avant d'envoyer (sinon la confirmation pourrait filer).
	time.Sleep(200 * time.Millisecond)
	ctxE, finE := context.WithTimeout(ctx, delaiAppel)
	sans := true
	rep, err := o.msgs.MessageSend(ctxE, &cmdv1.MessageSendRequest{DiscussionId: o.cfg.Discussion, Body: Texte(m, mise),
		DisableLinkPreview: &sans})
	finE()
	if err != nil {
		return erreurGRPC("envoi", err)
	}
	id := rep.GetMessage().GetId()
	confirme := make(chan error, 1)
	go func() {
		for {
			n, err := flux.Recv()
			if err != nil {
				confirme <- err
				return
			}
			if n.GetMessage().GetId().GetId() == id.GetId() {
				confirme <- nil
				return
			}
		}
	}()
	select {
	case err := <-confirme:
		if err == nil {
			return nil
		}
		if ctxAb.Err() == nil { // flux coupé avant la confirmation
			return o.annuler(ctx, id, erreurGRPC("confirmation", err))
		}
	case <-time.After(o.attente):
	case <-ctx.Done():
	}
	return o.annuler(ctx, id, fmt.Errorf("olvid : message non confirmé par le serveur Olvid en %s (réseau Olvid coupé ?)", o.attente))
}

// annuler : retire le message du daemon (il ne partira pas plus tard).
func (o *olvid) annuler(ctx context.Context, id *dtv1.MessageId, cause error) error {
	ctxS, fin := context.WithTimeout(context.WithoutCancel(ctx), delaiAppel)
	defer fin()
	non := false
	if _, err := o.msgs.MessageDelete(ctxS, &cmdv1.MessageDeleteRequest{MessageId: id, DeleteEverywhere: &non}); err != nil {
		return fmt.Errorf("%w ; annulation impossible (%v) : doublon possible au retour du réseau", cause, erreurGRPC("annulation", err))
	}
	return fmt.Errorf("%w ; message annulé dans le daemon", cause)
}

// Verifier : le daemon répond (Ping) et la clé est acceptée
// (AuthenticationTest). Ne dit pas si le daemon joint les serveurs Olvid :
// aucun appel ne le donne ; c'est la confirmation des envois qui le dit.
func (o *olvid) Verifier(ctx context.Context) error {
	ctx, fin := context.WithTimeout(o.ctx(ctx), delaiAppel)
	defer fin()
	if _, err := o.outils.Ping(ctx, &cmdv1.PingRequest{}); err != nil {
		return erreurGRPC("ping", err)
	}
	if _, err := o.outils.AuthenticationTest(ctx, &cmdv1.AuthenticationTestRequest{}); err != nil {
		return erreurGRPC("authentification", err)
	}
	return nil
}
