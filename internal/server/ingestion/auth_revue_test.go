// SPDX-License-Identifier: GPL-3.0-or-later

package ingestion

import (
	"context"
	"errors"
	"net/netip"
	"testing"
)

// Revue du 28/11 (B9) : une lecture en base commencée avant un Oublier
// (révocation) n'est pas remise dans le cache.
func TestCacheTokenApresOublier(t *testing.T) {
	b := nouveauBanc(t, nil)
	a := b.g.auth
	ctx := context.Background()
	ip := netip.MustParseAddr("192.0.2.1")
	// La révocation arrive pendant la lecture du token en base.
	a.apresLecture = func() {
		a.apresLecture = nil
		if _, err := b.db.Exec(`UPDATE tokens SET revoque_le = 5 WHERE machine_id = 1`); err != nil {
			t.Error(err)
		}
		a.Oublier()
	}
	if _, err := a.Verifier(ctx, tokenPC, ip, reception); err != nil {
		t.Fatalf("requête commencée avant la révocation : %v", err)
	}
	if _, err := a.Verifier(ctx, tokenPC, ip, reception); !errors.Is(err, ErrToken) {
		t.Fatalf("token révoqué encore accepté (entrée remise en cache) : %v", err)
	}
}

// Revue du 28/11 (B9) : VerifierSansCache relit la base, même si le cache
// dit encore « valable ».
func TestVerifierSansCache(t *testing.T) {
	b := nouveauBanc(t, nil)
	a := b.g.auth
	ctx := context.Background()
	ip := netip.MustParseAddr("192.0.2.1")
	if _, err := a.Verifier(ctx, tokenPC, ip, reception); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.Exec(`UPDATE tokens SET revoque_le = 5 WHERE machine_id = 1`); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Verifier(ctx, tokenPC, ip, reception); err != nil {
		t.Fatalf("cache (60 s) : %v", err)
	}
	if _, err := a.VerifierSansCache(ctx, tokenPC, ip, "", reception); !errors.Is(err, ErrToken) {
		t.Fatalf("sans cache : %v", err)
	}
}
