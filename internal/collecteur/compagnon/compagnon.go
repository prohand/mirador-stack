// SPDX-License-Identifier: GPL-3.0-or-later

// Package compagnon : relevé des processus de la machine du collecteur par
// un second service, sans réseau (SPEC § 120.1, retour du 22/11 : « voir
// tous les processus mais sécuriser »).
//
// Pourquoi deux services : mirador-collector écoute sur le LAN ; son unité
// systemd lui cache les processus des autres comptes (ProtectProc=invisible).
// Le compagnon, lui, voit toute la machine mais ne peut ni écouter ni
// envoyer (aucune famille d'adresses réseau, IPAddressDeny=any), tourne sous
// son propre compte (mirador-processus) et s'exécute depuis un binaire posé
// par root, que le collecteur ne peut pas remplacer. Il ne lit que
// /proc/<pid>/stat, le compte et le cgroup : jamais les lignes de commande.
//
// Échanges, par fichiers seulement :
//   - collecteur → compagnon : un repère vide (/run/mirador-collector/direct),
//     touché tant qu'une page regarde le collecteur en direct ; le compagnon
//     n'en lit que la date ;
//   - compagnon → collecteur : direct.json (chaque seconde pendant le
//     direct) et minute.json (chaque minute, « cause probable » des
//     alertes), dans /run/mirador-processus (0644 : ce que « ps » montre
//     déjà à tout compte de la machine).
package compagnon

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"os"
	"path/filepath"
	"time"
	"unicode/utf8"

	"mirador-stack/internal/agent/direct"
	"mirador-stack/internal/protocole"
)

// Chemins en service (unités systemd du script d'installation).
const (
	DossierSortie = "/run/mirador-processus"
	RepereDirect  = "/run/mirador-collector/direct"
)

const (
	fichierDirect = "direct.json"
	fichierMinute = "minute.json"
	// Le repère est touché au plus toutes les 5 s ; au-delà de 15 s sans
	// mise à jour, le direct est fini.
	fraicheurRepere = 15 * time.Second
	toucherRepere   = 5 * time.Second
	// Lectures côté collecteur : relevé direct de moins de 5 s, relevé
	// minute de moins de 3 min (sinon le compagnon est arrêté ou absent).
	fraicheurDirect = 5 * time.Second
	fraicheurMinute = 3 * time.Minute
	tailleMax       = 64 << 10
	// NbMinute : processus gardés par classement (comme l'agent).
	NbMinute = 5
	version  = 1
)

// Fichier : contenu de direct.json et minute.json.
type Fichier struct {
	V         int                         `json:"v"`
	T         int64                       `json:"t"` // millisecondes Unix
	Processus []protocole.ProcessusDirect `json:"processus,omitempty"`
	CPU       []protocole.ProcessusDirect `json:"cpu,omitempty"`
	Memoire   []protocole.ProcessusDirect `json:"mem,omitempty"`
}

// Compagnon : la boucle du service mirador-collector-processus.
type Compagnon struct {
	Sortie, Repere string
	Proc           string // « /proc » en service
	Journal        *slog.Logger
	Maintenant     func() time.Time
	periodeMinute  time.Duration // une minute (plus court dans les tests)
}

// Tourner : une boucle par seconde ; relevé minute toujours, relevé
// direct seulement quand le repère est frais.
func (c *Compagnon) Tourner(ctx context.Context) error {
	if c.Maintenant == nil {
		c.Maintenant = time.Now
	}
	if c.periodeMinute <= 0 {
		c.periodeMinute = time.Minute
	}
	rDirect, rMinute := direct.Nouveau(), direct.Nouveau()
	if c.Proc != "" {
		rDirect.Proc, rMinute.Proc = c.Proc, c.Proc
	}
	rMinute.Classement(c.Maintenant(), NbMinute) // compteurs de départ
	prochaineMinute := c.Maintenant().Add(c.periodeMinute)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	enDirect := false
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
		m := c.Maintenant()
		if c.repereFrais(m) {
			l := rDirect.ReleverProcessus(m)
			if enDirect { // le premier relevé ne connaît pas encore le CPU
				c.ecrire(fichierDirect, Fichier{V: version, T: m.UnixMilli(), Processus: l})
			}
			enDirect = true
		} else if enDirect {
			enDirect = false
			_ = os.Remove(filepath.Join(c.Sortie, fichierDirect))
		}
		if !m.Before(prochaineMinute) {
			cpu, mem := rMinute.Classement(m, NbMinute)
			c.ecrire(fichierMinute, Fichier{V: version, T: m.UnixMilli(), CPU: cpu, Memoire: mem})
			prochaineMinute = m.Add(c.periodeMinute)
		}
	}
}

func (c *Compagnon) repereFrais(m time.Time) bool {
	st, err := os.Stat(c.Repere)
	return err == nil && m.Sub(st.ModTime()) < fraicheurRepere
}

// ecrire : fichier remplacé d'un coup (écriture à côté, puis renommage).
func (c *Compagnon) ecrire(nom string, f Fichier) {
	b, err := json.Marshal(f)
	if err != nil {
		return
	}
	tmp := filepath.Join(c.Sortie, "."+nom+".tmp")
	if err := os.WriteFile(tmp, b, 0o644); err != nil { // #nosec G306 -- lisible par le collecteur ; rien de plus que « ps »
		c.Journal.Warn("écriture impossible", "fichier", tmp, "erreur", err)
		return
	}
	if err := os.Rename(tmp, filepath.Join(c.Sortie, nom)); err != nil {
		c.Journal.Warn("écriture impossible", "fichier", nom, "erreur", err)
	}
}

// Lecteur : côté mirador-collector.
type Lecteur struct {
	Dossier, Repere string
	Maintenant      func() time.Time

	derniereTouche time.Time
}

// NouveauLecteur : chemins en service.
func NouveauLecteur() *Lecteur {
	return &Lecteur{Dossier: DossierSortie, Repere: RepereDirect, Maintenant: time.Now}
}

// Present : le compagnon tourne (relevé minute récent).
func (l *Lecteur) Present() bool {
	_, ok := l.lire(fichierMinute, fraicheurMinute)
	return ok
}

// ProcessusDirect : pour direct.Releveur.SourceProcessus. Touche le repère
// (le compagnon passe au rythme d'une seconde) et rend son dernier relevé.
// Compagnon absent : ok faux (relevé local). Compagnon présent mais direct
// pas encore commencé : liste vide (une ou deux secondes).
func (l *Lecteur) ProcessusDirect(m time.Time) ([]protocole.ProcessusDirect, bool) {
	l.toucher(m)
	if f, ok := l.lire(fichierDirect, fraicheurDirect); ok {
		return f.Processus, true
	}
	if l.Present() {
		return nil, true
	}
	return nil, false
}

// Minute : dernier classement du compagnon (« cause probable »).
func (l *Lecteur) Minute() (cpu, mem []protocole.ProcessusDirect, ok bool) {
	f, ok := l.lire(fichierMinute, fraicheurMinute)
	return f.CPU, f.Memoire, ok
}

func (l *Lecteur) toucher(m time.Time) {
	if m.Sub(l.derniereTouche) < toucherRepere {
		return
	}
	l.derniereTouche = m
	if err := os.Chtimes(l.Repere, m, m); err != nil {
		if f, err := os.OpenFile(l.Repere, os.O_CREATE|os.O_WRONLY, 0o644); err == nil { // #nosec G302 G304 -- repère vide, chemin figé
			_ = f.Close()
		}
	}
}

// lire : fichier du compagnon, borné et vérifié (il vient d'un autre
// service : jamais pris tel quel).
func (l *Lecteur) lire(nom string, fraicheur time.Duration) (Fichier, bool) {
	var f Fichier
	fh, err := os.Open(filepath.Join(l.Dossier, nom)) // #nosec G304 -- dossier figé, nom constant
	if err != nil {
		return f, false
	}
	defer fh.Close()
	b, err := io.ReadAll(io.LimitReader(fh, tailleMax+1))
	if err != nil || len(b) > tailleMax || json.Unmarshal(b, &f) != nil || f.V != version {
		return Fichier{}, false
	}
	age := l.Maintenant().Sub(time.UnixMilli(f.T))
	if age > fraicheur || age < -fraicheur {
		return Fichier{}, false
	}
	if valider(f.Processus, protocole.DirectMaxProcessus) != nil || valider(f.CPU, NbMinute) != nil || valider(f.Memoire, NbMinute) != nil {
		return Fichier{}, false
	}
	return f, true
}

var errHorsBornes = errors.New("relevé du compagnon hors bornes")

func valider(l []protocole.ProcessusDirect, max int) error {
	if len(l) > max {
		return errHorsBornes
	}
	for _, p := range l {
		if p.PID <= 0 || math.IsNaN(p.CPU) || math.IsInf(p.CPU, 0) || p.CPU < 0 || p.CPU > 409600 ||
			math.IsNaN(p.Memoire) || math.IsInf(p.Memoire, 0) || p.Memoire < 0 || p.Memoire > 1e15 {
			return errHorsBornes
		}
		for _, t := range []string{p.Nom, p.Utilisateur, p.Groupe} {
			if len(t) > 64 || !utf8.ValidString(t) {
				return errHorsBornes
			}
		}
	}
	return nil
}
