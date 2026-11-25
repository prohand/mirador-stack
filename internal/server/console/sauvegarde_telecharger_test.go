// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"mirador-stack/internal/server/sauvegarde"
)

// Télécharger une sauvegarde (retour du 25/11) : lien sur l'exécution dont
// l'archive est encore sur le VPS et bouton « la dernière » ; le fichier est
// envoyé tel quel (chiffré), noté dans l'audit ; tout autre nom est refusé.
func TestTelechargerSauvegarde(t *testing.T) {
	b := nouveauBanc(t)
	b.c.opt.Donnees = t.TempDir()
	dossier := sauvegarde.DossierArchives(b.c.opt.Donnees)
	if err := os.MkdirAll(dossier, 0o700); err != nil {
		t.Fatal(err)
	}
	nom := "mirador-20261125-120000.sauvegarde"
	if err := os.WriteFile(filepath.Join(dossier, nom), []byte("contenu chiffré"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(b.c.opt.Donnees, "secret.txt"), []byte("à ne pas servir"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := b.c.db.Exec(`INSERT INTO sauvegardes(demandee_le, origine, metriques, logs, statut, fichier, taille) VALUES (1, 'planifiee', 0, 0, 'ok', ?, 15),
		(2, 'planifiee', 0, 0, 'ok', ?, 15)`, filepath.Join(dossier, nom), filepath.Join(dossier, "mirador-20261101-120000.sauvegarde")); err != nil {
		t.Fatal(err)
	}
	cl, _ := b.premierAdmin("admin", mdpAdmin)
	_, page, _ := b.get(cl, "/reglages/sauvegardes")
	lien := `href="/reglages/sauvegardes/telecharger/` + nom + `"`
	if strings.Count(page, lien) != 2 || !strings.Contains(page, "Télécharger la dernière") {
		t.Errorf("liens de téléchargement : %d (une ligne de l'historique + « la dernière »)", strings.Count(page, lien))
	}
	if strings.Contains(page, "telecharger/mirador-20261101") {
		t.Error("lien vers une archive qui n'est plus sur le VPS")
	}
	code, corps, h := b.get(cl, "/reglages/sauvegardes/telecharger/"+nom)
	if code != 200 || corps != "contenu chiffré" || !strings.Contains(h.Get("Content-Disposition"), "attachment") {
		t.Fatalf("téléchargement : %d %q %q", code, corps, h.Get("Content-Disposition"))
	}
	var n int
	_ = b.c.db.QueryRow(`SELECT COUNT(*) FROM audit WHERE action = 'sauvegarde_telechargee' AND objet = ?`, nom).Scan(&n)
	if n != 1 {
		t.Error("téléchargement absent de l'audit")
	}
	for _, mauvais := range []string{"secret.txt", "..%2Fsecret.txt", "mirador-20261101-120000.sauvegarde"} {
		if code, _, _ := b.get(cl, "/reglages/sauvegardes/telecharger/"+mauvais); code != 404 {
			t.Errorf("%s : %d au lieu de 404", mauvais, code)
		}
	}
}
