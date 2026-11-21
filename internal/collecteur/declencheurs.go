// SPDX-License-Identifier: GPL-3.0-or-later

package collecteur

import (
	"log/slog"
	"sync"
)

// Declencheurs : « Vérifier maintenant » d'une sonde (SPEC § 108). Le
// serveur donne des noms de sondes ; seules celles déjà dans la
// configuration du collecteur ont un canal : un autre nom est ignoré (rien
// d'autre ne peut être lancé à distance).
type Declencheurs struct {
	mu      sync.Mutex
	canaux  map[string]chan struct{}
	Journal *slog.Logger
}

// Canal : signal de la sonde nom (créé au besoin). Sur un Declencheurs nil
// (essais, ou ancien appel) : nil, c'est-à-dire jamais de signal.
func (d *Declencheurs) Canal(nom string) <-chan struct{} {
	if d == nil {
		return nil
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.canaux == nil {
		d.canaux = map[string]chan struct{}{}
	}
	if d.canaux[nom] == nil {
		d.canaux[nom] = make(chan struct{}, 1)
	}
	return d.canaux[nom]
}

// Declencher : essai tout de suite des sondes connues parmi noms.
func (d *Declencheurs) Declencher(noms []string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, nom := range noms {
		ch := d.canaux[nom]
		if ch == nil {
			continue // sonde inconnue de ce collecteur
		}
		select {
		case ch <- struct{}{}:
			if d.Journal != nil {
				d.Journal.Info("essai de sonde demandé", "sonde", nom)
			}
		default: // déjà demandé, pas encore fait
		}
	}
}
