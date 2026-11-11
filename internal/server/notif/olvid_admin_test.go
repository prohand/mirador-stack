// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"net"
	"sync"
	"testing"

	"google.golang.org/grpc"

	cmdv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/command/v1"
	dtv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/datatypes/v1"
	svcv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/services/v1"
)

// fauxDaemonAdmin : invitations, contacts, discussions (faux daemon gRPC).
type fauxDaemonAdmin struct {
	svcv1.UnimplementedIdentityCommandServiceServer
	svcv1.UnimplementedInvitationCommandServiceServer
	svcv1.UnimplementedContactCommandServiceServer
	svcv1.UnimplementedDiscussionCommandServiceServer

	mu       sync.Mutex
	acceptee []uint64
	sas      map[uint64]string
	invites  []string
}

func (f *fauxDaemonAdmin) IdentityGetInvitationLink(ctx context.Context, _ *cmdv1.IdentityGetInvitationLinkRequest) (*cmdv1.IdentityGetInvitationLinkResponse, error) {
	if err := autorise(ctx); err != nil {
		return nil, err
	}
	return &cmdv1.IdentityGetInvitationLinkResponse{InvitationLink: "https://invitation.olvid.io/#AAAAAAAAAAAAAAAAAAAAAAAA"}, nil
}

func (f *fauxDaemonAdmin) InvitationList(_ *cmdv1.InvitationListRequest, flux grpc.ServerStreamingServer[cmdv1.InvitationListResponse]) error {
	if err := autorise(flux.Context()); err != nil {
		return err
	}
	sas := "1234"
	return flux.Send(&cmdv1.InvitationListResponse{Invitations: []*dtv1.Invitation{
		{Id: 1, DisplayName: "Alice", Status: dtv1.Invitation_STATUS_INVITATION_WAIT_YOU_TO_ACCEPT, Timestamp: 1000},
		{Id: 2, DisplayName: "Bob", Status: dtv1.Invitation_STATUS_INVITATION_WAIT_YOU_FOR_SAS_EXCHANGE, Sas: &sas, Timestamp: 2000},
	}})
}

func (f *fauxDaemonAdmin) InvitationAccept(ctx context.Context, r *cmdv1.InvitationAcceptRequest) (*cmdv1.InvitationAcceptResponse, error) {
	if err := autorise(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.acceptee = append(f.acceptee, r.GetInvitationId())
	f.mu.Unlock()
	return &cmdv1.InvitationAcceptResponse{}, nil
}

func (f *fauxDaemonAdmin) InvitationSas(ctx context.Context, r *cmdv1.InvitationSasRequest) (*cmdv1.InvitationSasResponse, error) {
	if err := autorise(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.sas[r.GetInvitationId()] = r.GetSas()
	f.mu.Unlock()
	return &cmdv1.InvitationSasResponse{}, nil
}

func (f *fauxDaemonAdmin) InvitationNew(ctx context.Context, r *cmdv1.InvitationNewRequest) (*cmdv1.InvitationNewResponse, error) {
	if err := autorise(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	f.invites = append(f.invites, r.GetInvitationUrl())
	f.mu.Unlock()
	return &cmdv1.InvitationNewResponse{}, nil
}

func (f *fauxDaemonAdmin) ContactList(_ *cmdv1.ContactListRequest, flux grpc.ServerStreamingServer[cmdv1.ContactListResponse]) error {
	if err := autorise(flux.Context()); err != nil {
		return err
	}
	return flux.Send(&cmdv1.ContactListResponse{Contacts: []*dtv1.Contact{{Id: 5, DisplayName: "Moi", HasOneToOneDiscussion: true, DeviceCount: 1}}})
}

func (f *fauxDaemonAdmin) DiscussionList(_ *cmdv1.DiscussionListRequest, flux grpc.ServerStreamingServer[cmdv1.DiscussionListResponse]) error {
	if err := autorise(flux.Context()); err != nil {
		return err
	}
	return flux.Send(&cmdv1.DiscussionListResponse{Discussions: []*dtv1.Discussion{{Id: 7, Title: "Moi"}, {Id: 9, Title: "Groupe supervision"}}})
}

// LancerFauxDaemonAdmin : pour les tests d'autres paquets (interface web).
func lancerFauxDaemonAdmin(t *testing.T) (*fauxDaemonAdmin, string) {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fauxDaemonAdmin{sas: map[uint64]string{}}
	s := grpc.NewServer()
	svcv1.RegisterIdentityCommandServiceServer(s, f)
	svcv1.RegisterInvitationCommandServiceServer(s, f)
	svcv1.RegisterContactCommandServiceServer(s, f)
	svcv1.RegisterDiscussionCommandServiceServer(s, f)
	go func() { _ = s.Serve(l) }()
	t.Cleanup(s.Stop)
	return f, l.Addr().String()
}

func TestAdminOlvid(t *testing.T) {
	ctx := context.Background()
	f, adr := lancerFauxDaemonAdmin(t)
	a, err := nouvelAdminOlvid(adr, cleOlvidTest)
	if err != nil {
		t.Fatal(err)
	}
	defer a.Fermer()
	if l, err := a.LienInvitation(ctx); err != nil || l == "" {
		t.Fatalf("lien : %q %v", l, err)
	}
	inv, err := a.Invitations(ctx)
	if err != nil || len(inv) != 2 {
		t.Fatalf("invitations : %+v %v", inv, err)
	}
	if inv[0].Nom != "Bob" || !inv[0].SaisirSAS || inv[0].SAS != "1234" || !inv[1].Accepter {
		t.Errorf("états : %+v", inv)
	}
	if err := a.Accepter(ctx, 1); err != nil {
		t.Fatal(err)
	}
	if err := a.SaisirSAS(ctx, 2, "12ab"); err == nil {
		t.Error("code SAS invalide accepté")
	}
	if err := a.SaisirSAS(ctx, 2, "5678"); err != nil {
		t.Fatal(err)
	}
	if err := a.Inviter(ctx, "https://exemple.org/piege"); err == nil {
		t.Error("lien étranger accepté")
	}
	if err := a.Inviter(ctx, "https://invitation.olvid.io/#AwAAAHAAAAAsaHR0cHM6Ly9zZXJ2"); err != nil {
		t.Fatal(err)
	}
	cs, err := a.Contacts(ctx)
	if err != nil || len(cs) != 1 || !cs[0].TeteATete {
		t.Errorf("contacts : %+v %v", cs, err)
	}
	ds, err := a.Discussions(ctx)
	if err != nil || len(ds) != 2 {
		t.Errorf("discussions : %+v %v", ds, err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.acceptee) != 1 || f.sas[2] != "5678" || len(f.invites) != 1 {
		t.Errorf("daemon : %+v", f)
	}
	// Mauvaise clé : refusée, sans être montrée.
	b, _ := nouvelAdminOlvid(adr, "mauvaise-cle")
	defer b.Fermer()
	if _, err := b.Invitations(ctx); err == nil || err.Error() != "olvid invitations : clé client refusée" {
		t.Errorf("mauvaise clé : %v", err)
	}
}
