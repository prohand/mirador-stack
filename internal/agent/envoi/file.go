// SPDX-License-Identifier: GPL-3.0-or-later

// Package envoi gère la file d'attente des lots et leur envoi en HTTPS.
//
// Règles (SPEC.md § 6.3) :
//   - la file est en mémoire et bornée en octets ;
//   - pleine, elle jette les lots les plus anciens (et les compte) ;
//   - jamais d'écriture disque.
package envoi

import "sync"

// File est une file FIFO de lots (octets déjà compressés), bornée en taille.
type File struct {
	mu     sync.Mutex
	lots   [][]byte
	taille int // somme des longueurs des lots en attente
	max    int
	jetes  uint64 // lots jetés depuis le démarrage
	signal chan struct{}
	libres [][]byte // tampons rendus après envoi, réutilisés par Prendre
}

// NewFile crée une file bornée à max octets.
func NewFile(max int) *File {
	return &File{max: max, signal: make(chan struct{}, 1)}
}

// Ajouter met un lot en fin de file. Si la place manque, les plus anciens sont
// jetés. Un lot plus gros que toute la file est refusé (renvoie false).
func (f *File) Ajouter(lot []byte) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(lot) > f.max {
		f.jetes++
		return false
	}
	for f.taille+len(lot) > f.max && len(f.lots) > 0 {
		f.taille -= len(f.lots[0])
		// Pas de recyclage ici : l'expéditeur est peut-être en train d'envoyer
		// ce lot. Le ramasse-miettes le libérera après.
		f.lots[0] = nil
		f.lots = f.lots[1:]
		f.jetes++
	}
	f.lots = append(f.lots, lot)
	f.taille += len(lot)
	select {
	case f.signal <- struct{}{}:
	default:
	}
	return true
}

// Premier renvoie le plus ancien lot sans le retirer (nil si la file est vide).
func (f *File) Premier() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.lots) == 0 {
		return nil
	}
	return f.lots[0]
}

// Retirer enlève le plus ancien lot s'il s'agit toujours de lot (il a pu être
// jeté entre-temps pour faire de la place).
func (f *File) Retirer(lot []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(lot) == 0 || len(f.lots) == 0 || len(f.lots[0]) == 0 || &f.lots[0][0] != &lot[0] {
		return
	}
	f.taille -= len(lot)
	f.recycler(lot)
	f.lots[0] = nil
	f.lots = f.lots[1:]
}

// Signal se déclenche quand un lot est ajouté.
func (f *File) Signal() <-chan struct{} { return f.signal }

// Etat renvoie le nombre de lots, les octets en attente et les lots jetés.
func (f *File) Etat() (lots, octets int, jetes uint64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.lots), f.taille, f.jetes
}

// Prendre renvoie un tampon libre à remplir (évite une allocation par cycle).
func (f *File) Prendre() []byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	if n := len(f.libres); n > 0 {
		b := f.libres[n-1]
		f.libres = f.libres[:n-1]
		return b[:0]
	}
	return nil
}

// recycler garde au plus 4 tampons libres. Appelé avec le verrou pris.
func (f *File) recycler(b []byte) {
	if len(f.libres) < 4 {
		f.libres = append(f.libres, b[:0])
	}
}
