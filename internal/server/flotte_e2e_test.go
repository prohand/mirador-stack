// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"mirador-stack/internal/agent/config"
	"mirador-stack/internal/agent/distant"
	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	srvconfig "mirador-stack/internal/server/config"
	"mirador-stack/internal/server/flotte"
	"mirador-stack/internal/version"
)

// TestFlotteDeBoutEnBout : le code de l'agent parle au vrai serveur pour tout
// le cycle de vie d'une machine.
func TestFlotteDeBoutEnBout(t *testing.T) {
	dir := t.TempDir()
	cle := filepath.Join(dir, "cle")
	_ = os.WriteFile(cle, []byte("AAECAwQFBgcICQoLDA0ODxAREhMUFRYXGBkaGxwdHh8=\n"), 0o400)
	cfgS := srvconfig.Defaut()
	cfgS.EcouteAgents, cfgS.EcouteInterface = portLibre(t), portLibre(t)
	cfgS.Donnees, cfgS.FichierCle = filepath.Join(dir, "donnees"), cle
	journal := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := Demarrer(context.Background(), cfgS, journal)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Arreter()
	base := "http://" + cfgS.EcouteAgents
	ctx, annuler := context.WithTimeout(context.Background(), 30*time.Second)
	defer annuler()
	client := &http.Client{Timeout: 5 * time.Second}

	// Clé de signature de test et version d'agent simulée.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ancCles, ancVersion := majagent.ClesPubliques, version.Version
	majagent.ClesPubliques, version.Version = base64.StdEncoding.EncodeToString(pub), "1.0.0"
	defer func() { majagent.ClesPubliques, version.Version = ancCles, ancVersion }()

	// 1. Enrôlement : jeton → attente → approbation → token.
	jeton, err := flotte.CreerJeton(ctx, s.db, time.Hour, "test", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fichierToken := filepath.Join(dir, "token")
	fin := make(chan error, 1)
	go func() {
		e := distant.Enrolement{URL: base + protocole.CheminEnrolement, Jeton: jeton, NomPropose: "pc-test",
			FichierToken: fichierToken, Client: client, AttenteMin: 50 * time.Millisecond, AttenteMax: 50 * time.Millisecond}
		fin <- e.Executer(ctx)
	}()
	for {
		if _, err := flotte.IDMachine(ctx, s.db, "pc-test"); err == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := flotte.Approuver(ctx, s.db, "pc-test", true, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := <-fin; err != nil {
		t.Fatalf("enrôlement : %v", err)
	}
	token, err := config.LireToken(fichierToken)
	if err != nil {
		t.Fatal(err)
	}
	if info, _ := os.Stat(fichierToken); info.Mode().Perm() != 0o600 {
		t.Errorf("droits du token : %v", info.Mode().Perm())
	}

	// 2. Version signée publiée et assignée ; configuration distante définie.
	binaire := []byte("nouveau binaire de mirador-agent 1.1.0")
	h := sha256.Sum256(binaire)
	m := &protocole.Manifeste{Version: "1.1.0", Architecture: runtime.GOARCH, SHA256: hex.EncodeToString(h[:]), Taille: int64(len(binaire))}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.MessageSigne()))
	if err := flotte.Publier(ctx, s.db, filepath.Join(cfgS.Donnees, "versions"), m, binaire, "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := flotte.DefinirCible(ctx, s.db, 0, "1.1.0", "test", time.Now()); err != nil {
		t.Fatal(err)
	}
	if err := flotte.DefinirConfig(ctx, s.db, "defaut", []byte(`{"intervalle":"60s"}`), "test", time.Now()); err != nil {
		t.Fatal(err)
	}

	// Dossier des binaires de l'agent : version 1.0.0 en service.
	bin := filepath.Join(dir, "bin")
	_ = os.MkdirAll(bin, 0o750)
	_ = os.WriteFile(filepath.Join(bin, "mirador-agent-1.0.0"), []byte("ancien"), 0o755) // #nosec G306 -- test
	_ = os.Symlink("mirador-agent-1.0.0", filepath.Join(bin, "current"))
	inst := majagent.NewInstallateur(bin, "/dev/null")
	inst.SelfTest = func(context.Context, string) error { return nil }

	cfgA := config.Defaut()
	cfgA.Nom, cfgA.Site, cfgA.Env = "pc-test", "maison", "prod"
	cfgA.Serveur, cfgA.MajAuto = base, true
	cfgA.URLMetriques, cfgA.URLLogs = base+protocole.CheminMetriques, base+protocole.CheminLogs
	cfgA.FichierConfigDistante = filepath.Join(dir, "config-distante.json")
	redemarrages := make(chan string, 100)
	vider := func() {
		for len(redemarrages) > 0 {
			<-redemarrages
		}
	}
	// Comme un vrai démarrage : configuration locale + configuration distante gardée.
	lancerLien := func(serveur string) context.CancelFunc {
		vider()
		c := cfgA
		c.Serveur = serveur
		l := &distant.Lien{Cfg: distant.ConfigAuDemarrage(c, journal), Locale: c, Token: token, Client: client, Journal: journal,
			Installateur: inst, Redemarrer: func(r string) {
				select { // l'agent réel s'arrête ; ici la boucle continue : ne pas bloquer
				case redemarrages <- r:
				default:
				}
			}}
		l.ReglerRythmes(50*time.Millisecond, 50*time.Millisecond, 50*time.Millisecond, 500*time.Millisecond, 0)
		c2, a := context.WithCancel(ctx)
		fini := make(chan struct{})
		go func() { l.Tourner(c2); close(fini) }()
		return func() { a(); <-fini } // arrêt complet avant de changer la version simulée
	}

	// 3. L'agent bat, récupère la configuration et la mise à jour : deux
	// demandes de redémarrage (configuration, mise à jour).
	arret := lancerLien(base)
	vus := map[string]bool{}
	for len(vus) < 2 {
		select {
		case r := <-redemarrages:
			vus[r] = true
		case <-ctx.Done():
			t.Fatalf("redémarrages vus : %v", vus)
		}
	}
	arret()
	if !vus["configuration modifiée"] || !vus["mise à jour 1.1.0"] {
		t.Fatalf("redémarrages : %v", vus)
	}
	if doc, _ := os.ReadFile(cfgA.FichierConfigDistante); string(doc) == "" {
		t.Error("configuration distante non gardée")
	}
	if cible, _ := os.Readlink(filepath.Join(bin, "current")); cible != "mirador-agent-1.1.0" {
		t.Fatalf("lien current : %s", cible)
	}
	// La configuration gardée est relue au démarrage suivant.
	if c := distant.ConfigAuDemarrage(cfgA, journal); c.Intervalle != time.Minute {
		t.Errorf("configuration au démarrage : %v", c.Intervalle)
	}
	if err := s.flotte.EcrireBattements(ctx); err != nil {
		t.Fatal(err)
	}
	var vu int64
	_ = s.db.QueryRow(`SELECT battement_le FROM machines WHERE nom = 'pc-test'`).Scan(&vu)
	if vu == 0 {
		t.Error("battement non enregistré")
	}

	// 4. L'agent redémarré sur la nouvelle version joint le serveur : confirmé.
	version.Version = "1.1.0"
	arret = lancerLien(base)
	for e, _ := inst.EnEssai(); e != nil && ctx.Err() == nil; e, _ = inst.EnEssai() {
		time.Sleep(20 * time.Millisecond)
	}
	arret()
	if e, _ := inst.EnEssai(); e != nil {
		t.Fatal("mise à jour non confirmée")
	}

	// 5. Nouvelle mise à jour, mais le serveur est injoignable ensuite :
	// retour à la version précédente, et l'échec n'est pas retenté.
	version.Version = "1.0.0"
	_ = os.Remove(filepath.Join(bin, "current"))
	_ = os.Symlink("mirador-agent-1.0.0", filepath.Join(bin, "current"))
	_ = os.WriteFile(filepath.Join(bin, "mirador-agent-1.0.0"), []byte("ancien"), 0o755) // #nosec G306 -- test
	if err := inst.Installer(ctx, m, binaire); err != nil {
		t.Fatal(err)
	}
	arret = lancerLien("http://127.0.0.1:1") // personne n'écoute
	select {
	case r := <-redemarrages:
		if r != "retour arrière" {
			t.Fatalf("redémarrage : %s", r)
		}
	case <-ctx.Done():
		t.Fatal("pas de retour arrière")
	}
	arret()
	if cible, _ := os.Readlink(filepath.Join(bin, "current")); cible != "mirador-agent-1.0.0" {
		t.Fatalf("après retour arrière : %s", cible)
	}
	if v, _ := inst.DernierEchec(); v != "1.1.0" {
		t.Fatalf("échec noté : %q", v)
	}
	// Le serveur assigne toujours 1.1.0 : l'agent ne la retente pas.
	arret = lancerLien(base)
	select {
	case r := <-redemarrages:
		if r == "mise à jour 1.1.0" {
			t.Fatal("version en échec retentée")
		}
	case <-time.After(500 * time.Millisecond):
	}
	arret()
}
