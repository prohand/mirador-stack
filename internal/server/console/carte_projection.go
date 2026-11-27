// SPDX-License-Identifier: GPL-3.0-or-later

package console

import (
	"math"
	"sort"
	"strconv"
	"strings"
)

// Projection Equal Earth de la carte des flux bloqués (SPEC § 137, maquette
// validée le 27/11) : les contours embarqués (carte_monde.go) sont en
// équirectangulaire 1000 × 500 (84° N à 58° S) ; ils sont reprojetés une
// fois au démarrage (pays aux bonnes proportions, bords arrondis), avec la
// même échelle en largeur et en hauteur, dans une zone de 1000 × 520 qui
// laisse la place du titre en haut.

const (
	largeurCarte, hauteurCarte = 1000.0, 520.0
	margeHaut, margeBas        = 64.0, 18.0
	margeCote                  = 10.0
)

// equalEarth : coordonnées projetées (sans échelle) d'une longitude et
// d'une latitude en degrés (Šavrič, Patterson, Jenny 2018).
func equalEarth(lon, lat float64) (float64, float64) {
	const a1, a2, a3, a4 = 1.340264, -0.081106, 0.000893, 0.003796
	m := math.Sqrt(3) / 2
	l, p := lon*math.Pi/180, lat*math.Pi/180
	th := math.Asin(m * math.Sin(p))
	t2 := th * th
	t6 := t2 * t2 * t2
	x := l * math.Cos(th) / (m * (a1 + 3*a2*t2 + t6*(7*a3+9*a4*t2)))
	y := th * (a1 + a2*t2 + t6*(a3+a4*t2))
	return x, y
}

// cadre : échelle et décalage qui font tenir le monde dans la zone.
var cadre = func() (c struct{ x0, y1, k, dx float64 }) {
	x0, x1, y0, y1 := math.Inf(1), math.Inf(-1), math.Inf(1), math.Inf(-1)
	for lat := -58.0; lat <= 84; lat += 1 {
		for _, lon := range []float64{-180, 180, 0} {
			x, y := equalEarth(lon, lat)
			x0, x1, y0, y1 = math.Min(x0, x), math.Max(x1, x), math.Min(y0, y), math.Max(y1, y)
		}
	}
	k := math.Min((largeurCarte-2*margeCote)/(x1-x0), (hauteurCarte-margeHaut-margeBas)/(y1-y0))
	c.x0, c.y1, c.k, c.dx = x0, y1, k, (largeurCarte-k*(x1-x0))/2
	return c
}()

// projeter : point de l'ancienne carte (équirectangulaire 1000 × 500) vers
// la nouvelle.
func projeter(px, py float64) (float64, float64) {
	x, y := equalEarth(px/1000*360-180, 84-py/500*142)
	return cadre.dx + (x-cadre.x0)*cadre.k, margeHaut + (cadre.y1-y)*cadre.k
}

// projeterLonLat : longitude et latitude en degrés vers la nouvelle carte.
func projeterLonLat(lon, lat float64) (float64, float64) {
	return projeter((lon+180)/360*1000, (84-lat)/142*500)
}

// reprojeterChemin : un tracé « M x y L x y … Z » de l'ancienne carte.
func reprojeterChemin(d string) string {
	var b strings.Builder
	champs := strings.FieldsFunc(d, func(r rune) bool { return r == ' ' })
	cmd := byte('M')
	var nums []float64
	ecrire := func() {
		for i := 0; i+1 < len(nums); i += 2 {
			x, y := projeter(nums[i], nums[i+1])
			if i == 0 && cmd == 'M' {
				b.WriteByte('M')
			} else {
				b.WriteByte('L')
			}
			b.WriteString(strconv.FormatFloat(x, 'f', 1, 64) + " " + strconv.FormatFloat(y, 'f', 1, 64))
		}
		nums = nums[:0]
	}
	for _, ch := range champs {
		for len(ch) > 0 {
			switch ch[0] {
			case 'M', 'L', 'Z':
				ecrire()
				if ch[0] == 'Z' {
					b.WriteByte('Z')
				}
				cmd = ch[0]
				ch = ch[1:]
				continue
			}
			j := strings.IndexAny(ch, "MLZ")
			num := ch
			if j >= 0 {
				num, ch = ch[:j], ch[j:]
			} else {
				ch = ""
			}
			if f, err := strconv.ParseFloat(num, 64); err == nil {
				nums = append(nums, f)
			}
		}
	}
	ecrire()
	return b.String()
}

// paysProjetes : contours reprojetés (une fois).
var paysProjetes = func() []paysSVG {
	out := make([]paysSVG, len(paysCarte))
	for i, p := range paysCarte {
		out[i] = paysSVG{Code: p.Code, Nom: p.Nom, D: reprojeterChemin(p.D)}
	}
	return out
}()

// grilleCarte : méridiens tous les 30°, parallèles tous les 20°.
var grilleCarte = func() string {
	var b strings.Builder
	ligne := func(pts [][2]float64) {
		for i, p := range pts {
			x, y := projeterLonLat(p[0], p[1])
			if i == 0 {
				b.WriteByte('M')
			} else {
				b.WriteByte('L')
			}
			b.WriteString(strconv.FormatFloat(x, 'f', 1, 64) + " " + strconv.FormatFloat(y, 'f', 1, 64))
		}
	}
	for lat := -40.0; lat <= 80; lat += 20 {
		var pts [][2]float64
		for lon := -180.0; lon <= 180; lon += 5 {
			pts = append(pts, [2]float64{lon, lat})
		}
		ligne(pts)
	}
	for lon := -180.0; lon <= 180; lon += 30 {
		var pts [][2]float64
		for lat := -58.0; lat <= 84; lat += 4 {
			pts = append(pts, [2]float64{lon, lat})
		}
		ligne(pts)
	}
	return b.String()
}()

// bordCarte : contour du monde (océan).
var bordCarte = func() string {
	var pts [][2]float64
	for lat := 84.0; lat >= -58; lat -= 2 {
		pts = append(pts, [2]float64{-180, lat})
	}
	for lat := -58.0; lat <= 84; lat += 2 {
		pts = append(pts, [2]float64{180, lat})
	}
	var b strings.Builder
	for i, p := range pts {
		x, y := projeterLonLat(p[0], p[1])
		if i == 0 {
			b.WriteByte('M')
		} else {
			b.WriteByte('L')
		}
		b.WriteString(strconv.FormatFloat(x, 'f', 1, 64) + " " + strconv.FormatFloat(y, 'f', 1, 64))
	}
	b.WriteByte('Z')
	return b.String()
}()

// centresPays : point représentatif de chaque pays (médiane des points du
// contour d'origine, corrigée à la main pour les pays dont les territoires
// lointains déplacent la médiane), projeté.
var centresPays = func() map[string][2]float64 {
	m := make(map[string][2]float64, len(paysCarte))
	for _, p := range paysCarte {
		var xs, ys []float64
		for i, champ := range strings.FieldsFunc(p.D, func(r rune) bool { return r == ' ' || r == 'M' || r == 'L' || r == 'Z' }) {
			f, err := strconv.ParseFloat(champ, 64)
			if err != nil {
				continue
			}
			if i%2 == 0 {
				xs = append(xs, f)
			} else {
				ys = append(ys, f)
			}
		}
		if len(xs) == 0 || len(ys) == 0 {
			continue
		}
		sort.Float64s(xs)
		sort.Float64s(ys)
		x, y := projeter(xs[len(xs)/2], ys[len(ys)/2])
		m[p.Code] = [2]float64{math.Round(x*10) / 10, math.Round(y*10) / 10}
	}
	for k, v := range map[string][2]float64{"FR": {507, 120}, "US": {230, 140}, "RU": {700, 70}, "CN": {760, 160}, "NO": {525, 75}, "SG": {786, 248}} {
		x, y := projeter(v[0], v[1])
		m[k] = [2]float64{math.Round(x*10) / 10, math.Round(y*10) / 10}
	}
	return m
}()
