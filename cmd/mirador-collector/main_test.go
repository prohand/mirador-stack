// SPDX-License-Identifier: GPL-3.0-or-later

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirador-stack/internal/agent/distant"
	"mirador-stack/internal/agent/majagent"
	"mirador-stack/internal/protocole"
)

func ecrireConf(t *testing.T, contenu string) string {
	t.Helper()
	chemin := filepath.Join(t.TempDir(), "collector.conf")
	if err := os.WriteFile(chemin, []byte(contenu), 0o600); err != nil {
		t.Fatal(err)
	}
	return chemin
}

func TestLancer(t *testing.T) {
	defer func() { majagent.ProduitCourant, distant.Capacites = protocole.ProduitAgent, protocole.Capacites }()
	var sortie, erreurs bytes.Buffer
	if code := lancer([]string{"-version"}, &sortie, &erreurs); code != 0 || !strings.HasPrefix(sortie.String(), "mirador-collector ") {
		t.Fatalf("-version : %d %q", code, sortie.String())
	}
	// Ce binaire se présente comme collecteur (mises à jour et serveur).
	if majagent.ProduitCourant != protocole.ProduitCollecteur || distant.Capacites != protocole.CapaciteCollecteur+","+protocole.CapaciteSondes {
		t.Errorf("produit %q, capacités %q", majagent.ProduitCourant, distant.Capacites)
	}
	conf := ecrireConf(t, "MIRADOR_NOM=collecteur-test\nMIRADOR_SERVEUR=https://mirador.exemple.invalid\n"+
		"MIRADOR_FICHIER_CONFIG_DISTANTE="+filepath.Join(t.TempDir(), "distante.json")+"\n")
	sortie.Reset()
	if code := lancer([]string{"-config", conf, "-self-test"}, &sortie, &erreurs); code != 0 || !strings.Contains(sortie.String(), "self-test OK") {
		t.Fatalf("-self-test : %d %q %q", code, sortie.String(), erreurs.String())
	}
	// Clé inconnue : refusée (comme l'agent).
	erreurs.Reset()
	if code := lancer([]string{"-config", ecrireConf(t, "MIRADOR_NOM=c\nMIRADOR_COMMANDE=rm\n"), "-self-test"}, &sortie, &erreurs); code == 0 {
		t.Error("clé inconnue acceptée")
	}
	// Sans token ni jeton : -verifier échoue proprement.
	erreurs.Reset()
	if code := lancer([]string{"-config", conf, "-verifier"}, &sortie, &erreurs); code == 0 || !strings.Contains(erreurs.String(), "token") {
		t.Errorf("-verifier sans token : %d %q", code, erreurs.String())
	}
}
