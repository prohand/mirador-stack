// SPDX-License-Identifier: GPL-3.0-or-later

//go:build race

package unifi

// avecCourse : tests lancés avec -race (la CI). Le détecteur d'accès
// concurrents gonfle les allocations (environ 2,6 fois pour le décodage
// d'une grande réponse UniFi, mesuré le 22/11).
const avecCourse = true
