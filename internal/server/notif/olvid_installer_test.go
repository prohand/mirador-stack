// SPDX-License-Identifier: GPL-3.0-or-later

package notif

import (
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	adminv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/admin/v1"
	dtv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/datatypes/v1"
	svcv1 "mirador-stack/internal/server/notif/olvid/pb/olvid/daemon/services/v1"
)

const cleAdminTest = "cle-admin-de-test"

// fauxDaemonInstall : identités et clés client (services admin).
type fauxDaemonInstall struct {
	svcv1.UnimplementedIdentityAdminServiceServer
	svcv1.UnimplementedClientKeyAdminServiceServer
	mu         sync.Mutex
	identites  []*dtv1.Identity
	clesClient int
}

func autoriseAdmin(ctx context.Context) error {
	md, _ := metadata.FromIncomingContext(ctx)
	if v := md.Get("daemon-client-key"); len(v) != 1 || v[0] != cleAdminTest {
		return status.Error(codes.Unauthenticated, "invalid admin client key")
	}
	return nil
}

func (f *fauxDaemonInstall) IdentityList(_ *adminv1.IdentityListRequest, flux grpc.ServerStreamingServer[adminv1.IdentityListResponse]) error {
	if err := autoriseAdmin(flux.Context()); err != nil {
		return err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	return flux.Send(&adminv1.IdentityListResponse{Identities: f.identites})
}

func (f *fauxDaemonInstall) IdentityNew(ctx context.Context, r *adminv1.IdentityNewRequest) (*adminv1.IdentityNewResponse, error) {
	if err := autoriseAdmin(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	id := &dtv1.Identity{Id: uint64(len(f.identites) + 1), DisplayName: r.GetIdentityDetails().GetFirstName()}
	f.identites = append(f.identites, id)
	return &adminv1.IdentityNewResponse{Identity: id}, nil
}

func (f *fauxDaemonInstall) ClientKeyNew(ctx context.Context, r *adminv1.ClientKeyNewRequest) (*adminv1.ClientKeyNewResponse, error) {
	if err := autoriseAdmin(ctx); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.clesClient++
	return &adminv1.ClientKeyNewResponse{ClientKey: &dtv1.ClientKey{Name: r.GetName(), Key: "cle-client-generee", IdentityId: r.GetIdentityId()}}, nil
}

func TestInstallerOlvid(t *testing.T) {
	ctx := context.Background()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fauxDaemonInstall{}
	s := grpc.NewServer()
	svcv1.RegisterIdentityAdminServiceServer(s, f)
	svcv1.RegisterClientKeyAdminServiceServer(s, f)
	go func() { _ = s.Serve(l) }()
	t.Cleanup(s.Stop)
	adr := l.Addr().String()
	db := baseTest(t)

	// Mauvaise clé admin : refus clair, rien d'enregistré.
	if _, err := InstallerOlvid(ctx, db, cleTest, adr, "fausse", "mirador", "test", time.Now()); err == nil {
		t.Fatal("clé admin fausse acceptée")
	}
	if _, err := InstallerOlvid(ctx, db, cleTest, "sans-port", cleAdminTest, "mirador", "test", time.Now()); err == nil {
		t.Fatal("adresse sans port acceptée")
	}
	// Premier passage : identité créée, canal actif, clé client chiffrée.
	res, err := InstallerOlvid(ctx, db, cleTest, adr, cleAdminTest, "mirador", "test", time.Now())
	if err != nil || !res.Creee || res.Identite != 1 {
		t.Fatalf("installation : %+v %v", res, err)
	}
	r, _ := LireReglages(ctx, db)
	if o := r.Canaux.Olvid; o == nil || !o.Actif || o.Adresse != adr || o.Discussion != 0 {
		t.Fatalf("réglages : %+v", r.Canaux.Olvid)
	}
	var brut string
	_ = db.QueryRow(`SELECT valeur FROM reglages WHERE cle = 'notifications'`).Scan(&brut)
	if strings.Contains(brut, "cle-client-generee") || strings.Contains(brut, cleAdminTest) {
		t.Fatal("clé en clair en base")
	}
	// Second passage : identité reprise (pas de doublon), nouvelle clé client.
	res, err = InstallerOlvid(ctx, db, cleTest, adr, cleAdminTest, "Mirador", "test", time.Now())
	if err != nil || res.Creee || res.Identite != 1 || len(f.identites) != 1 || f.clesClient != 2 {
		t.Fatalf("reprise : %+v %v (%d identités, %d clés)", res, err, len(f.identites), f.clesClient)
	}
}
