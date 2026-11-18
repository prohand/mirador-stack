// SPDX-License-Identifier: GPL-3.0-or-later

package distant

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
)

// « Réessayer la mise à jour » (SPEC § 44) : une version déjà en échec n'est
// retentée que si l'interface l'a demandé APRÈS l'échec ; l'agent annonce
// la capacité « reessai ».
func TestReessaiApresEchec(t *testing.T) {
	dossier := t.TempDir()
	echec := time.Date(2026, 11, 17, 10, 0, 0, 0, time.UTC)
	ecrireEchec := func() {
		brut, _ := json.Marshal(map[string]string{"version": "9.9.9", "raison": "pas de battement", "date": echec.Format(time.RFC3339)})
		if err := os.WriteFile(filepath.Join(dossier, "echec.json"), brut, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	var reessai int64
	var capacites string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		capacites = r.Header.Get(protocole.EnteteCapacites)
		_ = json.NewEncoder(w).Encode(protocole.ReponseMaj{Manifeste: &protocole.Manifeste{Version: "9.9.9", Architecture: "amd64"}, Reessai: reessai})
	}))
	defer srv.Close()
	l := &Lien{Cfg: config.Agent{Serveur: srv.URL}, Client: srv.Client(), Journal: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Installateur: majagent.NewInstallateur(dossier, ""), Locale: config.Agent{FichierCles: filepath.Join(dossier, "absent")}}

	// Sans demande, ou demande plus ancienne que l'échec : rien (la version
	// n'est pas retentée, pas d'erreur).
	ecrireEchec()
	for _, r := range []int64{0, echec.Add(-time.Hour).Unix()} {
		reessai = r
		if err := l.mettreAJour(context.Background()); err != nil {
			t.Fatalf("reessai=%d : erreur %v (version en échec retentée ?)", r, err)
		}
		if _, err := os.Stat(filepath.Join(dossier, "echec.json")); err != nil {
			t.Fatalf("reessai=%d : échec oublié à tort", r)
		}
	}
	if !strings.Contains(capacites, protocole.CapaciteReessai) {
		t.Errorf("capacité « reessai » non annoncée : %q", capacites)
	}
	// Demande faite après l'échec : l'échec est oublié et la version est
	// retentée (ici, elle s'arrête à la vérification de la signature : pas de clé).
	reessai = echec.Add(time.Hour).Unix()
	err := l.mettreAJour(context.Background())
	if err == nil || !strings.Contains(err.Error(), "9.9.9 refusée") {
		t.Fatalf("nouvel essai attendu (arrêt à la signature), obtenu %v", err)
	}
	if _, err := os.Stat(filepath.Join(dossier, "echec.json")); !os.IsNotExist(err) {
		t.Error("échec toujours noté après la demande de nouvel essai")
	}
}

// Le réveil de la boucle de mise à jour ne bloque jamais.
func TestDemanderMajSansBlocage(t *testing.T) {
	l := &Lien{}
	l.demanderMaj()
	l.demanderMaj() // canal déjà plein : ignoré
	select {
	case <-l.reveilMaj():
	default:
		t.Fatal("réveil non reçu")
	}
}
