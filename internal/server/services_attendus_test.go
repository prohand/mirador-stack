// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"mirador-stack/internal/server/alertes"
	srvconfig "mirador-stack/internal/server/config"
	"mirador-stack/internal/server/modeles"
)

// Retour du 16/11 (SPEC § 34.6) : « un agent muet ne devrait rien remonter
// d'autre ». Les services attendus par un modèle ne sont écrits que pour une
// machine qui répond ; pour une machine muette, la règle
// ServiceDuModeleAbsent ne voit donc plus rien (seule AgentMuet reste).
func TestServicesAttendusSeulementSiLaMachineRepond(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	cle := filepath.Join(dir, "cle")
	_ = os.WriteFile(cle, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400)
	cfg := srvconfig.Defaut()
	cfg.EcouteAgents, cfg.EcouteInterface = portLibre(t), portLibre(t)
	cfg.Donnees, cfg.FichierCle = filepath.Join(dir, "d"), cle
	s, err := Demarrer(ctx, cfg, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Arreter()
	now := time.Now()
	ids := map[string]int64{}
	for nom, vu := range map[string]time.Time{"vivante": now.Add(-40 * time.Second), "muette": now.Add(-50 * time.Minute)} {
		res, err := s.db.Exec(`INSERT INTO machines(nom, statut, cree_le, vu_le) VALUES (?, 'approuvee', ?, ?)`, nom, now.Unix()-86400, vu.Unix())
		if err != nil {
			t.Fatal(err)
		}
		ids[nom], _ = res.LastInsertId()
		if err := modeles.Definir(ctx, s.db, modeles.PorteeMachine(ids[nom]), "docker", modeles.EtatActif, nil, nil, "test", now); err != nil {
			t.Fatal(err)
		}
	}
	(&interne{s: s}).tour(ctx, now)
	lecteur := alertes.NouveauLecteur(s.met.Requetes(), slog.New(slog.NewTextHandler(io.Discard, nil)))
	compter := func(expr string) int {
		t.Helper()
		ech, err := lecteur.Instant(ctx, expr, now.Add(time.Second))
		if err != nil {
			t.Fatal(err)
		}
		return len(ech)
	}
	if n := compter(`mirador_modele_service{instance="vivante"}`); n == 0 {
		t.Fatal("machine qui répond : services attendus absents")
	}
	if n := compter(`mirador_modele_service{instance="muette"}`); n != 0 {
		t.Fatalf("machine muette : %d service(s) attendu(s) écrits (fausses alertes ServiceDuModeleAbsent)", n)
	}
	// Le reste du modèle est toujours écrit : les règles importées qu'il
	// remplace restent remplacées pendant la panne.
	if n := compter(`mirador_machine_modele{instance="muette", modele="docker"}`); n != 1 {
		t.Fatalf("machine muette : modèle non écrit (%d)", n)
	}
}
