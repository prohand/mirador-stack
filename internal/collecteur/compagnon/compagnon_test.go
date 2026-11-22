// SPDX-License-Identifier: GPL-3.0-or-later

package compagnon

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"mirador-stack/internal/protocole"
)

// faux /proc : deux processus (stat lisible, 22 champs après le nom).
func fauxProc(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	for pid, nom := range map[int]string{101: "postgres", 202: "nginx: worker"} {
		dir := filepath.Join(d, fmt.Sprint(pid))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		champs := make([]string, 22)
		for i := range champs {
			champs[i] = "0"
		}
		champs[0], champs[11], champs[21] = "S", "50", "1000"
		stat := fmt.Sprintf("%d (%s) %s\n", pid, nom, strings.Join(champs, " "))
		if err := os.WriteFile(filepath.Join(dir, "stat"), []byte(stat), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return d
}

// Le compagnon écrit minute.json chaque « minute », et direct.json seulement
// quand le repère est frais ; le lecteur du collecteur les relit.
func TestCompagnonEtLecteur(t *testing.T) {
	sortie, run := t.TempDir(), t.TempDir()
	repere := filepath.Join(run, "direct")
	c := &Compagnon{Sortie: sortie, Repere: repere, Proc: fauxProc(t),
		Journal: slog.New(slog.NewTextHandler(io.Discard, nil)), periodeMinute: time.Second}
	l := &Lecteur{Dossier: sortie, Repere: repere, Maintenant: time.Now}

	if _, ok := l.ProcessusDirect(time.Now()); ok {
		t.Fatal("sans compagnon : le collecteur doit garder son relevé local")
	}
	if _, err := os.Stat(repere); err != nil {
		t.Fatal("repère du direct non posé par le collecteur")
	}
	ctx, arreter := context.WithTimeout(context.Background(), 3500*time.Millisecond)
	defer arreter()
	go func() { _ = c.Tourner(ctx) }()
	<-ctx.Done()
	time.Sleep(100 * time.Millisecond)

	cpu, mem, ok := l.Minute()
	if !ok || len(mem) != 2 || cpu != nil && len(cpu) > NbMinute {
		t.Fatalf("relevé minute : %v %v %v", cpu, mem, ok)
	}
	if mem[0].Memoire <= 0 || mem[0].Nom == "" {
		t.Errorf("mémoire : %+v", mem[0])
	}
	l.derniereTouche = time.Time{}
	if _, ok := l.ProcessusDirect(time.Now()); !ok {
		t.Error("compagnon présent : relevé du compagnon attendu")
	}
	if _, err := os.Stat(filepath.Join(sortie, fichierDirect)); err != nil {
		t.Error("direct.json absent alors que le repère est frais")
	}
	// Repère ancien : plus de relevé direct.
	vieux := time.Now().Add(-time.Minute)
	_ = os.Chtimes(repere, vieux, vieux)
	if c.repereFrais(time.Now()) {
		t.Error("repère ancien pris pour frais")
	}
}

// Fichiers du compagnon : jamais pris tels quels (périmés, trop gros, hors
// bornes ou d'une autre version : ignorés).
func TestLecteurRefuse(t *testing.T) {
	d := t.TempDir()
	l := &Lecteur{Dossier: d, Repere: filepath.Join(d, "r"), Maintenant: time.Now}
	ecrire := func(f any) {
		b, _ := json.Marshal(f)
		if err := os.WriteFile(filepath.Join(d, fichierMinute), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	ok := protocole.ProcessusDirect{PID: 1, Nom: "init", Memoire: 10}
	ecrire(Fichier{V: version, T: time.Now().UnixMilli(), Memoire: []protocole.ProcessusDirect{ok}})
	if _, _, bon := l.Minute(); !bon {
		t.Fatal("fichier correct refusé")
	}
	for nom, f := range map[string]Fichier{
		"périmé":        {V: version, T: time.Now().Add(-time.Hour).UnixMilli(), Memoire: []protocole.ProcessusDirect{ok}},
		"autre version": {V: 99, T: time.Now().UnixMilli()},
		"pid nul":       {V: version, T: time.Now().UnixMilli(), Memoire: []protocole.ProcessusDirect{{Nom: "x"}}},
		"trop":          {V: version, T: time.Now().UnixMilli(), Memoire: []protocole.ProcessusDirect{ok, ok, ok, ok, ok, ok}},
		"nom trop long": {V: version, T: time.Now().UnixMilli(), Memoire: []protocole.ProcessusDirect{{PID: 1, Nom: strings.Repeat("a", 65)}}},
		"cpu négatif":   {V: version, T: time.Now().UnixMilli(), CPU: []protocole.ProcessusDirect{{PID: 1, CPU: -1}}},
	} {
		ecrire(f)
		if _, _, bon := l.Minute(); bon {
			t.Errorf("%s : accepté", nom)
		}
	}
	if err := os.WriteFile(filepath.Join(d, fichierMinute), []byte(strings.Repeat(" ", tailleMax+10)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, _, bon := l.Minute(); bon {
		t.Error("fichier trop gros accepté")
	}
}
