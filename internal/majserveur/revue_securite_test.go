// SPDX-License-Identifier: GPL-3.0-or-later

package majserveur

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// B13 (revue du 28/11) : un serveur piraté écrit d'avance serveur.json
// (« 0.6.0, démarré dans 1 h ») ; la 0.6.0 ne démarre pas. La présence est
// effacée après l'arrêt : retour arrière fait, pas de faux « ok ».
func TestPresenceEcriteDAvance(t *testing.T) {
	b := nouveauBanc(t)
	b.d.demarre["0.6.0"] = false
	b.publier(t, "0.6.0")
	b.demander(t, "0.6.0")
	faux := Presence{Version: "0.6.0", DemarreLe: b.horloge.Add(time.Hour).Unix()}
	if err := EcrireJSON(b.h.R.DossierDemande, NomPresence, faux, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := b.h.Appliquer(context.Background()); err == nil {
		t.Fatal("faux succès : présence écrite d'avance acceptée")
	}
	if e := b.etat(t); e.Etat != EtatEchec || !strings.Contains(e.Message, "retour à 0.5.0 fait") {
		t.Fatalf("état : %+v", e)
	}
	if v, _ := b.h.VersionInstallee(); v != "0.5.0" {
		t.Errorf("version installée : %q", v)
	}
}

// B13 : une date de démarrage dans le futur (> maintenant + 1 min) est
// refusée ; dans la minute, acceptée (décalage d'horloge).
func TestPresenceDateFuture(t *testing.T) {
	b := nouveauBanc(t)
	debut := b.horloge
	for _, c := range []struct {
		decalage time.Duration
		ok       bool
	}{{2 * time.Minute, false}, {time.Hour, false}, {30 * time.Second, true}, {0, true}, {-time.Second, false}} {
		p := Presence{Version: "0.6.0", DemarreLe: debut.Add(c.decalage).Unix()}
		if err := EcrireJSON(b.h.R.DossierDemande, NomPresence, p, 0o644); err != nil {
			t.Fatal(err)
		}
		b.horloge = debut
		if got := b.h.attendre(context.Background(), "0.6.0", debut); got != c.ok {
			t.Errorf("démarré à début %+v : accepté = %v, attendu %v", c.decalage, got, c.ok)
		}
	}
}

// B14 : le conteneur remplace son journal WAL par un lien symbolique vers un
// fichier du VPS. La copie est refusée (serveur relancé sans changement) et
// la cible n'est jamais créée ni modifiée.
func TestCopieBaseLienRefuse(t *testing.T) {
	b := nouveauBanc(t)
	cible := filepath.Join(t.TempDir(), "cron-root")
	b.d.liens = map[string]string{b.h.R.FichierBase + "-wal": cible}
	delete(b.d.fichiers, b.h.R.FichierBase+"-wal")
	b.publier(t, "0.6.0")
	b.demander(t, "0.6.0")
	if err := b.h.Appliquer(context.Background()); err == nil {
		t.Fatal("copie avec lien symbolique acceptée")
	}
	if e := b.etat(t); e.Etat != EtatEchec || !strings.Contains(e.Message, "copie de la base impossible") {
		t.Fatalf("état : %+v", e)
	}
	if _, err := os.Lstat(cible); !os.IsNotExist(err) {
		t.Error("cible du lien créée")
	}
	// Copie refusée effacée, .env inchangé.
	es, _ := os.ReadDir(b.h.R.DossierSauvegardes)
	for _, e := range es {
		if strings.HasPrefix(e.Name(), "avant-") {
			t.Errorf("copie refusée gardée : %s", e.Name())
		}
	}
	env, _ := os.ReadFile(filepath.Join(b.h.R.DossierCompose, ".env"))
	if !strings.Contains(string(env), "MIRADOR_VERSION=0.5.0") {
		t.Errorf(".env modifié : %s", env)
	}
}

// B14 : remettreBase ne suit jamais un lien (même s'il apparaît dans la
// copie) et crée les fichiers manquants sans écraser ni suivre.
func TestRemettreBaseLien(t *testing.T) {
	b := nouveauBanc(t)
	copie := t.TempDir()
	nom := filepath.Base(b.h.R.FichierBase)
	if err := os.WriteFile(filepath.Join(copie, nom), []byte("base"), 0o600); err != nil {
		t.Fatal(err)
	}
	cible := filepath.Join(t.TempDir(), "passwd")
	if err := os.Symlink(cible, filepath.Join(copie, nom+"-shm")); err != nil {
		t.Fatal(err)
	}
	if err := b.h.remettreBase(context.Background(), copie); err == nil {
		t.Fatal("lien symbolique accepté")
	}
	if _, err := os.Lstat(cible); !os.IsNotExist(err) {
		t.Error("cible du lien créée")
	}
	if len(b.d.appels) != 0 {
		t.Errorf("copie rendue au conteneur malgré le lien : %q", b.d.appels)
	}
	// Sans lien : -wal et -shm manquants créés vides (fichiers ordinaires).
	_ = os.Remove(filepath.Join(copie, nom+"-shm"))
	if err := b.h.remettreBase(context.Background(), copie); err != nil {
		t.Fatal(err)
	}
	for _, suf := range []string{"-wal", "-shm"} {
		if err := fichierOrdinaire(filepath.Join(copie, nom+suf)); err != nil {
			t.Error(err)
		}
	}
}
