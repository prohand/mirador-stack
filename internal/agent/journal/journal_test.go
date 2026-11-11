// SPDX-License-Identifier: GPL-3.0-or-later

package journal

import (
	"bufio"
	"context"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// Lignes au format de « journalctl -o json » (Debian 12), dont un MESSAGE
// en tableau d'octets (codes couleur) et une ligne abîmée.
const sortieJournal = `{"__CURSOR":"s=abc;i=1;b=def;m=1;t=600;x=1","__REALTIME_TIMESTAMP":"1762596000123456","MESSAGE":"Accepted publickey for bob","PRIORITY":"6","_SYSTEMD_UNIT":"ssh.service","SYSLOG_IDENTIFIER":"sshd","_TRANSPORT":"syslog"}
{"__CURSOR":"s=abc;i=2;b=def;m=2;t=601;x=2","__REALTIME_TIMESTAMP":"1762596001000000","MESSAGE":[27,91,51,49,109,69,82,82,27,91,48,109],"PRIORITY":"3","SYSLOG_IDENTIFIER":"app","_TRANSPORT":"stdout"}
pas du json
{"__CURSOR":"s=abc;i=3;b=def;m=3;t=602;x=3","__REALTIME_TIMESTAMP":"1762596002000000","MESSAGE":"Out of memory: Killed process 1234 (java)","PRIORITY":"3","SYSLOG_IDENTIFIER":"kernel","_TRANSPORT":"kernel"}
`

func TestAnalyser(t *testing.T) {
	var e Entree
	sc := bufio.NewScanner(strings.NewReader(sortieJournal))
	var lues []Entree
	for sc.Scan() {
		if analyser(sc.Bytes(), &e) {
			lues = append(lues, e)
		}
	}
	if len(lues) != 3 {
		t.Fatalf("3 entrées attendues, %d", len(lues))
	}
	if lues[0].Unite != "ssh.service" || lues[0].Niveau() != "info" || lues[0].Temps.UnixMicro() != 1762596000123456 {
		t.Errorf("entrée 1 mal lue : %+v", lues[0])
	}
	if lues[1].Message != "\x1b[31mERR\x1b[0m" || lues[1].Niveau() != "err" {
		t.Errorf("MESSAGE en octets mal lu : %q", lues[1].Message)
	}
	if lues[2].Transport != "kernel" {
		t.Errorf("entrée 3 mal lue : %+v", lues[2])
	}
}

// fauxJournalctl écrit un script qui note ses arguments puis imprime les lignes.
func fauxJournalctl(t *testing.T, dir string) string {
	t.Helper()
	donnees := filepath.Join(dir, "sortie.json")
	if err := os.WriteFile(donnees, []byte(sortieJournal), 0o600); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(dir, "journalctl")
	contenu := "#!/bin/sh\necho \"$@\" >> " + filepath.Join(dir, "args") + "\ncat " + donnees + "\nsleep 30\n"
	if err := os.WriteFile(script, []byte(contenu), 0o700); err != nil { // #nosec G306 -- script de test exécutable
		t.Fatal(err)
	}
	return script
}

func TestLecteurEtCurseur(t *testing.T) {
	dir := t.TempDir()
	curseur := filepath.Join(dir, "journal.cursor")
	l := NewLecteur(Options{
		FichierCurseur: curseur,
		Commande:       fauxJournalctl(t, dir),
		Journal:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	var mu sync.Mutex
	var recues []string
	ctx, annuler := context.WithCancel(context.Background())
	fini := make(chan struct{})
	go func() {
		l.Tourner(ctx, func(e *Entree) {
			mu.Lock()
			recues = append(recues, e.Message)
			mu.Unlock()
		})
		close(fini)
	}()
	fin := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(recues)
		mu.Unlock()
		if n == 3 {
			break
		}
		if time.Now().After(fin) {
			t.Fatalf("%d entrées reçues sur 3", n)
		}
		time.Sleep(10 * time.Millisecond)
	}
	annuler()
	<-fini
	if l.Illisibles.Load() != 1 {
		t.Errorf("1 ligne illisible attendue, %d", l.Illisibles.Load())
	}
	// À l'arrêt, le curseur est écrit (position de la dernière ligne).
	c, err := os.ReadFile(curseur)
	if err != nil || strings.TrimSpace(string(c)) != "s=abc;i=3;b=def;m=3;t=602;x=3" {
		t.Fatalf("curseur incorrect : %q, %v", c, err)
	}
	args, _ := os.ReadFile(filepath.Join(dir, "args"))
	for _, attendu := range []string{"--follow", "-o json", "-p 0..6", "-n 0"} {
		if !strings.Contains(string(args), attendu) {
			t.Errorf("option %q absente : %s", attendu, args)
		}
	}

	// Redémarrage : le lecteur reprend après le curseur, au plus 12 h en arrière.
	l2 := NewLecteur(Options{FichierCurseur: curseur, Commande: "inutilisé"})
	a := strings.Join(l2.arguments(), " ")
	if !strings.Contains(a, "--after-cursor=s=abc;i=3;b=def;m=3;t=602;x=3") || !strings.Contains(a, "--since=-12h") {
		t.Errorf("reprise incorrecte : %s", a)
	}
}

func TestCurseurInvalideIgnore(t *testing.T) {
	dir := t.TempDir()
	curseur := filepath.Join(dir, "journal.cursor")
	if err := os.WriteFile(curseur, []byte("s=1; rm -rf /\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	l := NewLecteur(Options{FichierCurseur: curseur})
	if strings.Contains(strings.Join(l.arguments(), " "), "after-cursor") {
		t.Error("un curseur mal formé doit être ignoré")
	}
}

func FuzzAnalyser(f *testing.F) {
	for _, l := range strings.Split(sortieJournal, "\n") {
		f.Add([]byte(l))
	}
	f.Fuzz(func(t *testing.T, b []byte) {
		var e Entree
		analyser(b, &e)
	})
}
