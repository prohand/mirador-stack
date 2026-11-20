// SPDX-License-Identifier: GPL-3.0-or-later

package collect

import (
	"strconv"
	"time"

	"mirador-stack/internal/agent/direct"
	"mirador-stack/internal/agent/metrics"
)

// Processus : les 5 processus les plus gourmands en CPU et en mémoire,
// relevés chaque minute (collecteur lent) pour la « cause probable » des
// alertes (SPEC § 98) :
//
//	mirador_processus_cpu_pourcent{rang,nom,groupe}   % d'un cœur sur la minute
//	mirador_processus_memoire_octets{rang,nom,groupe} mémoire résidente
//
// Le nom (comm) seulement, pas le pid : le nombre de séries reste petit.
type Processus struct {
	r *direct.Releveur
}

// NewProcessus : relevé dans proc (« /proc » en service).
func NewProcessus(proc string) *Processus {
	r := direct.Nouveau()
	if proc != "" {
		r.Proc = proc
	}
	return &Processus{r: r}
}

func (p *Processus) Nom() string { return "processus" }
func (p *Processus) Lent() bool  { return true }

const nbProcessus = 5

func (p *Processus) Collecter(b *metrics.Batch) error {
	cpu, mem := p.r.Classement(time.Now(), nbProcessus)
	for i, x := range cpu {
		b.Add("mirador_processus_cpu_pourcent", x.CPU, metrics.L("rang", strconv.Itoa(i+1)), metrics.L("nom", x.Nom), metrics.L("groupe", x.Groupe))
	}
	for i, x := range mem {
		b.Add("mirador_processus_memoire_octets", x.Memoire, metrics.L("rang", strconv.Itoa(i+1)), metrics.L("nom", x.Nom), metrics.L("groupe", x.Groupe))
	}
	return nil
}
