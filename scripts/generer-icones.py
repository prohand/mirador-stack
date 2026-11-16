#!/usr/bin/env python3
# SPDX-License-Identifier: GPL-3.0-or-later
"""Icônes PNG de l'application installée (PWA, SPEC § 34.3).

Dessine le logo de mirador (carré bleu arrondi, trait blanc) aux tailles
attendues par Android et iOS, dans internal/server/console/statique/icones/.
À relancer seulement si le logo change :

    python3 scripts/generer-icones.py

Dépendance : Pillow (dessin). Rien n'est téléchargé.
"""
import os
from PIL import Image, ImageDraw

BLEU = (62, 99, 221, 255)   # #3E63DD : bleu du logo (menu)
BLANC = (255, 255, 255, 255)
# Trait du logo, dans un carré de 32 (comme le SVG du menu).
POINTS = [(7, 20), (12.5, 12.5), (17, 18), (20.5, 13.5), (25, 20)]
DOSSIER = os.path.join(os.path.dirname(__file__), "..", "internal", "server", "console", "statique", "icones")


def icone(taille, plein=False, marge=0.0):
    """plein : fond bleu jusqu'aux bords (iOS et icône « masquable » arrondissent
    eux-mêmes) ; marge : part du bord laissée libre autour du trait."""
    k = 4  # dessin 4 fois plus grand, puis réduit : bords lissés
    n = taille * k
    img = Image.new("RGBA", (n, n), BLEU if plein else (0, 0, 0, 0))
    d = ImageDraw.Draw(img)
    if not plein:
        d.rounded_rectangle([0, 0, n - 1, n - 1], radius=round(n * 9 / 32), fill=BLEU)
    e = n * (1 - 2 * marge) / 32          # échelle du trait
    o = n * marge                          # décalage
    pts = [(o + x * e, o + y * e) for x, y in POINTS]
    ep = 2.5 * e
    d.line(pts, fill=BLANC, width=round(ep), joint="curve")
    for x, y in pts:                       # bouts et angles arrondis
        d.ellipse([x - ep / 2, y - ep / 2, x + ep / 2, y + ep / 2], fill=BLANC)
    return img.resize((taille, taille), Image.LANCZOS)


if __name__ == "__main__":
    os.makedirs(DOSSIER, exist_ok=True)
    for nom, img in {
        "icone-192.png": icone(192),
        "icone-512.png": icone(512),
        # « Masquable » (Android découpe en rond ou en carré arrondi) : le
        # trait reste dans la zone sûre (80 % du centre).
        "icone-masquable-512.png": icone(512, plein=True, marge=0.14),
        # iOS : carré plein de 180, sans transparence.
        "icone-apple-180.png": icone(180, plein=True, marge=0.06).convert("RGB"),
    }.items():
        img.save(os.path.join(DOSSIER, nom), optimize=True)
        print(nom, os.path.getsize(os.path.join(DOSSIER, nom)), "octets")
