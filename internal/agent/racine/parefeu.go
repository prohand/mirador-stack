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
}

// Restrictif dit si la politique bloque par défaut.
func (p *PareFeu) Restrictif() bool {
	return p.Politique == "DROP" || p.Politique == "REJECT"
}

// lirePareFeu : iptables d'abord (couvre ufw et iptables-nft), puis nftables
// natif si iptables n'a rien donné de restrictif.
func lirePareFeu(prog programmes) (*PareFeu, error) {
	pf := &PareFeu{Outil: "aucun", Politique: "AUCUNE"}
	sortie, errIpt := prog.iptablesInput()
	if errIpt == nil {
		if pol, n, ok := analyserIptables(sortie); ok {
			pf.Outil, pf.Politique, pf.Regles = "iptables", pol, n
		}
	}
	if !pf.Restrictif() {
		if rs, err := prog.nftRuleset(); err == nil {
			if n, drop := analyserNft(rs); drop {
				pf.Outil, pf.Politique, pf.Regles = "nftables", "DROP", n
			}
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
