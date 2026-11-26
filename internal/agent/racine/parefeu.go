// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"bufio"
	"bytes"
	"errors"
	"regexp"
	"strings"
)

// PareFeu : politique par défaut de la chaîne INPUT (même logique
// qu'obs-firewall.sh, SPEC.md § 4.2).
type PareFeu struct {
	Outil     string `json:"outil"`     // iptables, nftables, aucun
	Politique string `json:"politique"` // DROP, REJECT, ACCEPT, AUCUNE
	Regles    int    `json:"regles"`    // nombre de règles dans INPUT
	// Gestion : outil qui pose les règles (ufw, firewalld), vide sinon.
	Gestion string `json:"gestion,omitempty"`
	// Journal : règles qui journalisent les paquets (SPEC § 136) ; nil :
	// assistant trop ancien pour le dire.
	Journal *JournalPareFeu `json:"journal,omitempty"`
}

// JournalPareFeu : règles de journalisation trouvées (LOG, NFLOG, log).
type JournalPareFeu struct {
	Regles     int      `json:"regles"`
	SansLimite int      `json:"sans_limite"` // sans limite de débit
	Prefixes   []string `json:"prefixes,omitempty"`
}

// Bornes des préfixes rapportés.
const (
	maxPrefixesJournal = 10
	maxTaillePrefixe   = 40
)

// Restrictif dit si la politique bloque par défaut.
func (p *PareFeu) Restrictif() bool {
	return p.Politique == "DROP" || p.Politique == "REJECT"
}

// lirePareFeu : iptables d'abord (couvre ufw et iptables-nft), puis nftables
// natif si iptables n'a rien donné de restrictif.
func lirePareFeu(prog programmes) (*PareFeu, error) {
	pf := &PareFeu{Outil: "aucun", Politique: "AUCUNE", Journal: &JournalPareFeu{}}
	sortie, errIpt := prog.iptablesInput()
	if errIpt == nil {
		if pol, n, ok := analyserIptables(sortie); ok {
			pf.Outil, pf.Politique, pf.Regles = "iptables", pol, n
		}
		journalIptables(sortie, pf)
	}
	// nftables natif : lu si iptables n'a rien donné de restrictif, ou pour
	// trouver une journalisation qu'iptables ne montre pas.
	if !pf.Restrictif() || pf.Journal.Regles == 0 {
		if rs, err := prog.nftRuleset(); err == nil {
			if n, drop := analyserNft(rs); drop && !pf.Restrictif() {
				pf.Outil, pf.Politique, pf.Regles = "nftables", "DROP", n
			}
			journalNft(rs, pf)
		} else if errIpt != nil {
			// Ni iptables ni nft utilisables : on ne sait rien.
			if errors.Is(errIpt, errAbsent) && errors.Is(err, errAbsent) {
				return pf, nil // aucun outil : politique « AUCUNE », alerte voulue
			}
			return nil, errIpt
		}
	}
	return pf, nil
}

var (
	reLogPrefixe = regexp.MustCompile(`--(?:log|nflog)-prefix\s+(?:"([^"]*)"|(\S+))`)
	reNftPrefixe = regexp.MustCompile(`\blog\b.*?\bprefix\s+"([^"]*)"`)
	reNftLog     = regexp.MustCompile(`(^|\s)log(\s|$)`)
)

// journalIptables : règles « -j LOG » ou « -j NFLOG » de toutes les chaînes
// (sortie de « iptables -S ») ; chaînes « ufw-… » : règles posées par ufw.
func journalIptables(sortie []byte, pf *PareFeu) {
	sc := bufio.NewScanner(bytes.NewReader(sortie))
	for sc.Scan() {
		l := sc.Text()
		if strings.Contains(l, " ufw-") || strings.HasPrefix(l, "-N ufw-") {
			pf.Gestion = "ufw"
		}
		if strings.Contains(l, "IN_public") || strings.Contains(l, "INPUT_direct") {
			pf.Gestion = "firewalld"
		}
		if !strings.HasPrefix(l, "-A ") || (!strings.Contains(l, " -j LOG") && !strings.Contains(l, " -j NFLOG")) {
			continue
		}
		p := ""
		if m := reLogPrefixe.FindStringSubmatch(l); m != nil {
			p = m[1] + m[2]
		}
		noterJournal(pf.Journal, p, strings.Contains(l, "-m limit") || strings.Contains(l, "-m hashlimit"))
	}
}

// journalNft : instructions « log » de « nft list ruleset ».
func journalNft(rs []byte, pf *PareFeu) {
	sc := bufio.NewScanner(bytes.NewReader(rs))
	for sc.Scan() {
		l := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(l, "table inet firewalld") || strings.HasPrefix(l, "table ip firewalld") {
			pf.Gestion = "firewalld"
		}
		if strings.HasPrefix(l, "chain ") || strings.HasPrefix(l, "type ") || !reNftLog.MatchString(l) {
			continue
		}
		p := ""
		if m := reNftPrefixe.FindStringSubmatch(l); m != nil {
			p = m[1]
		}
		noterJournal(pf.Journal, p, strings.Contains(l, "limit rate"))
	}
}

// noterJournal : une règle de journalisation (préfixe nettoyé et borné).
func noterJournal(j *JournalPareFeu, prefixe string, limite bool) {
	j.Regles++
	if !limite {
		j.SansLimite++
	}
	prefixe = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f || r == '"' || r == '\\' {
			return -1
		}
		return r
	}, strings.TrimSpace(prefixe))
	if prefixe == "" || len(prefixe) > maxTaillePrefixe || len(j.Prefixes) >= maxPrefixesJournal {
		return
	}
	for _, x := range j.Prefixes {
		if x == prefixe {
			return
		}
	}
	j.Prefixes = append(j.Prefixes, prefixe)
}

// analyserIptables lit « iptables -S INPUT » : ligne « -P INPUT DROP » et
// lignes « -A INPUT ... ».
func analyserIptables(sortie []byte) (politique string, regles int, ok bool) {
	sc := bufio.NewScanner(bytes.NewReader(sortie))
	for sc.Scan() {
		champs := strings.Fields(sc.Text())
		switch {
		case len(champs) >= 3 && champs[0] == "-P" && champs[1] == "INPUT":
			politique, ok = champs[2], true
		case len(champs) >= 2 && champs[0] == "-A" && champs[1] == "INPUT":
			regles++
		}
	}
	return politique, regles, ok
}

var reHookInputDrop = regexp.MustCompile(`(?i)hook\s+input\b.*\bpolicy\s+drop`)

// analyserNft cherche une chaîne accrochée à « input » avec « policy drop » et
// compte les règles des chaînes « input ».
func analyserNft(ruleset []byte) (regles int, drop bool) {
	dansChaine, estInput := false, false
	sc := bufio.NewScanner(bytes.NewReader(ruleset))
	for sc.Scan() {
		ligne := strings.TrimSpace(sc.Text())
		switch {
		case strings.HasPrefix(ligne, "chain "):
			dansChaine, estInput = true, false
		case dansChaine && strings.Contains(ligne, "hook input"):
			estInput = true
			if reHookInputDrop.MatchString(ligne) {
				drop = true
			}
		case dansChaine && ligne == "}":
			dansChaine, estInput = false, false
		case estInput && ligne != "":
			regles++
		}
	}
	return regles, drop
}
