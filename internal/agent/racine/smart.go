// SPDX-License-Identifier: GPL-3.0-or-later

package racine

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
)

// Disque : état SMART d'un disque, réduit à ce que les règles et dashboards
// utilisent (SPEC.md § 4.2).
type Disque struct {
	Nom          string           `json:"nom"` // sda, nvme0n1
	Modele       string           `json:"modele,omitempty"`
	EnVeille     bool             `json:"en_veille,omitempty"` // non interrogé pour ne pas le réveiller
	SanteOK      *bool            `json:"sante_ok,omitempty"`
	Temperature  *float64         `json:"temperature,omitempty"`
	HeuresMarche *float64         `json:"heures_marche,omitempty"`
	ReserveNVMe  *float64         `json:"reserve_nvme,omitempty"` // available_spare (%)
	Attributs    map[string]int64 `json:"attributs,omitempty"`    // valeurs brutes ATA retenues
}

// Attributs ATA gardés (valeur brute). Les dashboards et règles utilisent les
// deux premiers ; les autres signalent aussi un disque qui se dégrade.
var attributsGardes = map[string]bool{
	"Reallocated_Sector_Ct":  true,
	"Current_Pending_Sector": true,
	"Offline_Uncorrectable":  true,
	"Reported_Uncorrect":     true,
	"UDMA_CRC_Error_Count":   true,
}

// maxDisques : garde-fou contre une sortie anormale.
const maxDisques = 64

// Seuls ces types de pilote smartctl sont acceptés pour « -d » : ils viennent
// de la sortie de --scan-open, on les vérifie quand même.
var reTypeSmart = regexp.MustCompile(`^[a-z0-9]+(,[0-9]+)?$`)

// Message de smartctl quand le disque dort (--nocheck=standby).
var reVeille = regexp.MustCompile(`(?i)standby|sleep`)

// Le nom de périphérique doit être un chemin sous /dev.
var reDisque = regexp.MustCompile(`^/dev/[A-Za-z0-9/_-]+$`)

func lireSmart(prog programmes) ([]Disque, error) {
	sortie, err := prog.smartScan()
	if err != nil {
		return nil, err
	}
	var scan struct {
		Devices []struct {
			Name string `json:"name"`
			Type string `json:"type"`
		} `json:"devices"`
	}
	if err := json.Unmarshal(sortie, &scan); err != nil {
		return nil, fmt.Errorf("--scan-open illisible : %w", err)
	}
	var disques []Disque
	var premiereErreur error
	for i, d := range scan.Devices {
		if i >= maxDisques {
			break
		}
		if !reDisque.MatchString(d.Name) || (d.Type != "" && !reTypeSmart.MatchString(d.Type)) {
			continue
		}
		brut, err := prog.smartDisque(d.Name, d.Type)
		if err != nil {
			if premiereErreur == nil {
				premiereErreur = fmt.Errorf("%s : %w", d.Name, err)
			}
			continue
		}
		disque, err := analyserSmart(d.Name, brut)
		if err != nil {
			if premiereErreur == nil {
				premiereErreur = err
			}
			continue
		}
		disques = append(disques, disque)
	}
	return disques, premiereErreur
}

// analyserSmart lit la sortie JSON de « smartctl --json --all ».
func analyserSmart(chemin string, brut []byte) (Disque, error) {
	var s struct {
		ModelName   string `json:"model_name"`
		SmartStatus *struct {
			Passed bool `json:"passed"`
		} `json:"smart_status"`
		Temperature *struct {
			Current float64 `json:"current"`
		} `json:"temperature"`
		PowerOnTime *struct {
			Hours float64 `json:"hours"`
		} `json:"power_on_time"`
		ATA *struct {
			Table []struct {
				Name string `json:"name"`
				Raw  struct {
					Value int64 `json:"value"`
				} `json:"raw"`
			} `json:"table"`
		} `json:"ata_smart_attributes"`
		NVMe *struct {
			AvailableSpare float64 `json:"available_spare"`
		} `json:"nvme_smart_health_information_log"`
		Smartctl struct {
			Messages []struct {
				String string `json:"string"`
			} `json:"messages"`
		} `json:"smartctl"`
	}
	if err := json.Unmarshal(brut, &s); err != nil {
		return Disque{}, fmt.Errorf("%s : JSON smartctl illisible : %w", chemin, err)
	}
	d := Disque{Nom: filepath.Base(chemin), Modele: s.ModelName}
	if s.SmartStatus == nil && s.Temperature == nil && s.ATA == nil && s.NVMe == nil {
		// --nocheck=standby : disque en veille, rien n'a été lu.
		for _, m := range s.Smartctl.Messages {
			if reVeille.MatchString(m.String) {
				d.EnVeille = true
			}
		}
		return d, nil
	}
	if s.SmartStatus != nil {
		ok := s.SmartStatus.Passed
		d.SanteOK = &ok
	}
	if s.Temperature != nil {
		t := s.Temperature.Current
		d.Temperature = &t
	}
	if s.PowerOnTime != nil {
		h := s.PowerOnTime.Hours
		d.HeuresMarche = &h
	}
	if s.NVMe != nil {
		r := s.NVMe.AvailableSpare
		d.ReserveNVMe = &r
	}
	if s.ATA != nil {
		for _, a := range s.ATA.Table {
			if attributsGardes[a.Name] {
				if d.Attributs == nil {
					d.Attributs = make(map[string]int64)
				}
				d.Attributs[a.Name] = a.Raw.Value
			}
		}
	}
	return d, nil
}
