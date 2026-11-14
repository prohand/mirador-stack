// SPDX-License-Identifier: GPL-3.0-or-later

// Package snmp : relevé SNMP v3 des équipements du LAN par le collecteur
// (SPEC § 23.15). Lecture seule (GET et GETBULK), liste d'OID figée dans le
// code par préréglage, mêmes noms de métriques et mêmes étiquettes que
// snmp_exporter (modules synology et if_mib) : les règles importées et le
// tableau « Synology » marchent sans changement (job="snmp").
package snmp

// Valeur : nombre (entier, compteur, jauge) ou texte (métrique « info » :
// étiquette du même nom, valeur 1, comme les DisplayString de snmp_exporter).
type colonne struct {
	sous  string // numéro de colonne dans l'entrée de table
	nom   string
	texte bool
}

type scalaire struct {
	oid   string
	nom   string
	texte bool
}

// table : entrée de table SNMP ; l'index est remplacé par une étiquette lue
// dans une colonne (lookup de snmp_exporter, index retiré), sauf garderIndex.
type table struct {
	entree      string // OID de l'entrée (…Table.1)
	etiquette   string // nom de l'étiquette (diskID, raidName…)
	colEtiq     string // OID complet de la colonne qui donne l'étiquette
	garderIndex string // étiquette de l'index gardée (ifIndex) ; vide : retirée
	colonnes    []colonne
}

type prereglage struct {
	scalaires []scalaire
	tables    []table
}

// Communs à tous les équipements : SNMPv2-MIB, UCD-SNMP-MIB (mémoire, CPU),
// IF-MIB (interfaces).
var (
	scalairesSysteme = []scalaire{
		{"1.3.6.1.2.1.1.3.0", "sysUpTime", false},
		{"1.3.6.1.4.1.2021.4.5.0", "memTotalReal", false},
		{"1.3.6.1.4.1.2021.4.6.0", "memAvailReal", false},
		{"1.3.6.1.4.1.2021.4.14.0", "memBuffer", false},
		{"1.3.6.1.4.1.2021.4.15.0", "memCached", false},
		{"1.3.6.1.4.1.2021.11.11.0", "ssCpuIdle", false},
	}
	tablesInterfaces = []table{
		{entree: "1.3.6.1.2.1.31.1.1.1", etiquette: "ifName", colEtiq: "1.3.6.1.2.1.31.1.1.1.1", garderIndex: "ifIndex",
			colonnes: []colonne{{"6", "ifHCInOctets", false}, {"10", "ifHCOutOctets", false}}},
		{entree: "1.3.6.1.2.1.2.2.1", etiquette: "ifName", colEtiq: "1.3.6.1.2.1.31.1.1.1.1", garderIndex: "ifIndex",
			colonnes: []colonne{{"13", "ifInDiscards", false}, {"14", "ifInErrors", false}, {"19", "ifOutDiscards", false}, {"20", "ifOutErrors", false}}},
	}
)

// prereglages : « standard » (tout équipement SNMP) et « synology »
// (SYNOLOGY-SYSTEM/DISK/RAID/UPS/SERVICES/STORAGEIO-MIB).
var prereglages = map[string]prereglage{
	"standard": {scalaires: scalairesSysteme, tables: tablesInterfaces},
	"synology": {
		scalaires: append(append([]scalaire{}, scalairesSysteme...),
			scalaire{"1.3.6.1.4.1.6574.1.1.0", "systemStatus", false},
			scalaire{"1.3.6.1.4.1.6574.1.2.0", "temperature", false},
			scalaire{"1.3.6.1.4.1.6574.1.3.0", "powerStatus", false},
			scalaire{"1.3.6.1.4.1.6574.1.4.1.0", "systemFanStatus", false},
			scalaire{"1.3.6.1.4.1.6574.1.4.2.0", "cpuFanStatus", false},
			scalaire{"1.3.6.1.4.1.6574.1.5.1.0", "modelName", true},
			scalaire{"1.3.6.1.4.1.6574.1.5.3.0", "version", true},
			scalaire{"1.3.6.1.4.1.6574.1.5.4.0", "upgradeAvailable", false},
			scalaire{"1.3.6.1.4.1.6574.1.7.1.0", "cpuUtilization", false},
			scalaire{"1.3.6.1.4.1.6574.1.7.2.0", "memUtilization", false},
			scalaire{"1.3.6.1.4.1.6574.4.2.1.0", "upsInfoStatus", true},
			scalaire{"1.3.6.1.4.1.6574.4.2.12.1.0", "upsInfoLoadValue", false},
			scalaire{"1.3.6.1.4.1.6574.4.3.1.1.0", "upsBatteryChargeValue", false},
			scalaire{"1.3.6.1.4.1.6574.4.3.6.1.0", "upsBatteryRuntimeValue", false},
		),
		tables: append(append([]table{}, tablesInterfaces...),
			table{entree: "1.3.6.1.4.1.6574.2.1.1", etiquette: "diskID", colEtiq: "1.3.6.1.4.1.6574.2.1.1.2", colonnes: []colonne{
				{"3", "diskModel", true}, {"5", "diskStatus", false}, {"6", "diskTemperature", false}, {"8", "diskRetry", false},
				{"9", "diskBadSector", false}, {"10", "diskIdentifyFail", false}, {"11", "diskRemainLife", false}, {"13", "diskHealthStatus", false}}},
			table{entree: "1.3.6.1.4.1.6574.3.1.1", etiquette: "raidName", colEtiq: "1.3.6.1.4.1.6574.3.1.1.2", colonnes: []colonne{
				{"3", "raidStatus", false}, {"4", "raidFreeSize", false}, {"5", "raidTotalSize", false}, {"6", "raidHotspareCnt", false}}},
			table{entree: "1.3.6.1.4.1.6574.6.1.1", etiquette: "serviceName", colEtiq: "1.3.6.1.4.1.6574.6.1.1.2", colonnes: []colonne{
				{"3", "serviceUsers", false}}},
			table{entree: "1.3.6.1.4.1.6574.101.1.1", etiquette: "storageIODevice", colEtiq: "1.3.6.1.4.1.6574.101.1.1.2", colonnes: []colonne{
				{"3", "storageIONRead", false}, {"4", "storageIONWritten", false}, {"5", "storageIOReads", false}, {"6", "storageIOWrites", false},
				{"8", "storageIOLA", false}, {"12", "storageIONReadX", false}, {"13", "storageIONWrittenX", false}}},
		),
	},
}
