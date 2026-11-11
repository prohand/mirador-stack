#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-3.0-or-later
#
# Génère internal/server/console/carte_monde.go (carte du monde des flux
# bloqués) à partir de Natural Earth 1:110m (domaine public) :
#   curl -L -o pays.geojson https://raw.githubusercontent.com/nvkelso/natural-earth-vector/v5.1.2/geojson/ne_110m_admin_0_countries.geojson
#   python3 -I scripts/generer-carte.py pays.geojson internal/server/console/carte_monde.go
import json, sys
src, dst = sys.argv[1], sys.argv[2]
d = json.load(open(src, encoding='utf-8'))
W, H = 1000, 500
LAT_MAX, LAT_MIN = 84.0, -58.0   # pas d'Antarctique
def proj(lon, lat):
    x = (lon + 180) / 360 * W
    lat = max(min(lat, LAT_MAX), LAT_MIN)
    y = (LAT_MAX - lat) / (LAT_MAX - LAT_MIN) * H
    return round(x, 1), round(y, 1)
def anneau(coords):
    pts, prev = [], None
    for lon, lat in coords:
        p = proj(lon, lat)
        if p != prev:
            pts.append(p); prev = p
    if len(pts) < 3: return ''
    s = 'M' + ' '.join(f'{x:g} {y:g}' if i == 0 else f'{x:g} {y:g}' for i, (x, y) in enumerate(pts[:1]))
    s += 'L' + ' '.join(f'{x:g} {y:g}' for x, y in pts[1:]) + 'Z'
    return s
pays = []
for f in d['features']:
    p = f['properties']
    code = p.get('ISO_A2_EH') or p.get('ISO_A2')
    if not code or code == '-99' or len(code) != 2: continue
    if p.get('CONTINENT') == 'Antarctica': continue
    g = f['geometry']
    polys = g['coordinates'] if g['type'] == 'MultiPolygon' else [g['coordinates']]
    chemin = ''.join(anneau(poly[0]) for poly in polys)  # contour extérieur seul
    if not chemin: continue
    nom = p.get('NAME_FR') or p.get('NAME')
    pays.append((code, nom, chemin))
pays.sort()
with open(dst, 'w', encoding='utf-8') as o:
    o.write('// SPDX-License-Identifier: GPL-3.0-or-later\n\n')
    o.write('// Code généré par scripts/generer-carte.py à partir de Natural Earth\n')
    o.write('// (ne_110m_admin_0_countries, v5.1.2, domaine public) : NE PAS MODIFIER.\n\n')
    o.write('package console\n\n')
    o.write('// paysCarte : contour de chaque pays (projection équirectangulaire,\n')
    o.write('// 1000 × 500, de 84° N à 58° S), code ISO 3166-1 alpha-2 et nom français.\n')
    o.write('var paysCarte = []paysSVG{\n')
    for code, nom, ch in pays:
        o.write(f'\t{{{json.dumps(code)}, {json.dumps(nom, ensure_ascii=False)}, {json.dumps(ch)}}},\n')
    o.write('}\n')
print(len(pays), 'pays')
