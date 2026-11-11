// SPDX-License-Identifier: GPL-3.0-or-later

package server

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
	"mirador-stack/internal/server/base"
	"mirador-stack/internal/server/config"
)

func TestCommandes(t *testing.T) {
	dir := t.TempDir()
	cfg := config.Defaut()
	cfg.Donnees = dir
	ctx := context.Background()
	lancer := func(args ...string) (int, string, string) {
		var so, se bytes.Buffer
		code := Commande(ctx, cfg, args, &so, &se)
		return code, so.String(), se.String()
	}
	// Base pas encore créée par le serveur : refus clair.
	if code, _, se := lancer("machines"); code == 0 || !strings.Contains(se, "démarrez d'abord le serveur") {
		t.Fatalf("base non migrée : %d %s", code, se)
	}
	db, err := base.Ouvrir(filepath.Join(dir, "mirador.db"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := base.Migrer(ctx, db, dir); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()

	if code, so, _ := lancer("aide"); code != 0 || !strings.Contains(so, "approuver") {
		t.Fatal("aide")
	}
	if code, so, _ := lancer("jeton", "-duree", "2h"); code != 0 || !strings.Contains(so, "mirador_enrol_") {
		t.Fatalf("jeton : %s", so)
	}
	code, so, _ := lancer("token", "srv-test-mirador")
	if code != 0 || !strings.Contains(so, "mirador_") {
		t.Fatalf("token : %s", so)
	}
	for _, c := range [][]string{
		{"ips", "srv-test-mirador", "203.0.113.7, 2001:db8::/64"},
		{"etiquettes", "-site", "maison", "-env", "prod", "-groupe", "dns", "srv-test-mirador"},
	} {
		if code, _, se := lancer(c...); code != 0 {
			t.Fatalf("%v : %s", c, se)
		}
	}
	doc := filepath.Join(dir, "config.json")
	_ = os.WriteFile(doc, []byte(`{"intervalle": "60s"}`), 0o600)
	if code, _, se := lancer("config", "machine:srv-test-mirador", doc); code != 0 {
		t.Fatalf("config : %s", se)
	}
	if code, so, _ := lancer("voir-config", "srv-test-mirador"); code != 0 || strings.TrimSpace(so) != `{"intervalle":"60s"}` {
		t.Fatalf("voir-config : %q", so)
	}
	code, so, _ = lancer("machines")
	if code != 0 || !strings.Contains(so, "srv-test-mirador") || !strings.Contains(so, "203.0.113.7/32") || !strings.Contains(so, "approuvee") {
		t.Fatalf("machines :\n%s", so)
	}
	// Erreurs attendues.
	for _, c := range [][]string{
		{"approuver", "inconnue"},
		{"cible", "9.9.9"},
		{"ips", "srv-test-mirador", "pas une adresse"},
		{"config", "defaut", filepath.Join(dir, "absent.json")},
		{"approuver"},
		{"nimporte"},
	} {
		if code, _, _ := lancer(c...); code == 0 {
			t.Errorf("%v accepté", c)
		}
	}
	// Publication : manifeste en texte, binaire sur l'entrée standard.
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	ancienne := majagent.ClesPubliques
	majagent.ClesPubliques = base64.StdEncoding.EncodeToString(pub)
	defer func() { majagent.ClesPubliques = ancienne }()
	binaire := []byte("binaire 1.0.1")
	h := sha256.Sum256(binaire)
	m := protocole.Manifeste{Version: "1.0.1", Architecture: "arm64", SHA256: hex.EncodeToString(h[:]), Taille: int64(len(binaire))}
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(priv, m.MessageSigne()))
	brutM, _ := json.Marshal(m)
	entree = bytes.NewReader(binaire)
	if code, _, se := lancer("publier", string(brutM), "-"); code != 0 {
		t.Fatalf("publier : %s", se)
	}
	entree = bytes.NewReader([]byte("autre contenu"))
	if code, _, _ := lancer("publier", string(brutM), "-"); code == 0 {
		t.Error("binaire différent publié")
	}
	if code, _, se := lancer("cible", "-machine", "srv-test-mirador", "1.0.1"); code != 0 {
		t.Fatalf("cible : %s", se)
	}
	if code, _, se := lancer("revoquer", "srv-test-mirador"); code != 0 {
		t.Fatal(se)
	}
	if _, so, _ := lancer("machines"); !strings.Contains(so, "  0  ") && !strings.Contains(so, " 0 ") {
		t.Errorf("tokens après révocation :\n%s", so)
	}
}
