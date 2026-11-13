// SPDX-License-Identifier: GPL-3.0-or-later

package flotte

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/agent/majagent"
)

// Purge des versions inutiles (mesure du 13/11, SPEC § 22.6).
func TestNettoyerVersions(t *testing.T) {
	b := nouveauBanc(t)
	ancienne := majagent.ClesPubliques
	majagent.ClesPubliques = ""
	defer func() { majagent.ClesPubliques = ancienne }()
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err := DefinirClesPubliques(ctx, b.db, base64.StdEncoding.EncodeToString(pub), "admin", t0); err != nil {
		t.Fatal(err)
	}
	dossier := t.TempDir()
	binaire := []byte("agent")
	for _, v := range []string{"0.2.9", "0.2.10", "0.2.11", "0.2.18", "0.2.19", "0.2.20"} {
		for _, a := range []string{"amd64", "arm64"} {
			if err := Publier(ctx, b.db, dossier, signer(t, priv, v, a, binaire), binaire, "admin", t0.Add(-48*time.Hour)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// Une machine amd64 en 0.2.11 (version propre 0.2.11), une autre en
	// essai de 0.2.10 ; cible globale 0.2.18 ; une machine refusée en arm64.
	for _, m := range []struct{ nom, statut, arch, version, cible, essai string }{
		{"a", "approuvee", "amd64", "0.2.11", "0.2.11", ""},
		{"b", "approuvee", "amd64", "0.2.9", "", "0.2.10"},
		{"c", "refusee", "arm64", "0.2.18", "", ""},
	} {
		if _, err := b.db.Exec(`INSERT INTO machines(nom, statut, architecture, version_agent, version_cible, maj_essai, cree_le) VALUES (?, ?, ?, ?, ?, ?, ?)`,
			m.nom, m.statut, m.arch, m.version, m.cible, m.essai, t0.Unix()); err != nil {
			t.Fatal(err)
		}
	}
	if err := DefinirCible(ctx, b.db, 0, "0.2.18", "admin", t0); err != nil {
		t.Fatal(err)
	}
	// Un fichier orphelin (écriture interrompue) et un fichier étranger.
	_ = os.WriteFile(filepath.Join(dossier, "mirador-agent-0.1.0-linux-amd64.tmp"), []byte("x"), 0o600)
	_ = os.WriteFile(filepath.Join(dossier, "autre.txt"), []byte("x"), 0o600)

	faites, err := NettoyerVersions(ctx, b.db, dossier, "systeme", t0)
	if err != nil {
		t.Fatal(err)
	}
	// Gardées (amd64 seulement : la machine arm64 est refusée) : 0.2.9 et
	// 0.2.10 (machine b), 0.2.11 (machine a), 0.2.18 (cible), 0.2.19 et
	// 0.2.20 (les 2 plus récentes). Rien d'autre en amd64 ici ; toutes les
	// arm64 partent.
	publiees, _ := versionsPubliees(ctx, b.db)
	var reste []string
	for _, p := range publiees {
		reste = append(reste, p[0]+"/"+p[1])
	}
	sort.Strings(reste)
	attendu := "0.2.10/amd64 0.2.11/amd64 0.2.18/amd64 0.2.19/amd64 0.2.20/amd64 0.2.9/amd64"
	if strings.Join(reste, " ") != attendu {
		t.Errorf("gardées : %v\nattendu : %s", reste, attendu)
	}
	if len(faites) != 7 { // 6 arm64 + le fichier orphelin
		t.Errorf("supprimées : %v", faites)
	}
	if _, err := os.Stat(filepath.Join(dossier, "mirador-agent-0.2.20-linux-arm64")); !os.IsNotExist(err) {
		t.Error("binaire arm64 non effacé")
	}
	if _, err := os.Stat(filepath.Join(dossier, "autre.txt")); err != nil {
		t.Error("fichier étranger effacé")
	}
	// La machine b a confirmé 0.2.10 : 0.2.9 n'est plus utile.
	if _, err := b.db.Exec(`UPDATE machines SET version_agent = '0.2.10', maj_essai = '' WHERE nom = 'b'`); err != nil {
		t.Fatal(err)
	}
	if faites, err = NettoyerVersions(ctx, b.db, dossier, "systeme", t0); err != nil || len(faites) != 1 || faites[0] != "0.2.9/amd64" {
		t.Errorf("deuxième passage : %v %v", faites, err)
	}
	// Version ancienne importée à l'instant (pour un retour arrière) : gardée 24 h.
	if err := Publier(ctx, b.db, dossier, signer(t, priv, "0.2.5", "amd64", binaire), binaire, "admin", t0); err != nil {
		t.Fatal(err)
	}
	if faites, _ = NettoyerVersions(ctx, b.db, dossier, "systeme", t0.Add(time.Hour)); len(faites) != 0 {
		t.Errorf("version importée à l'instant supprimée : %v", faites)
	}
	if faites, _ = NettoyerVersions(ctx, b.db, dossier, "systeme", t0.Add(25*time.Hour)); len(faites) != 1 || faites[0] != "0.2.5/amd64" {
		t.Errorf("après 24 h : %v", faites)
	}
	// Bouton « Supprimer » : refusé pour une version en service.
	if err := SupprimerVersion(ctx, b.db, dossier, "0.2.18", "admin", t0); err == nil || !strings.Contains(err.Error(), "en service") {
		t.Errorf("cible supprimée : %v", err)
	}
	if err := SupprimerVersion(ctx, b.db, dossier, "0.2.19", "admin", t0); err != nil {
		t.Errorf("suppression : %v", err)
	}
	if err := SupprimerVersion(ctx, b.db, dossier, "0.2.19", "admin", t0); err == nil {
		t.Error("version absente supprimée deux fois")
	}
	if err := SupprimerVersion(ctx, b.db, dossier, "../x", "admin", t0); err == nil {
		t.Error("version invalide acceptée")
	}
	// Aucune machine : toutes les architectures restent utiles.
	if _, err := b.db.Exec(`DELETE FROM machines`); err != nil {
		t.Fatal(err)
	}
	if err := Publier(ctx, b.db, dossier, signer(t, priv, "0.2.20", "arm64", binaire), binaire, "admin", t0); err != nil {
		t.Fatal(err)
	}
	// Restent : 0.2.18 (cible) et 0.2.20 (récentes : 0.2.20, 0.2.18), les
	// deux architectures de 0.2.20 ; 0.2.10 et 0.2.11 partent.
	faites, _ = NettoyerVersions(ctx, b.db, dossier, "systeme", t0.Add(25*time.Hour))
	sort.Strings(faites)
	if strings.Join(faites, " ") != "0.2.10/amd64 0.2.11/amd64" {
		t.Errorf("sans machine : %v", faites)
	}
}
