// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/prometheus/model/labels"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"mirador-stack/internal/server/alertes"
	cmdv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/command/v1"
	dtv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/datatypes/v1"
	notv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/notification/v1"
	svcv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/services/v1"
)

// fauxDaemon : daemon Olvid simulé. reseau = false : il accepte les messages
// mais ne les « téléverse » jamais (serveurs Olvid injoignables, comme
// pendant l'essai de coupure du 10/11).
type fauxDaemon struct {
	svcv1.UnimplementedToolCommandServiceServer
	svcv1.UnimplementedMessageCommandServiceServer
	svcv1.UnimplementedMessageNotificationServiceServer

	mu       sync.Mutex
	reseau   bool
	corps    []string
	supprime []uint64
	abonnes  []chan uint64
	suivant  uint64
}

const cleOlvidTest = "cle-client-de-test"

func autorise(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("daemon-client-key"); len(v) != 1 || v[0] != cleOlvidTest {
		return status.Error(codes.Unauthenticated, "invalid client key")
	}
	return nil
}

func (f *fauxDaemon) Ping(context.Context, *cmdv1.PingRequest) (*cmdv1.PingResponse, error) {
	return &cmdv1.PingResponse{}, nil
}

func (f *fauxDaemon) AuthenticationTest(ctx context.Context, _ *cmdv1.AuthenticationTestRequest) (*cmdv1.AuthenticationTestResponse, error) {
	return &cmdv1.AuthenticationTestResponse{}, autorise(ctx)
}

func (f *fauxDaemon) MessageSend(ctx context.Context, r *cmdv1.MessageSendRequest) (*cmdv1.MessageSendResponse, error) {
	if err := autorise(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.suivant++
	f.corps = append(f.corps, r.GetBody())
	id := &dtv1.MessageId{Type: dtv1.MessageId_TYPE_OUTBOUND, Id: f.suivant}
	if f.reseau {
		for _, a := range f.abonnes {
			a <- f.suivant
		}
	}
	return &cmdv1.MessageSendResponse{Message: &dtv1.Message{Id: id, DiscussionId: r.GetDiscussionId(), Body: r.GetBody()}}, nil
}

func (f *fauxDaemon) MessageDelete(ctx context.Context, r *cmdv1.MessageDeleteRequest) (*cmdv1.MessageDeleteResponse, error) {
	if err := autorise(ctx); err != nil {
		return nil, err
	}
	if r.GetDeleteEverywhere() {
		return nil, status.Error(codes.InvalidArgument, "suppression partout non attendue")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.supprime = append(f.supprime, r.GetMessageId().GetId())
	return &cmdv1.MessageDeleteResponse{}, nil
}

func (f *fauxDaemon) MessageUploaded(r *notv1.SubscribeToMessageUploadedNotification,
	flux grpc.ServerStreamingServer[notv1.MessageUploadedNotification]) error {
	if err := autorise(flux.Context()); err != nil {
		return err
	}
	if r.GetFilter().GetType() != dtv1.MessageId_TYPE_OUTBOUND || r.GetFilter().GetDiscussionId() != 7 {
		return status.Error(codes.InvalidArgument, "filtre inattendu")
	}
	c := make(chan uint64, 4)
	f.mu.Lock()
	f.abonnes = append(f.abonnes, c)
	f.mu.Unlock()
	for {
		select {
		case <-flux.Context().Done():
			return nil
		case id := <-c:
			if err := flux.Send(&notv1.MessageUploadedNotification{Message: &dtv1.Message{
				Id: &dtv1.MessageId{Type: dtv1.MessageId_TYPE_OUTBOUND, Id: id}}}); err != nil {
				return err
			}
		}
	}
}

func lancerFauxDaemon(t *testing.T) (*fauxDaemon, string, func()) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fauxDaemon{reseau: true}
	s := grpc.NewServer()
	svcv1.RegisterToolCommandServiceServer(s, f)
	svcv1.RegisterMessageCommandServiceServer(s, f)
	svcv1.RegisterMessageNotificationServiceServer(s, f)
	go func() { _ = s.Serve(l) }()
	return f, l.Addr().String(), s.Stop
}

func TestOlvid(t *testing.T) {
	ctx := context.Background()
	f, adr, arreter := lancerFauxDaemon(t)
	defer arreter()
	o, err := nouvelOlvid(Olvid{Actif: true, Adresse: adr, Discussion: 7, Confirmation: alertes.Duree(time.Second)}, cleOlvidTest)
	if err != nil {
		t.Fatal(err)
	}
	defer o.Fermer()
	m := &Message{Le: time.Now(), Gravite: alertes.Critique, Type: TypeAlerte,
		Actives: []alertes.Alerte{{Regle: "AgentMuet", Gravite: alertes.Critique, Etiquettes: labelsInstance("pc1")}}}

	// 1. Réseau Olvid joignable : confirmé, rien d'annulé.
	if err := o.Verifier(ctx); err != nil {
		t.Fatal(err)
	}
	if err := o.Envoyer(ctx, m, Mise{}); err != nil {
		t.Fatal(err)
	}
	f.mu.Lock()
	if len(f.corps) != 1 || !strings.Contains(f.corps[0], "AgentMuet") || len(f.supprime) != 0 {
		t.Fatalf("envoi : %v, supprimés %v", f.corps, f.supprime)
	}
	// 2. Coupure : accepté par le daemon mais jamais confirmé → échec et
	// message annulé (pas de doublon en retard).
	f.reseau = false
	f.mu.Unlock()
	debut := time.Now()
	err = o.Envoyer(ctx, m, Mise{})
	if err == nil || !strings.Contains(err.Error(), "non confirmé") || !strings.Contains(err.Error(), "message annulé") {
		t.Fatalf("coupure : %v", err)
	}
	if d := time.Since(debut); d < time.Second || d > 5*time.Second {
		t.Errorf("attente de la confirmation : %s", d)
	}
	f.mu.Lock()
	if len(f.supprime) != 1 || f.supprime[0] != 2 {
		t.Fatalf("annulation : %v", f.supprime)
	}
	f.mu.Unlock()

	// 3. Mauvaise clé : refusée, sans la montrer.
	faux, _ := nouvelOlvid(Olvid{Adresse: adr, Discussion: 7}, "mauvaise-cle")
	defer faux.Fermer()
	if err := faux.Verifier(ctx); err == nil || !strings.Contains(err.Error(), "clé client refusée") || strings.Contains(err.Error(), "mauvaise") {
		t.Fatalf("mauvaise clé : %v", err)
	}
	// 4. Daemon arrêté.
	arreter()
	if err := o.Verifier(ctx); err == nil {
		t.Fatal("daemon arrêté : pas d'erreur")
	}
}

func TestOlvidConfiguration(t *testing.T) {
	ctx := context.Background()
	db := baseTest(t)
	r := ReglagesDefaut()
	r.Canaux.Telegram = &Telegram{Actif: true, Jeton: jetonTest, Chat: "-1001234"}
	r.Canaux.Olvid = &Olvid{Actif: true, Adresse: "172.30.0.21:50051", Cle: "cle-secrete-olvid", Discussion: 1}
	r.Canaux.Principal, r.Canaux.Secours = NomOlvid, NomTelegram
	if err := EcrireReglages(ctx, db, r, cleTest, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	var v string
	_ = db.QueryRow(`SELECT valeur FROM reglages WHERE cle = 'notifications'`).Scan(&v)
	if strings.Contains(v, "cle-secrete-olvid") {
		t.Fatal("clé Olvid en clair en base")
	}
	lu, _ := LireReglages(ctx, db)
	canaux, err := fabriquer(&lu.Canaux, cleTest)
	if err != nil || canaux[NomOlvid] == nil {
		t.Fatalf("fabrication : %v", err)
	}
	canaux[NomOlvid].(*olvid).Fermer()
	for _, mauvais := range []Olvid{
		{Actif: true, Adresse: "sans-port", Cle: "x", Discussion: 1},
		{Actif: true, Adresse: "a:1", Cle: "x"},
		{Actif: true, Adresse: "a:1", Cle: "x", Discussion: 1, Confirmation: alertes.Duree(time.Second)},
	} {
		if err := mauvais.valider(true); err == nil {
			t.Errorf("accepté : %+v", mauvais)
		}
	}
	// Pas encore utilisé (ni principal ni secours) : la discussion peut attendre.
	if err := (&Olvid{Actif: true, Adresse: "a:1", Cle: "x"}).valider(false); err != nil {
		t.Errorf("discussion exigée trop tôt : %v", err)
	}
}

func labelsInstance(i string) labels.Labels { return labels.FromStrings("instance", i) }

// Olvid décrit sans Telegram configuré, en mode ombre : ses réglages sont
// quand même vérifiés (adresse sans port refusée).
func TestOlvidVerifieSansTelegram(t *testing.T) {
	r := ReglagesDefaut()
	r.Canaux.Olvid = &Olvid{Actif: true, Adresse: "sans-port", Cle: "x", Discussion: 1}
	if err := r.Valider(); err == nil {
		t.Fatal("réglages Olvid faux acceptés")
	}
	r.Canaux.Olvid.Adresse = "172.30.0.21:50051"
	if err := r.Valider(); err != nil {
		t.Fatal(err)
	}
}

// Heartbeat : service connu ou vide accepté, inconnu refusé.
func TestServiceHeartbeat(t *testing.T) {
	for s, ok := range map[string]bool{"": true, "healthchecks": true, "uptime-kuma": true, "inconnu": false} {
		r := ReglagesDefaut()
		r.Canaux.Heartbeat = &Heartbeat{Service: s, URL: "https://hc-ping.com/abc", Intervalle: alertes.Duree(time.Minute)}
		if err := r.Valider(); (err == nil) != ok {
			t.Errorf("service %q : %v", s, err)
		}
	}
	if (&Heartbeat{Service: "cronitor"}).NomService() != "Cronitor" || (&Heartbeat{}).NomService() != "Autre" {
		t.Error("nom du service faux")
	}
}
